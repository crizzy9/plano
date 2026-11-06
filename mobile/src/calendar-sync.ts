import * as BackgroundTask from 'expo-background-task';
import { randomUUID } from 'expo-crypto';
import * as SecureStore from 'expo-secure-store';
import * as TaskManager from 'expo-task-manager';
import { Platform } from 'react-native';
import {
  clearCalendarBackground, createAPI, loadServer, prepareCalendarBackground, serverStorageKey, type CalendarFlight, type PlanoAPI,
} from './api';
import { dateKey } from './flight';

const taskName = 'plano-calendar-sync-v1';
const cadenceMinutes = 30;
export const calendarSyncIntervalMs = cadenceMinutes * 60_000;
const lookaheadDays = 30;
const maxLogEntries = 64;
const legacyCandidateLimit = 64;
const maxBatches = 3;
const maxFlightsPerBatch = 4;
const secureOptions = { keychainAccessible: SecureStore.AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY };
const native = Platform.OS === 'ios' || Platform.OS === 'android';
const codePattern = /\b(?:[A-Z]{2,3}|[A-Z][0-9]|[0-9][A-Z])[ -]?\d{1,4}[A-Z]?\b/;
const routePattern = /\b[A-Z]{3}\b\s*(?:→|[-–]|to)\s*\b[A-Z]{3}\b/i;
const flightKeyPattern = /^\d{4}-\d{2}-\d{2} [A-Z0-9]{3,8}(?: \d{6})?$/;
const orderKeyPattern = /^\d{4}-\d{2}-\d{2} (?:\d{6})? [A-Z0-9]{3,8}$/;
const timedConsentPattern = /^timed:[a-f0-9-]{36}$/;

type CalendarEvent = {
  title?: string | null;
  startDate?: string | Date | null;
  timeZone?: string | null;
  allDay?: boolean;
  status?: string | null;
};

type SyncLog = {
  attemptedAt: number;
  after?: string;
  nextBatch?: number;
  imported: string[];
  consent?: string;
};

let scheduleQueue = Promise.resolve();
let ongoingSync: Promise<void> | undefined;

