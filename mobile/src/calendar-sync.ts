import * as BackgroundTask from 'expo-background-task';
import * as SecureStore from 'expo-secure-store';
import * as TaskManager from 'expo-task-manager';
import { Platform } from 'react-native';
import {
  clearCalendarBackground, createAPI, loadServer, prepareCalendarBackground, serverStorageKey,
} from './api';
import { dateKey } from './flight';

const taskName = 'plano-calendar-sync-v1';
const cadenceMinutes = 30;
const lookaheadDays = 30;
const maxCandidates = 64;
const maxBatches = 3;
const maxFlightsPerBatch = 4;
const secureOptions = { keychainAccessible: SecureStore.AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY };
const native = Platform.OS === 'ios' || Platform.OS === 'android';
const codePattern = /\b(?:[A-Z]{2,3}|[A-Z][0-9]|[0-9][A-Z])[ -]?\d{1,4}[A-Z]?\b/;
const routePattern = /\b[A-Z]{3}\b\s*(?:→|[-–]|to)\s*\b[A-Z]{3}\b/i;

type CalendarEvent = {
  title: string;
  startDate: string | Date;
  timeZone?: string;
};

type SyncLog = {
  attemptedAt: number;
  nextBatch: number;
  imported: string[];
};

let scheduleQueue = Promise.resolve();
let ongoingSync: Promise<void> | undefined;

function calendarDate(departure: Date, timeZone?: string) {
  if (timeZone) {
    try {
      const parts = new Intl.DateTimeFormat('en-US', {
        timeZone, year: 'numeric', month: '2-digit', day: '2-digit',
      }).formatToParts(departure);
      return ['year', 'month', 'day'].map((type) => parts.find((part) => part.type === type)?.value).join('-');
    } catch {
      // An unknown calendar time zone falls back to the device's local date.
    }
  }
  return dateKey(departure);
}

export function selectCalendarFlights(events: CalendarEvent[], from: Date, until: Date, limit: number, automatic: boolean) {
  const candidates = new Map<string, { day: string; code: string; timestamp: number }>();
  for (const event of events) {
    const title = event.title.trim();
    const match = title.match(codePattern);
    if (!match) continue;
    if (automatic && title !== match[0] && !/\bflight\b/i.test(title) && !routePattern.test(title)) continue;
    const departure = new Date(event.startDate);
    const timestamp = departure.getTime();
    if (!Number.isFinite(timestamp) || departure < from || departure > until) continue;
    const day = calendarDate(departure, event.timeZone);
    const code = match[0].replace(/[ -]/g, '');
    const pair = `${day} ${code}`;
    const existing = candidates.get(pair);
    if (!existing || timestamp < existing.timestamp) candidates.set(pair, { day, code, timestamp });
  }
  const ordered = [...candidates.values()].sort((a, b) => a.timestamp - b.timestamp);
  const byDate = new Map<string, string[]>();
  for (const { day, code } of ordered.slice(0, limit)) {
    const codes = byDate.get(day) ?? [];
    codes.push(code);
    byDate.set(day, codes);
  }
  return byDate;
}

async function readCalendarFlights(prompt: boolean) {
  const byDate = new Map<string, string[]>();
  if (!native) return byDate;
  const calendar = await import('expo-calendar/legacy');
  const permission = prompt
    ? await calendar.requestCalendarPermissionsAsync()
    : await calendar.getCalendarPermissionsAsync();
  if (!permission.granted) {
    if (prompt) throw new Error('Allow calendar access in device settings to import flight dates.');
    return byDate;
  }
  const calendars = await calendar.getCalendarsAsync(calendar.EntityTypes.EVENT);
  if (!calendars.length) return byDate;
  const from = new Date();
  const until = new Date(from);
  until.setDate(until.getDate() + lookaheadDays);
  const events = await calendar.getEventsAsync(calendars.map(({ id }) => id), from, until);
  return selectCalendarFlights(events, from, until, prompt ? 12 : maxCandidates, !prompt);
}

export function calendarFlights() {
  return readCalendarFlights(true);
}

const optInKey = (server: string) => serverStorageKey('calendar-sync', server);
const syncLogKey = (server: string) => serverStorageKey('calendar-imports', server);

export async function calendarSyncEnabled(server: string) {
  if (!native || !server || await SecureStore.getItemAsync(optInKey(server)) !== 'on') return false;
  return createAPI(server, { allowEnrollment: false }).hasSavedInstallationToken();
}

function reconcileSchedule() {
  const next = scheduleQueue.then(async () => {
    if (!native || !await TaskManager.isAvailableAsync()) return;
    const server = await loadServer({ savedOnly: true });
    const enabled = !!server && await calendarSyncEnabled(server);
    const registered = await TaskManager.isTaskRegisteredAsync(taskName);
    if (!enabled && registered) {
      await BackgroundTask.unregisterTaskAsync(taskName);
    } else if (enabled && !registered && await BackgroundTask.getStatusAsync() === BackgroundTask.BackgroundTaskStatus.Available) {
      await BackgroundTask.registerTaskAsync(taskName, { minimumInterval: cadenceMinutes });
    }
  });
  scheduleQueue = next.catch(() => {});
  return next;
}

