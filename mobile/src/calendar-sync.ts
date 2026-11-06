import * as BackgroundTask from 'expo-background-task';
import * as SecureStore from 'expo-secure-store';
import * as TaskManager from 'expo-task-manager';
import { Platform } from 'react-native';
import {
  clearCalendarBackground, createAPI, loadServer, prepareCalendarBackground, serverStorageKey, type PlanoAPI,
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
  title?: string | null;
  startDate: string | Date;
  timeZone?: string;
  allDay?: boolean;
  status?: string;
};

type CalendarFlight = {
  day: string;
  code: string;
  utcTime?: string;
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

function flightKey({ day, code, utcTime }: CalendarFlight) {
  return utcTime ? `${day} ${code} ${utcTime}` : `${day} ${code}`;
}

function flightLabel({ day, code }: CalendarFlight) {
  return `${code} · ${day}`;
}

export function selectCalendarFlights(events: CalendarEvent[], from: Date, until: Date, limit: number, automatic: boolean, includeTimes = true) {
  const candidates = new Map<string, { flight: CalendarFlight; timestamp: number }>();
  const firstDay = dateKey(from);
  const lastDay = dateKey(until);
  for (const event of events) {
    // Expo's iOS legacy bridge uses 'cancelled' for EKEventStatus.canceled.
    if (event.status === 'canceled' || event.status === 'cancelled') continue;
    const title = event.title?.trim() ?? '';
    const match = title.match(codePattern);
    if (!match) continue;
    if (automatic && title !== match[0] && !/\bflight\b/i.test(title) && !routePattern.test(title)) continue;
    const departure = new Date(event.startDate);
    const timestamp = departure.getTime();
    if (!Number.isFinite(timestamp)) continue;
    const code = match[0].replace(/[ -]/g, '');
    const iso = departure.toISOString();
    const absolute = typeof event.startDate !== 'string' || /T.*(?:Z|[+-]\d{2}:?\d{2})$/.test(event.startDate);
    const timed = !event.allDay && absolute;
    if (timed && (departure < from || departure > until)) continue;
    let flight: CalendarFlight;
    if (timed && includeTimes) {
      flight = { day: iso.slice(0, 10), code, utcTime: iso.slice(11, 19).replaceAll(':', '') };
    } else {
      const day = !absolute && typeof event.startDate === 'string' && /^\d{4}-\d{2}-\d{2}(?:T|$)/.test(event.startDate)
        ? event.startDate.slice(0, 10)
        : event.allDay && Platform.OS === 'android' ? iso.slice(0, 10) : calendarDate(departure, event.timeZone);
      if (!timed && (day < firstDay || day > lastDay)) continue;
      flight = { day, code };
    }
    const key = flightKey(flight);
    if (!candidates.has(key)) candidates.set(key, { flight, timestamp });
  }
  return [...candidates.values()].sort((a, b) => a.timestamp - b.timestamp).slice(0, limit).map(({ flight }) => flight);
}

export function calendarImportBatches(flights: CalendarFlight[], size: number) {
  const batches: CalendarFlight[][] = [];
  let batch: CalendarFlight[] = [];
  let codes = new Set<string>();
  for (const flight of flights) {
    // A timed event and a date-only event can resolve to the same flight across UTC midnight.
    if (batch.length === size || codes.has(flight.code)) {
      batches.push(batch);
      batch = [];
      codes = new Set();
    }
    batch.push(flight);
    codes.add(flight.code);
  }
  if (batch.length) batches.push(batch);
  return batches;
}

export function importCalendarBatch(api: PlanoAPI, flights: CalendarFlight[]) {
  const calendar = ['BEGIN:VCALENDAR', 'VERSION:2.0'];
  for (const { day, code, utcTime } of flights) {
    const date = day.replaceAll('-', '');
    const start = utcTime ? `DTSTART:${date}T${utcTime}Z` : `DTSTART;VALUE=DATE:${date}`;
    calendar.push('BEGIN:VEVENT', start, `SUMMARY:${code}`, 'END:VEVENT');
  }
  calendar.push('END:VCALENDAR');
  return api.importFlights('calendar', calendar.join('\r\n'));
}

async function readCalendarFlights(prompt: boolean, includeTimes = true) {
  if (!native) return [];
  const calendar = await import('expo-calendar/legacy');
  const permission = prompt
    ? await calendar.requestCalendarPermissionsAsync()
    : await calendar.getCalendarPermissionsAsync();
  if (!permission.granted) {
    if (prompt) throw new Error('Allow calendar access in device settings to import flight dates.');
    return [];
  }
  const calendars = await calendar.getCalendarsAsync(calendar.EntityTypes.EVENT);
  if (!calendars.length) return [];
  const from = new Date();
  const until = new Date(from);
  until.setDate(until.getDate() + lookaheadDays);
  const queryFrom = new Date(from);
  const queryUntil = new Date(until);
  queryFrom.setHours(0, 0, 0, 0);
  queryUntil.setHours(24, 0, 0, 0);
  if (Platform.OS === 'android') {
    // Android stores all-day events at UTC midnight and filters both endpoints.
    const utcFrom = Date.parse(`${dateKey(from)}T00:00:00Z`);
    const utcUntil = Date.parse(`${dateKey(until)}T00:00:00Z`) + 86_400_000;
    queryFrom.setTime(Math.min(queryFrom.getTime(), utcFrom));
    queryUntil.setTime(Math.max(queryUntil.getTime(), utcUntil));
  }
  const events = await calendar.getEventsAsync(calendars.map(({ id }) => id), queryFrom, queryUntil);
  return selectCalendarFlights(events, from, until, prompt ? 12 : maxCandidates, !prompt, includeTimes);
}

export function calendarFlights() {
  return readCalendarFlights(true);
}

const optInKey = (server: string) => serverStorageKey('calendar-sync', server);
const syncLogKey = (server: string) => serverStorageKey('calendar-imports', server);

async function calendarSyncMode(server: string) {
  if (!native || !server) return undefined;
  const mode = await SecureStore.getItemAsync(optInKey(server));
  if (mode !== 'on' && mode !== 'timed') return undefined;
  return await createAPI(server, { access: 'background' }).hasSavedInstallationToken() ? mode : undefined;
}

export async function calendarSyncEnabled(server: string) {
  return (await calendarSyncMode(server)) !== undefined;
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
    await SecureStore.setItemAsync(optInKey(server), 'timed', secureOptions);
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
          && /^\d{4}-\d{2}-\d{2} [A-Z0-9]{3,8}(?: \d{6})?$/.test(pair));
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

async function activeConsent(server: string, mode: 'on' | 'timed') {
  return (await loadServer({ savedOnly: true })) === server && await calendarSyncMode(server) === mode;
}

async function runSync() {
  if (!native) return;
  const server = await loadServer({ savedOnly: true });
  if (!server) return;
  const mode = await calendarSyncMode(server);
  if (!mode) return;
  const now = Date.now();
  const previous = await readLog(server);
  if (now >= previous.attemptedAt && now - previous.attemptedAt < cadenceMinutes * 60_000) return;
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);
  const recent = dateKey(yesterday);
  const imported = new Set(previous.imported.filter((pair) => pair.slice(0, 10) >= recent));
  // Existing background consent covered flight numbers and dates.
  const candidates = await readCalendarFlights(false, mode === 'timed');
  const groups = calendarImportBatches(candidates.filter((flight) => !imported.has(flightKey(flight))), maxFlightsPerBatch);
  if (!await activeConsent(server, mode)) return;
  if (!groups.length) {
    await storeLog(server, now, 0, imported);
    return;
  }

  const start = previous.nextBatch % groups.length;
  for (let offset = 0; offset < Math.min(maxBatches, groups.length); offset += 1) {
    if (!await activeConsent(server, mode)) return;
    const current = (start + offset) % groups.length;
    const group = groups[current];
    if (!group) continue;
    const next = (current + 1) % groups.length;
    // Move past an unresolved or failed batch on the next run so it cannot
    // prevent new flights from syncing.
    await storeLog(server, now, next, imported);
    const result = await importCalendarBatch(createAPI(server, { access: 'background' }), group);
    const missed = new Set(result.unmatched);
    const matched = group.filter((flight) => !missed.has(flightLabel(flight)));
    // Multiple designators may resolve to one flight. Store a batch only when
    // the reply accounts for every submitted pair.
    if (result.flights.length !== matched.length) continue;
    if (!await activeConsent(server, mode)) return;
    for (const flight of matched) imported.add(flightKey(flight));
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