function calendarDate(departure: Date, timeZone?: string | null) {
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

function calendarOrderKey({ day, code, utcTime }: CalendarFlight) {
  return `${day} ${utcTime ?? ''} ${code}`;
}

function flightLabel({ day, code }: CalendarFlight) {
  return `${code} · ${day}`;
}

export function selectCalendarFlights(events: CalendarEvent[], from: Date, until: Date, limit: number, automatic: boolean, includeTimes = true) {
  const candidates = new Map<string, { flight: CalendarFlight; timestamp: number }>();
  const firstDay = dateKey(from);
  const lastDay = dateKey(until);
  for (const event of events) {
    // The native bridges use different spellings for cancellation.
    if (event.status === 'canceled' || event.status === 'cancelled') continue;
    if (event.startDate == null) continue;
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
  return [...candidates.values()].sort((a, b) => {
    if (!automatic && a.timestamp !== b.timestamp) return a.timestamp - b.timestamp;
    const left = calendarOrderKey(a.flight);
    const right = calendarOrderKey(b.flight);
    return left < right ? -1 : left > right ? 1 : 0;
  }).slice(0, limit).map(({ flight }) => flight);
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
  const calendar = await import('expo-calendar');
  const permission = prompt
    ? await calendar.requestCalendarPermissions()
    : await calendar.getCalendarPermissions();
  if (!permission.granted) {
    if (prompt) throw new Error('Allow calendar access in device settings to import flight dates.');
    return [];
  }
  const calendars = await calendar.getCalendars(calendar.EntityTypes.EVENT);
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
  const events = await calendar.listEvents(calendars, queryFrom, queryUntil);
  return selectCalendarFlights(events, from, until, prompt ? 12 : Infinity, !prompt, includeTimes);
}

export function calendarFlights() {
  return readCalendarFlights(true);
}

const optInKey = (server: string) => serverStorageKey('calendar-sync', server);
const legacyLogKey = (server: string) => serverStorageKey('calendar-imports', server);
const syncLogKey = (server: string) => serverStorageKey('calendar-sync-imports', server);

async function calendarSyncConsent(server: string) {
  if (!native || !server) return undefined;
  const consent = await SecureStore.getItemAsync(optInKey(server));
  if (!consent || consent !== 'on' && consent !== 'timed' && !timedConsentPattern.test(consent)) return undefined;
  return await createAPI(server, { access: 'background' }).hasSavedInstallationToken() ? consent : undefined;
}

export async function calendarSyncEnabled(server: string) {
  return (await calendarSyncConsent(server)) !== undefined;
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
    await SecureStore.deleteItemAsync(legacyLogKey(server));
    await clearCalendarBackground(server);
    await reconcileSchedule();
    return;
  }

  const calendar = await import('expo-calendar');
  const permission = await calendar.requestCalendarPermissions();
  if (!permission.granted) throw new Error('Allow calendar access in device settings to enable sync.');
  await prepareCalendarBackground(server);
  try {
    await SecureStore.setItemAsync(optInKey(server), `timed:${randomUUID()}`, secureOptions);
    await reconcileSchedule();
  } catch (cause) {
    await SecureStore.deleteItemAsync(optInKey(server));
    await clearCalendarBackground(server);
    throw cause;
  }
  void synchronizeCalendar().catch(() => {});
}

async function readLog(key: string, consent: string): Promise<SyncLog> {
  const stored = await SecureStore.getItemAsync(key);
  if (stored) {
    try {
      const log = JSON.parse(stored) as Partial<SyncLog>;
      if (typeof log.attemptedAt === 'number' && Array.isArray(log.imported)
        && (log.consent === undefined || log.consent === consent)) {
        const imported = log.imported.filter((pair) => typeof pair === 'string' && flightKeyPattern.test(pair));
        const nextBatch = Number.isSafeInteger(log.nextBatch) && (log.nextBatch ?? 0) >= 0 ? log.nextBatch ?? 0 : 0;
        const after = typeof log.after === 'string' && orderKeyPattern.test(log.after) ? log.after : undefined;
        return { attemptedAt: log.attemptedAt, nextBatch, after, imported: imported.slice(-maxLogEntries) };
      }
    } catch {
      // A stale or corrupt local log can be rebuilt from the calendar.
    }
  }
  return { attemptedAt: 0, nextBatch: 0, imported: [] };
}

async function storeLog(key: string, consent: string, attemptedAt: number, after: string | undefined, imported: Set<string>, nextBatch?: number) {
  // Calendar titles, event IDs, notes, and booking data never enter this log.
  const log = { consent, attemptedAt, after, nextBatch, imported: [...imported].slice(-maxLogEntries) };
  await SecureStore.setItemAsync(key, JSON.stringify(log), secureOptions);
}

async function activeConsent(server: string, consent: string) {
  return (await loadServer({ savedOnly: true })) === server && await calendarSyncConsent(server) === consent;
}

function pendingCalendarBatches(candidates: CalendarFlight[], imported: Set<string>, otherImports: Set<string>, previous: SyncLog) {
  const pending = candidates.filter((flight) => {
    const key = flightKey(flight);
    return !imported.has(key) && !otherImports.has(key);
  });
  const after = previous.after;
  if (after !== undefined) {
    const first = pending.findIndex((flight) => calendarOrderKey(flight) > after);
    const ordered = first > 0 ? pending.slice(first).concat(pending.slice(0, first)) : pending;
    return calendarImportBatches(ordered, maxFlightsPerBatch);
  }
  const batches = calendarImportBatches(pending, maxFlightsPerBatch);
  if (!batches.length) return batches;
  const first = (previous.nextBatch ?? 0) % batches.length;
  return first > 0 ? batches.slice(first).concat(batches.slice(0, first)) : batches;
}

async function legacyCalendarBatch(api: PlanoAPI, group: CalendarFlight[]) {
  const result = await importCalendarBatch(api, group);
  const missed = new Set(result.unmatched);
  const matched = group.filter((flight) => !missed.has(flightLabel(flight)));
  // An older server deduplicates resolved flights across designators.
  return result.flights.length === matched.length ? matched.map(flightKey) : [];
}

async function runSync() {
  if (!native) return;
  const server = await loadServer({ savedOnly: true });
  if (!server) return;
  const consent = await calendarSyncConsent(server);
  if (!consent) return;
  const now = Date.now();
  const previous = await readLog(syncLogKey(server), consent);
  const previousLegacy = await readLog(legacyLogKey(server), consent);
  const attemptedAt = Math.max(
    previous.attemptedAt <= now ? previous.attemptedAt : 0,
    previousLegacy.attemptedAt <= now ? previousLegacy.attemptedAt : 0,
  );
  if (now - attemptedAt < calendarSyncIntervalMs) return;
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);
  const recent = dateKey(yesterday);
  const imported = new Set(previous.imported.filter((pair) => pair.slice(0, 10) >= recent));
  // Flights imported through older servers have no server receipt to protect
  // them from re-tracking if their local entries are displaced.
  const legacyImported = new Set(previousLegacy.imported.filter((pair) => pair.slice(0, 10) >= recent));
  if (legacyImported.size < previousLegacy.imported.length) {
    if (!await activeConsent(server, consent)) return;
    await storeLog(legacyLogKey(server), consent, previousLegacy.attemptedAt, previousLegacy.after, legacyImported, previousLegacy.nextBatch);
  }
  // Existing background consent covered flight numbers and dates.
  const candidates = await readCalendarFlights(false, consent !== 'on');
  let groups = pendingCalendarBatches(candidates, imported, legacyImported, previous);
  if (!await activeConsent(server, consent)) return;
  if (!groups.length) {
    await storeLog(syncLogKey(server), consent, now, undefined, imported);
    return;
  }

  const calendar = await import('expo-calendar');
  const api = createAPI(server, { access: 'background' });
  let legacy = false;
  for (let sent = 0; sent < maxBatches; ) {
    const group = groups.shift();
    const first = group?.[0];
    if (!group || !first || !await activeConsent(server, consent)) return;
    const last = group.at(-1) ?? first;
    const checkpoint = calendarOrderKey(legacy ? last : first);
    const key = legacy ? legacyLogKey(server) : syncLogKey(server);
    const journal = legacy ? legacyImported : imported;
    // If a provider lookup stalls a batch, give the remaining flights a
    // chance to go first on the next run.
    await storeLog(key, consent, now, checkpoint, journal);
    if (!(await calendar.getCalendarPermissions()).granted || !await activeConsent(server, consent)) return;
    const completed = legacy ? await legacyCalendarBatch(api, group) : await api.syncCalendarFlights(group);
    if (completed === null) {
      legacy = true;
      groups = pendingCalendarBatches(candidates.slice(0, legacyCandidateLimit), legacyImported, imported, previousLegacy);
      continue;
    }
    sent += 1;
    if (!await activeConsent(server, consent)) return;
    const keys = new Set(completed);
    const acknowledged = group.map(flightKey).filter((flight) => keys.has(flight));
    for (const flight of acknowledged) journal.add(flight);
    const after = legacy || acknowledged.length === group.length ? calendarOrderKey(last) : checkpoint;
    await storeLog(key, consent, now, after, journal);
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