export async function setCalendarSyncEnabled(server: string, enabled: boolean) {
  if (!native || !server) throw new Error('Connect a Plano server on your device before changing calendar sync.');
  if (!enabled) {
    await SecureStore.deleteItemAsync(optInKey(server));
    await SecureStore.deleteItemAsync(syncLogKey(server));
    await clearCalendarBackground(server);
    await reconcileSchedule();
    return;
  }

  const calendar = await import('expo-calendar/legacy');
  const permission = await calendar.requestCalendarPermissionsAsync();
  if (!permission.granted) throw new Error('Allow calendar access in device settings to enable sync.');
  await prepareCalendarBackground(server);
  try {
    await SecureStore.setItemAsync(optInKey(server), 'on', secureOptions);
    await reconcileSchedule();
  } catch (cause) {
    await SecureStore.deleteItemAsync(optInKey(server));
    await clearCalendarBackground(server);
    throw cause;
  }
  void synchronizeCalendar().catch(() => {});
}

async function readLog(server: string): Promise<SyncLog> {
  const stored = await SecureStore.getItemAsync(syncLogKey(server));
  if (stored) {
    try {
      const log = JSON.parse(stored) as Partial<SyncLog>;
      if (typeof log.attemptedAt === 'number' && Array.isArray(log.imported)) {
        const imported = log.imported.filter((pair) => typeof pair === 'string'
          && /^\d{4}-\d{2}-\d{2} [A-Z0-9]{3,8}$/.test(pair));
        const nextBatch = Number.isSafeInteger(log.nextBatch) && (log.nextBatch ?? 0) >= 0 ? log.nextBatch ?? 0 : 0;
        return { attemptedAt: log.attemptedAt, nextBatch, imported: imported.slice(-maxCandidates) };
      }
    } catch {
      // A stale or corrupt local log can be rebuilt from the calendar.
    }
  }
  return { attemptedAt: 0, nextBatch: 0, imported: [] };
}

async function storeLog(server: string, attemptedAt: number, nextBatch: number, imported: Set<string>) {
  // Calendar titles, event IDs, notes, and booking data never enter this log.
  const log = { attemptedAt, nextBatch, imported: [...imported].slice(-maxCandidates) };
  await SecureStore.setItemAsync(syncLogKey(server), JSON.stringify(log), secureOptions);
}

async function activeConsent(server: string) {
  return (await loadServer({ savedOnly: true })) === server && await calendarSyncEnabled(server);
}

async function runSync() {
  if (!native) return;
  const server = await loadServer({ savedOnly: true });
  if (!server || !await calendarSyncEnabled(server)) return;
  const now = Date.now();
  const previous = await readLog(server);
  if (now >= previous.attemptedAt && now - previous.attemptedAt < cadenceMinutes * 60_000) return;
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);
  const recent = dateKey(yesterday);
  const imported = new Set(previous.imported.filter((pair) => pair.slice(0, 10) >= recent));
  const candidates = await readCalendarFlights(false);
  const groups: { day: string; codes: string[] }[] = [];
  for (const [day, codes] of candidates) {
    const pending = codes.filter((code) => !imported.has(`${day} ${code}`));
    for (let offset = 0; offset < pending.length; offset += maxFlightsPerBatch) {
      groups.push({ day, codes: pending.slice(offset, offset + maxFlightsPerBatch) });
    }
  }
  if (!await activeConsent(server)) return;
  if (!groups.length) {
    await storeLog(server, now, 0, imported);
    return;
  }

  const start = previous.nextBatch % groups.length;
  for (let offset = 0; offset < Math.min(maxBatches, groups.length); offset += 1) {
    if (!await activeConsent(server)) return;
    const current = (start + offset) % groups.length;
    const group = groups[current];
    if (!group) continue;
    const next = (current + 1) % groups.length;
    // Move past an unresolved or failed batch on the next run so it cannot
    // prevent new flights from syncing.
    await storeLog(server, now, next, imported);
    const result = await createAPI(server, { allowEnrollment: false }).importFlights('calendar', group.codes.join('\n'), group.day);
    const missed = new Set(result.unmatched.map((entry) => entry.split(' · ')[0]));
    const matched = group.codes.filter((code) => !missed.has(code));
    // Multiple designators may resolve to one flight. Store a batch only when
    // the reply accounts for every submitted pair.
    if (result.flights.length !== matched.length) continue;
    if (!await activeConsent(server)) return;
    for (const code of matched) imported.add(`${group.day} ${code}`);
    await storeLog(server, now, next, imported);
  }
}

function synchronizeCalendar() {
  ongoingSync ??= runSync().finally(() => { ongoingSync = undefined; });
  return ongoingSync;
}

export async function resumeCalendarSync() {
  await reconcileSchedule().catch(() => {});
  await synchronizeCalendar();
}

// Native headless launches evaluate this module before React mounts.
if (native && !TaskManager.isTaskDefined(taskName)) {
  TaskManager.defineTask(taskName, async ({ error }) => {
    if (error) return BackgroundTask.BackgroundTaskResult.Failed;
    try {
      await synchronizeCalendar();
      return BackgroundTask.BackgroundTaskResult.Success;
    } catch {
      return BackgroundTask.BackgroundTaskResult.Failed;
    }
  });
}
