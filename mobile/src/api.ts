import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';
import {
  canFenceIOSPush, captureLiveProgressSession, clearLiveActivities, disableAndroidPush, retireIOSPush,
  selectAndroidPushOrigin, stopLiveActivityInSession, type RetiredAndroidPush,
} from './live';
import {
  clearSavedFlights, forgetSavedFlight, offlineFlightScope, readSavedFlight, readSavedOverview,
  saveFlightDetail, saveOverview,
} from './offline-flights';

export type FlightStatus = 'scheduled' | 'boarding' | 'taxi_out' | 'airborne'
  | 'landed' | 'arrived' | 'cancelled' | 'diverted' | 'unknown';

export interface Flight {
  id: string;
  number: string;
  origin: string;
  destination: string;
  originICAO?: string;
  destinationICAO?: string;
  scheduledDeparture?: string;
  scheduledArrival?: string;
  originTimeZone?: string;
  destinationTimeZone?: string;
  estimatedDeparture?: string;
  estimatedArrival?: string;
  actualDeparture?: string;
  actualTakeoff?: string;
  actualLanding?: string;
  actualArrival?: string;
  departureGate?: string;
  arrivalGate?: string;
  departureTerminal?: string;
  arrivalTerminal?: string;
  tailNumber?: string;
  inboundId?: string;
  aircraftType?: string;
  aircraftModel?: string;
  aircraftAge?: number;
  aircraftRegistry?: { source: 'transport_canada_ccar' | 'australia_casa'; model?: string; age?: number };
  status: FlightStatus;
  baggage?: string;
  source?: string;
  historyDate?: string;
  observedAt: string;
}

export interface FlightEvent {
  id?: string;
  type?: string;
  kind?: string;
  title?: string;
  body?: string;
  at: string;
  occurredAt?: string;
  observedAt?: string;
}

export interface Position {
  latitude: number;
  longitude: number;
  altitudeFeet?: number;
  groundSpeedKnots?: number;
  heading?: number;
  verticalRateFpm?: number;
  verticalTrend?: 'climbing' | 'descending' | 'level';
  at: string;
  source?: string;
}

export interface RadarBatch {
  positions: Position[];
  cursor?: string;
  receiverRevision?: string;
  providerPositions?: Position[];
  providerCursor?: string;
  providerRevision?: string;
  flightObservedAt?: string;
  swimPositions?: Position[];
  swimCursor?: string;
  swimRevision?: string;
  eurocontrolPositions?: Position[];
  eurocontrolCursor?: string;
  eurocontrolRevision?: string;
}

export interface RadarCursor {
  after?: string;
  revision?: string;
}

export interface Prediction {
  delayMinutes?: number;
  departureDelayMinutes?: number;
  reason?: string;
  explanation?: string;
  reasons?: string[];
  departure?: string;
  method?: string;
  validUntil?: string;
}

export interface HistoricalDepartureRisk {
  probability: number;
  sampleSize: number;
  trainingThrough: string;
  modelVersion: string;
}

export interface AirportWeather {
  airport: string;
  operation: 'departure' | 'arrival';
  conditions: string;
  observedAt: string;
  validUntil: string;
}

export interface AirportWeatherForecast {
  airport: string;
  operation: 'departure' | 'arrival';
  conditions: string;
  issuedAt: string;
  startsAt: string;
  endsAt: string;
  qualifier: 'prevailing' | 'temporary' | 'probability' | 'becoming';
  probability?: number;
}

export interface FlightNotice {
  id: string;
  airport: string;
  operation: 'departure' | 'arrival';
  kind: string;
  source: string;
  summary: string;
  observedAt: string;
  startsAt?: string;
  endsAt?: string;
  validUntil: string;
}

export interface ATFMSlot {
  source: 'faa_swim' | 'eurocontrol_nm';
  takeoffAt: string;
  observedAt: string;
  validUntil: string;
  reason?: string;
}

export interface FlightDetail {
  flight: Flight;
  rotationSnapshot?: boolean;
  events?: FlightEvent[];
  telemetry?: Position[];
  receiverRevision?: string;
  inbound?: Flight[];
  prediction?: Prediction | null;
  historicalDepartureRisk?: HistoricalDepartureRisk;
  weather?: AirportWeather[];
  weatherForecasts?: AirportWeatherForecast[];
  notices?: FlightNotice[];
  atfmSlots?: ATFMSlot[];
}

interface FlightOverview {
  flights: Flight[];
  savedAt?: string;
}

interface FlightScreenData {
  detail: FlightDetail;
  tracked: boolean;
  savedAt?: string;
}

export type FriendAlertMode = 'off' | 'arrival' | 'all';

export interface Friend {
  id: string;
  alias: string;
  direction: 'outgoing' | 'incoming';
  flightIds: string[];
  alertMode?: FriendAlertMode;
}

export interface Friends {
  friends: Friend[];
  flights: { friendId: string; alias: string; flight: Flight }[];
}

export interface FriendInvite {
  code: string;
  expiresAt: string;
}

export interface PickupLink {
  id: string;
  flightId: string;
  createdAt?: string;
  expiresAt: string;
}

export interface PassportSegment {
  origin: string;
  destination: string;
  originLatitude: number;
  originLongitude: number;
  destinationLatitude: number;
  destinationLongitude: number;
  distanceMiles: number;
}

export interface PassportFlight extends Flight {
  historyKey?: string;
  historyDurationMinutes?: number;
  historyArrivalDelayMinutes?: number;
}

export interface ManualPassportFlight {
  date: string;
  origin: string;
  destination: string;
  number?: string;
  tailNumber?: string;
  aircraftModel?: string;
  durationMinutes?: number;
  arrivalDelayMinutes?: number;
}

export interface PassportHistory {
  flights: PassportFlight[];
  nextCursor?: string;
}

interface PassportImportResult {
  added: number;
  duplicates: number;
  skipped: number;
}

export interface Passport extends PassportHistory {
  segments: PassportSegment[];
  stats: { flights: number; miles: number; hours: number; aircraft: number; delayMinutes: number };
  coverage?: {
    missingCoordinatesFlights: number;
    missingDistanceFlights: number;
    missingDurationFlights: number;
    missingDelayFlights: number;
    pendingAirportLookups: number;
  };
}

export interface Connection {
  risk: string;
  guidanceAvailable?: boolean;
  timingAvailable?: boolean;
  availableMinutes: number;
  transferMinutes: number;
  boardingReserveMinutes?: number;
  forecastTransferMinutes?: number;
  airport: string;
  instructions: string[];
  precision?: string;
  mapUrl?: string;
  guideUrl?: string;
  arrivalGate?: string;
  departureGate?: string;
  arrivalGatePrediction?: GatePrediction;
  departureGatePrediction?: GatePrediction;
  monitorEligible?: boolean;
  monitoring?: boolean;
}

export interface ConnectionMonitor {
  inbound: string;
  outbound: string;
  risk: string;
}

export interface ConnectionSuggestion {
  inbound: string;
  outbound: string;
  inboundNumber: string;
  outboundNumber: string;
  airport: string;
  timeZone?: string;
  scheduledArrival: string;
  scheduledDeparture: string;
  scheduledMinutes: number;
  source: 'booking' | 'schedule';
}

export interface GatePrediction {
  gate: string;
  samples: number;
  share: number;
}

export interface ImportResult {
  flights: Flight[];
  unmatched: string[];
  truncated?: boolean;
  nextOffset?: number;
}

export interface TripItSubscriptionStatus {
  available: boolean;
  enabled: boolean;
  nextSyncAt?: string;
  lastSyncAt?: string;
  status?: 'pending' | 'synced' | 'unavailable';
  matched?: number;
  unmatched?: number;
}

export interface PartnerBooking {
  carrier: string;
  locator: string;
  lastName: string;
}

export type ImportSource = 'calendar' | 'email' | 'tripit';

export type CalendarFlight = {
  day: string;
  code: string;
  utcTime?: string;
};

export interface Device {
  platform: 'ios' | 'android';
  token: string;
  kind: 'alert' | 'live_start' | 'live_update';
  flightId?: string;
  binding?: string;
  liveRevision?: number;
  pushClock?: string;
}

const defaultURL = process.env.EXPO_PUBLIC_API_URL || (__DEV__ ? Platform.select({
  android: 'http://10.0.2.2:8080',
  default: 'http://127.0.0.1:8080',
}) : '') || '';
const hostKey = 'plano.server.v1';
const cleanupKey = 'plano.retired-servers.v1';
const androidCleanupKey = 'plano.retired-android-push.v1';
const iosCleanupKey = 'plano.retired-ios-push.v1';
const androidBindingPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const androidDeviceRequests = new Map<string, Promise<void>>();
let drainingAndroidRetirements: Promise<void> | undefined;
let drainingIOSRetirements: Promise<void> | undefined;
let iosRetirementDrainRequested = false;
let iosPushIntent = 0;
const calendarBackgroundStore = {
  keychainService: 'plano.background.v1',
  keychainAccessible: SecureStore.AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY,
};
export const calendarSyncScopes = { consent: 'calendar-sync', log: 'calendar-sync-imports', legacyLog: 'calendar-imports' };
const installationEnrollments = new Map<string, Promise<string>>();
const installationRevisions = new Map<string, number>();
let serverSaveQueue = Promise.resolve();
let retirementQueue = Promise.resolve();
// Only the most recent flight read may update the saved view. This also
// fences requests from a retired API instance after a server switch.
let offlineRevision = 0;

async function retiredServers(): Promise<string[]> {
  return JSON.parse(await SecureStore.getItemAsync(cleanupKey) || '[]') as string[];
}

function serializeRetirement<T>(operation: () => Promise<T>) {
  const next = retirementQueue.then(operation);
  retirementQueue = next.then(() => {}, () => {});
  return next;
}

export function withActiveAndroidPushOrigin<T>(origin: string, operation: () => Promise<T>) {
  return serializeRetirement(async () => {
    if (Platform.OS !== 'android' || await loadServer() !== origin) return undefined;
    return operation();
  });
}

export function withActiveIOSPushOrigin<T>(origin: string, operation: () => Promise<T>) {
  return serializeRetirement(async () => {
    if (Platform.OS !== 'ios' || await loadServer() !== origin) return undefined;
    if (canFenceIOSPush && await SecureStore.getItemAsync(serverStorageKey('flight-alerts', origin)) === 'retiring') {
      await finishIOSDisable(origin);
    }
    return operation();
  });
}

export function beginIOSPushIntent() {
  const intent = ++iosPushIntent;
  return () => iosPushIntent === intent;
}

export function runAndroidDeviceRequest<T>(origin: string, operation: () => Promise<T>) {
  const next = (androidDeviceRequests.get(origin) ?? Promise.resolve()).then(operation);
  const tail = next.then(() => {}, () => {});
  androidDeviceRequests.set(origin, tail);
  void tail.then(() => {
    if (androidDeviceRequests.get(origin) === tail) androidDeviceRequests.delete(origin);
  });
  return next;
}

async function androidRetirements(): Promise<RetiredAndroidPush[]> {
  const pending = await SecureStore.getItemAsync(androidCleanupKey);
  if (!pending) return [];
  try {
    const entries: unknown = JSON.parse(pending);
    if (!Array.isArray(entries)) return [];
    return entries.filter((item): item is RetiredAndroidPush => {
      try {
        return typeof item?.origin === 'string' && normalizeServer(item.origin) === item.origin
          && typeof item.binding === 'string' && androidBindingPattern.test(item.binding);
      } catch {
        return false;
      }
    });
  } catch {
    return [];
  }
}

async function recordAndroidRetirement(retired: RetiredAndroidPush | null) {
  if (!retired) return;
  const pending = await androidRetirements();
  if (pending.some((item) => item.origin === retired.origin && item.binding === retired.binding)) return;
  await SecureStore.setItemAsync(androidCleanupKey, JSON.stringify([...pending, retired].slice(-24)));
}

export async function initializeAndroidPushOrigin(origin: string) {
  await withActiveAndroidPushOrigin(origin, async () => recordAndroidRetirement(await selectAndroidPushOrigin(origin)));
}

export async function disableAndroidPushForOrigin(origin: string) {
  await serializeRetirement(async () => {
    const retired = await disableAndroidPush(origin, null, true);
    await SecureStore.deleteItemAsync(serverStorageKey('flight-alerts', origin));
    await recordAndroidRetirement(retired);
  });
  void removeRetiredDevices(origin).catch(() => {});
}

export function deferAndroidPushRetirement(retired: RetiredAndroidPush | null) {
  if (!retired) return Promise.resolve();
  return serializeRetirement(() => recordAndroidRetirement(retired));
}

async function drainAndroidRetirements() {
  const pending = await androidRetirements();
  for (const item of pending) {
    try {
      await runAndroidDeviceRequest(item.origin, async () => {
        await createAPI(item.origin).revokeDevices(item.binding);
        await serializeRetirement(async () => {
          const entries = await androidRetirements();
          await SecureStore.setItemAsync(androidCleanupKey,
            JSON.stringify(entries.filter(({ origin, binding }) => origin !== item.origin || binding !== item.binding)));
        });
      });
    } catch {
      // The native generation was already cleared. Retry this exact deletion later.
    }
  }
}

type IOSRetirement = { origin: string; token: string; revision: number };

async function iosRetirements(): Promise<IOSRetirement[]> {
  const entries: unknown = JSON.parse(await SecureStore.getItemAsync(iosCleanupKey) || '[]');
  if (!Array.isArray(entries)) throw new Error('Invalid pending iOS alert retirements.');
  return entries.map((item) => {
    if (typeof item?.origin !== 'string' || normalizeServer(item.origin) !== item.origin
      || typeof item.token !== 'string' || !/^[A-Za-z0-9_-]{20,128}$/.test(item.token)
      || !Number.isSafeInteger(item.revision) || item.revision < 1) {
      throw new Error('Invalid pending iOS alert retirement.');
    }
    return { origin: item.origin, token: item.token, revision: item.revision };
  });
}

async function recordIOSRetirement(origin: string, clearUnregistered = false) {
  const token = await SecureStore.getItemAsync(serverStorageKey('installation', origin));
  if (!token) {
    if (clearUnregistered) await clearLiveActivities();
    return;
  }
  const pending = await iosRetirements();
  const revision = await retireIOSPush(origin, token);
  const retired = { origin, token, revision };
  await SecureStore.setItemAsync(iosCleanupKey, JSON.stringify([
    ...pending.filter((item) => item.origin !== origin || item.token !== token), retired,
  ]));
  return retired;
}

async function finishIOSDisable(origin: string) {
  const retired = await recordIOSRetirement(origin, true);
  await SecureStore.deleteItemAsync(serverStorageKey('flight-alerts', origin));
  return retired;
}

export async function disableIOSPushForOrigin(origin: string) {
  const result = await withActiveIOSPushOrigin(origin, async () => {
    // Keep the intent across a process exit between the native cutoff and the
    // durable network request. Foreground recovery completes this local step.
    await SecureStore.setItemAsync(serverStorageKey('flight-alerts', origin), 'retiring');
    return { retired: await finishIOSDisable(origin) };
  });
  if (!result) throw new Error('The active server changed. Try turning off alerts again.');
  if (result.retired && !await sendIOSRetirement(result.retired)) {
    throw new Error('Plano paused new alerts. Existing alerts may continue until your server receives the pending revocation.');
  }
}

async function sendIOSRetirement(item: IOSRetirement) {
  try {
    try {
      await request<void>(item.origin, '/v1/devices', {
        method: 'DELETE', headers: { Authorization: `Bearer ${item.token}` },
        body: JSON.stringify({ platform: 'ios', liveRevision: item.revision }),
      });
    } catch (cause) {
      if (!(cause instanceof APIError) || cause.status !== 401) throw cause;
    }
    await serializeRetirement(async () => {
      const entries = await iosRetirements();
      await SecureStore.setItemAsync(iosCleanupKey, JSON.stringify(entries.filter(({ origin, token, revision }) =>
        origin !== item.origin || token !== item.token || revision > item.revision)));
    });
    return true;
  } catch {
    // Keep this exact cutoff for a future foreground attempt.
    return false;
  }
}

async function drainIOSRetirements() {
  const pending = await serializeRetirement(async () => {
    const current = await loadServer();
    if (current && await SecureStore.getItemAsync(serverStorageKey('flight-alerts', current)) === 'retiring') {
      await finishIOSDisable(current);
    }
    // Upgrade origin-only cleanup from earlier builds before sending it.
    const legacy = await retiredServers();
    if (legacy.length > 0) {
      for (const origin of legacy) {
        if (origin !== current && normalizeServer(origin) === origin) await recordIOSRetirement(origin);
      }
      await SecureStore.deleteItemAsync(cleanupKey);
    }
    return iosRetirements();
  });
  for (const item of pending) await sendIOSRetirement(item);
}

export function removeRetiredDevices(activeServer: string) {
  if (Platform.OS === 'web' || !activeServer) return Promise.resolve();
  if (Platform.OS === 'android') {
    drainingAndroidRetirements ??= drainAndroidRetirements().finally(() => { drainingAndroidRetirements = undefined; });
    return drainingAndroidRetirements;
  }
  if (canFenceIOSPush) {
    iosRetirementDrainRequested = true;
    drainingIOSRetirements ??= (async () => {
      while (iosRetirementDrainRequested) {
        iosRetirementDrainRequested = false;
        await drainIOSRetirements();
      }
    })().finally(() => { drainingIOSRetirements = undefined; });
    return drainingIOSRetirements;
  }
  return serializeRetirement(async () => {
    let pending = await retiredServers();
    for (const server of [...pending]) {
      try {
        if (server !== activeServer) await createAPI(server).revokeDevices();
        pending = pending.filter((item) => item !== server);
        await SecureStore.setItemAsync(cleanupKey, JSON.stringify(pending));
      } catch {
        // Keep unreachable origins for the next foreground session.
      }
    }
  });
}

export function normalizeServer(value: string) {
  // React Native's URL implementation does not reliably canonicalize origins,
  // especially IPv6 hosts and explicit default ports.
  const match = value.trim().match(/^(https?):\/\/(\[[0-9a-f:.]+\]|[a-z0-9.-]+)(?::(\d{1,5}))?\/?$/i);
  const protocol = match?.[1]?.toLowerCase();
  const host = match?.[2]?.toLowerCase();
  const port = match?.[3] ? Number(match[3]) : undefined;
  if (!protocol || !host || (port !== undefined && (port < 1 || port > 65535))
    || (!host.startsWith('[') && !host.split('.').every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)))) {
    throw new Error('Enter the complete server origin without a path, query, or credentials.');
  }
  const local = ['127.0.0.1', '[::1]', 'localhost', '10.0.2.2'].includes(host);
  if (protocol !== 'https' && !(__DEV__ && local && protocol === 'http')) {
    throw new Error('Use an HTTPS address for your Plano server.');
  }
  const customPort = port && port !== (protocol === 'https' ? 443 : 80);
  return `${protocol}://${host}${customPort ? `:${port}` : ''}`;
}

export async function loadServer({ savedOnly = false } = {}): Promise<string> {
  if (savedOnly) {
    if (Platform.OS === 'web') return '';
    const saved = await SecureStore.getItemAsync(hostKey, calendarBackgroundStore);
    return saved ? normalizeServer(saved) : '';
  }
  const saved = Platform.OS === 'web'
    ? globalThis.localStorage?.getItem(hostKey)
    : await SecureStore.getItemAsync(hostKey);
  if (!saved && !defaultURL) return '';
  return normalizeServer(saved || defaultURL);
}

export async function prepareCalendarBackground(server: string, expectedToken: string) {
  await serializeRetirement(async () => {
    if (Platform.OS === 'web' || await loadServer() !== server) {
      throw new Error('Choose a Plano server before enabling background calendar sync.');
    }
    const key = serverStorageKey('installation', server);
    const token = await SecureStore.getItemAsync(key);
    if (token !== expectedToken) throw new Error('The Plano installation changed. Try enabling calendar sync again.');
    await SecureStore.setItemAsync(key, token, calendarBackgroundStore);
    try {
      await SecureStore.setItemAsync(hostKey, server, calendarBackgroundStore);
    } catch (cause) {
      await SecureStore.deleteItemAsync(key, calendarBackgroundStore);
      throw cause;
    }
  });
}

async function removeCalendarBackground(server: string) {
  const active = await SecureStore.getItemAsync(hostKey, calendarBackgroundStore);
  if (active === server) await SecureStore.deleteItemAsync(hostKey, calendarBackgroundStore);
  await SecureStore.deleteItemAsync(serverStorageKey('installation', server), calendarBackgroundStore);
}

export async function clearCalendarBackground(server: string) {
  if (Platform.OS !== 'web') await serializeRetirement(() => removeCalendarBackground(server));
}

export type ServerConnection = { url: string; replacedInstallation: boolean };

export async function saveServer(value: string, enrollmentCode?: string, previous?: string): Promise<ServerConnection> {
  const url = normalizeServer(value);
  const health = await request<{ status?: string; mode?: string }>(url, '/healthz');
  if (health?.status !== 'ok' || !['demo', 'live'].includes(health.mode ?? '')) {
    throw new Error('This address did not return a Plano health response.');
  }
  // Serialize explicit selections while keeping network probes outside the
  // native retirement queue. Existing-credential reads never wait for this queue.
  const selection = serverSaveQueue.then(async () => {
    const current = await loadServer();
    if (previous !== undefined && current !== previous && current !== url) {
      throw new Error('The active Plano server changed. Try connecting again.');
    }
    const existing = await savedInstallationToken(url);
    let replacement: string | undefined;
    if (existing) {
      try {
        const probe = await request<{ flights: Flight[] }>(url, '/v1/tracked?flightId=plano-installation-check', {
          headers: { Authorization: `Bearer ${existing}` },
        });
        if (!Array.isArray(probe?.flights)) throw new Error('Plano returned an invalid installation check.');
      } catch (cause) {
        if (!(cause instanceof APIError) || cause.status !== 401) throw cause;
        replacement = await enrollInstallation(url, enrollmentCode);
      }
    } else {
      await foregroundInstallationToken(url, enrollmentCode);
    }

    const result = { url, replacedInstallation: replacement !== undefined };
    if (Platform.OS === 'web') {
      if (replacement) await replaceInstallationToken(url, replacement);
      globalThis.localStorage?.setItem(hostKey, url);
      return result;
    }
    await serializeRetirement(async () => {
      if (replacement && await savedInstallationToken(url) !== existing) {
        throw new Error('The Plano installation changed. Try connecting again.');
      }
      if (Platform.OS === 'android') await recordAndroidRetirement(await selectAndroidPushOrigin(url));
      if (Platform.OS === 'ios' && current && current !== url) {
        beginIOSPushIntent();
        if (!canFenceIOSPush) await clearLiveActivities();
        else if (await SecureStore.getItemAsync(serverStorageKey('flight-alerts', current)) === 'retiring') await finishIOSDisable(current);
        else await recordIOSRetirement(current, true);
      }
      const previousBackground = await loadServer({ savedOnly: true });
      if (previousBackground && previousBackground !== url) await removeCalendarBackground(previousBackground);
      if (Platform.OS !== 'android' && !canFenceIOSPush) {
        const pending = await retiredServers();
        if (previous && previous !== url && !pending.includes(previous)) pending.push(previous);
        await SecureStore.setItemAsync(cleanupKey, JSON.stringify(pending.filter((item) => item !== url)));
      }
      if (current === url) await SecureStore.setItemAsync(hostKey, url);
      if (existing && replacement) {
        const preference = serverStorageKey('flight-alerts', url);
        if (Platform.OS === 'android') {
          await recordAndroidRetirement(await disableAndroidPush(url, null, true));
          await SecureStore.deleteItemAsync(preference);
        } else if (Platform.OS === 'ios') {
          beginIOSPushIntent();
          if (canFenceIOSPush) await finishIOSDisable(url);
          else {
            if (current === url) await clearLiveActivities();
            await SecureStore.deleteItemAsync(preference);
          }
        }
        await removeCalendarBackground(url);
        for (const scope of Object.values(calendarSyncScopes)) {
          await SecureStore.deleteItemAsync(serverStorageKey(scope, url));
        }
        await replaceInstallationToken(url, replacement);
        await clearInstallationCache(url, existing);
      }
      if (current !== url) await SecureStore.setItemAsync(hostKey, url);
      if (current && current !== url) await clearInstallationCache(current);
    });
    return result;
  });
  serverSaveQueue = selection.then(() => {}, () => {});
  return selection;
}

class APIError extends Error {
  constructor(readonly status: number, message: string, readonly routeMissing = false) {
    super(message);
  }
}

class ConnectionError extends Error {}

const deviceCapabilitiesPath = '/v1/devices/capabilities';

class RequestAbortedError extends Error {
  constructor() {
    super('Plano request was canceled.');
    this.name = 'AbortError';
  }
}

function checkRequestSignal(signal?: AbortSignal | null) {
  if (signal?.aborted) throw new RequestAbortedError();
}

// A read can leave while shared token acquisition or a storage check finishes.
async function waitForRequest<T>(pending: Promise<T>, signal?: AbortSignal | null): Promise<T> {
  if (!signal) return pending;
  let abort = () => {};
  const canceled = new Promise<never>((_, reject) => {
    abort = () => reject(new RequestAbortedError());
    signal.addEventListener('abort', abort);
    if (signal.aborted) abort();
  });
  try {
    const result = await Promise.race([pending, canceled]);
    checkRequestSignal(signal);
    return result;
  } finally {
    signal.removeEventListener('abort', abort);
  }
}

function canReadOffline(error: unknown) {
  return error instanceof ConnectionError || error instanceof APIError && error.status >= 500;
}

async function request<T>(server: string, path: string, init: RequestInit = {}, timeoutMs = 12_000): Promise<T> {
  const { signal } = init;
  checkRequestSignal(signal);
  if (!server) throw new Error('Connect to a Plano server in Settings.');
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener('abort', abort);
  const timeout = setTimeout(abort, timeoutMs);
  let raw: string;
  try {
    const response = await fetch(server + path, {
      ...init,
      headers: { Accept: 'application/json', ...(init.body ? { 'Content-Type': 'application/json' } : {}), ...init.headers },
      signal: controller.signal,
      redirect: 'error',
    });
    checkRequestSignal(controller.signal);
    if (!response.ok) {
      if (response.status === 401) throw new APIError(401, 'This installation needs to reconnect.');
      if (response.status === 403) throw new APIError(403, 'This server requires an enrollment code. Enter it in Settings.');
      if (response.status === 404) {
        const type = response.headers.get('Content-Type')?.split(';', 1)[0]?.trim().toLowerCase();
        const routeMissing = type === 'text/plain' && (path !== deviceCapabilitiesPath || await response.text() === '404 page not found\n');
        checkRequestSignal(controller.signal);
        throw new APIError(404, 'That information is unavailable on this server.', routeMissing);
      }
      if (response.status === 429) throw new APIError(429, 'Too many requests. Try again in a moment.');
      if (response.status >= 500) throw new APIError(response.status, 'Plano’s server is temporarily unavailable.');
      throw new APIError(response.status, 'Plano couldn’t complete that request.');
    }
    if (path === deviceCapabilitiesPath && (response.status !== 200 ||
        response.headers.get('Content-Type')?.split(';', 1)[0]?.trim().toLowerCase() !== 'application/json')) {
      throw new APIError(response.status, 'Plano returned invalid device capabilities.');
    }
    raw = await response.text();
    checkRequestSignal(controller.signal);
  } catch (error) {
    checkRequestSignal(signal);
    if (error instanceof APIError) throw error;
    throw new ConnectionError('Couldn’t reach Plano. Check the server address and your connection.');
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener('abort', abort);
  }
  try {
    return (raw ? JSON.parse(raw) : undefined) as T;
  } catch {
    throw new Error('Plano returned an invalid response.');
  }
}

export function serverStorageKey(scope: string, server: string) {
  return `plano.${scope}.` + Array.from(server)
    .map((character) => character.codePointAt(0)?.toString(16).padStart(2, '0')).join('');
}

async function savedInstallationToken(server: string, background = false) {
  const key = serverStorageKey('installation', server);
  return Platform.OS === 'web'
    ? globalThis.sessionStorage?.getItem(key)
    : SecureStore.getItemAsync(key, background ? calendarBackgroundStore : undefined);
}

async function storeInstallationToken(server: string, token: string) {
  const key = serverStorageKey('installation', server);
  if (Platform.OS === 'web') globalThis.sessionStorage?.setItem(key, token);
  else await SecureStore.setItemAsync(key, token);
}

async function replaceInstallationToken(server: string, token: string) {
  await storeInstallationToken(server, token);
  installationEnrollments.delete(server);
  installationRevisions.set(server, (installationRevisions.get(server) ?? 0) + 1);
}

async function enrollInstallation(server: string, enrollmentCode?: string) {
  const enrollment = await request<{ token?: string }>(server, '/v1/installations', {
    method: 'POST', body: JSON.stringify({ enrollmentCode: enrollmentCode || undefined }),
  });
  if (typeof enrollment?.token !== 'string' || !enrollment.token) {
    throw new Error('Plano couldn’t register this installation.');
  }
  return enrollment.token;
}

async function foregroundInstallationToken(server: string, enrollmentCode?: string) {
  const pending = installationEnrollments.get(server);
  if (pending) {
    try {
      return await pending;
    } catch (cause) {
      // A foreground read may have started an enrollment without the code
      // that the user has since supplied in Settings.
      if (!enrollmentCode || !(cause instanceof APIError) || cause.status !== 403) throw cause;
      return foregroundInstallationToken(server, enrollmentCode);
    }
  }
  const enrollment = (async () => {
    const saved = await savedInstallationToken(server);
    if (saved) return saved;
    const token = await enrollInstallation(server, enrollmentCode);
    await storeInstallationToken(server, token);
    return token;
  })().finally(() => {
    if (installationEnrollments.get(server) === enrollment) installationEnrollments.delete(server);
  });
  installationEnrollments.set(server, enrollment);
  return enrollment;
}

async function clearInstallationCache(server: string, oldToken?: string) {
  offlineRevision += 1;
  try {
    const token = oldToken ?? await savedInstallationToken(server);
    const scope = token && await offlineFlightScope(server, token);
    if (scope) clearSavedFlights(scope);
  } catch {
    // Server selection works even when the device cache is unavailable.
  }
}

const flightPath = (id: string) => `/v1/flights/${encodeURIComponent(id)}`;
const friendPath = (id: string) => `/v1/friends/${encodeURIComponent(id)}`;
const passportPath = '/v1/passport?limit=50';
const passportManualPath = '/v1/passport/manual';

type APIAccessOptions =
  | { access: 'background'; withRequestAuthorization?: <T>(send: () => Promise<T>) => Promise<T> }
  | { access?: 'existing-installation'; withRequestAuthorization?: never };

export function createAPI(server: string, options: APIAccessOptions = {}) {
  // The canonical origin is part of the key, so two installations on different
  // hosts can never use each other's bearer token.
  const background = options.access === 'background';
  const savedToken = () => savedInstallationToken(server, background);
  let tokenRevision: number | undefined;
  let tokenPromise: Promise<string> | undefined;
  let iosPushClockSupport: Promise<boolean> | undefined;

  function supportsIOSPushClock() {
    if (iosPushClockSupport) return iosPushClockSupport;
    const probe = request<{ iosPushClock?: unknown }>(server, deviceCapabilitiesPath).then((capabilities) => {
      if (capabilities?.iosPushClock !== true) throw new Error('Plano returned invalid device capabilities.');
      return true;
    }, (cause: unknown) => {
      if (cause instanceof APIError && cause.status === 404 && cause.routeMissing) return false;
      throw cause;
    });
    iosPushClockSupport = probe;
    // Cache support, and probe again later after an outage or a server upgrade.
    const forget = () => { iosPushClockSupport = undefined; };
    void probe.then((supported) => { if (!supported) forget(); }, forget);
    return probe;
  }

  async function installationToken(enrollmentCode?: string): Promise<string> {
    if (!options.access) {
      const revision = installationRevisions.get(server) ?? 0;
      if (!tokenPromise || tokenRevision !== revision) {
        const pending = foregroundInstallationToken(server, enrollmentCode);
        tokenPromise = pending;
        tokenRevision = revision;
        void pending.catch(() => { if (tokenPromise === pending) tokenPromise = undefined; });
      }
      return tokenPromise;
    }
    const existing = await savedToken();
    if (!existing) throw new Error('This server has no saved Plano installation.');
    return existing;
  }

  function readInstallationToken(signal?: AbortSignal | null) {
    checkRequestSignal(signal);
    return waitForRequest(installationToken(), signal);
  }

  async function authenticated<T>(path: string, init: RequestInit = {}, timeoutMs?: number) {
    const token = await readInstallationToken(init.signal);
    return requestAs<T>(token, path, init, timeoutMs);
  }

  async function requestAs<T>(token: string, path: string, init: RequestInit = {}, timeoutMs?: number) {
    const send = () => request<T>(server, path, { ...init, headers: { ...init.headers, Authorization: `Bearer ${token}` } }, timeoutMs);
    const authorize = options.withRequestAuthorization;
    if (authorize) {
      const { response } = await serializeRetirement(async () => {
        if (await loadServer({ savedOnly: true }) !== server || await savedToken() !== token) {
          throw new Error('Access for this server ended.');
        }
        // Keep the saved host stable through the final consent check and
        // fetch invocation. The response settles outside the queue.
        return authorize(async () => ({ response: send() }));
      });
      return response;
    }
    if (options.access && (await loadServer({ savedOnly: background }) !== server || await savedToken() !== token)) {
      throw new Error('Access for this server ended.');
    }
    return send();
  }

  async function withFlightCache<T>(token: string, revision: number | undefined, use: (scope: string) => T,
    signal?: AbortSignal): Promise<T | undefined> {
    checkRequestSignal(signal);
    if (options.access || Platform.OS === 'web') return;
    async function useCurrentCache() {
      const scope = await offlineFlightScope(server, token);
      checkRequestSignal(signal);
      if (!scope) return;
      return serializeRetirement(async () => {
        checkRequestSignal(signal);
        if (await loadServer() !== server) return;
        checkRequestSignal(signal);
        if (await savedToken() !== token) return;
        checkRequestSignal(signal);
        if (revision !== undefined && offlineRevision !== revision) return;
        return use(scope);
      });
    }
    try {
      return await waitForRequest(useCurrentCache(), signal);
    } catch {
      checkRequestSignal(signal);
      // A cache error does not affect the live response.
    }
  }

  async function clearRejectedCache(error: unknown, token: string, signal?: AbortSignal) {
    if (error instanceof APIError && (error.status === 401 || error.status === 403)) {
      await withFlightCache(token, undefined, (scope) => {
        offlineRevision += 1;
        clearSavedFlights(scope);
      }, signal);
    }
  }

  async function trackedAs(token: string, flightIds: readonly string[], signal?: AbortSignal): Promise<Flight[]> {
    const query = new URLSearchParams();
    for (const id of flightIds) query.append('flightId', id);
    const path = flightIds.length ? `/v1/tracked?${query}` : '/v1/tracked';
    return (await requestAs<{ flights: Flight[] }>(token, path, { signal })).flights;
  }

  function detailAs(token: string, id: string, rotationOf?: string, signal?: AbortSignal) {
    const query = rotationOf ? `?${new URLSearchParams({ rotationOf })}` : '';
    return requestAs<FlightDetail>(token, `${flightPath(id)}${query}`, { signal });
  }

  return {
    server,
    hasSavedInstallationToken: async () => Boolean(await savedToken()),
    installationToken,
    async search(number: string, date: string): Promise<Flight[]> {
      const query = new URLSearchParams({ number, date });
      return (await authenticated<{ flights: Flight[] }>(`/v1/flights?${query}`)).flights;
    },
    async tracked(flightIds: readonly string[] = [], signal?: AbortSignal) {
      return trackedAs(await readInstallationToken(signal), flightIds, signal);
    },
    async detail(id: string, rotationOf?: string, signal?: AbortSignal) {
      return detailAs(await readInstallationToken(signal), id, rotationOf, signal);
    },
    async flightOverview(signal?: AbortSignal): Promise<FlightOverview> {
      const token = await readInstallationToken(signal);
      checkRequestSignal(signal);
      const revision = ++offlineRevision;
      try {
        const flights = await trackedAs(token, [], signal);
        await withFlightCache(token, revision, (scope) => saveOverview(scope, flights), signal);
        return { flights };
      } catch (error) {
        checkRequestSignal(signal);
        if (canReadOffline(error)) {
          const saved = await withFlightCache(token, revision, readSavedOverview, signal);
          if (saved) return saved;
        }
        await clearRejectedCache(error, token, signal);
        throw error;
      }
    },
    async flightScreen(id: string, rotationOf?: string, signal?: AbortSignal): Promise<FlightScreenData> {
      const token = await readInstallationToken(signal);
      checkRequestSignal(signal);
      const revision = ++offlineRevision;
      const [snapshot, selection] = await Promise.allSettled([detailAs(token, id, rotationOf, signal), trackedAs(token, [id], signal)]);
      checkRequestSignal(signal);
      if (snapshot.status === 'fulfilled' && selection.status === 'fulfilled') {
        const flight = snapshot.value;
        const isTracked = !flight.rotationSnapshot && selection.value.some((item) => item.id === id);
        if (!rotationOf) {
          await withFlightCache(token, revision, (scope) => {
            if (isTracked && flight.flight.id === id) saveFlightDetail(scope, flight);
            else if (!isTracked) forgetSavedFlight(scope, id);
          }, signal);
        }
        return { detail: flight, tracked: isTracked };
      }
      const failures = [snapshot, selection].flatMap((result) => result.status === 'rejected' ? [result.reason as unknown] : []);
      const rejection = failures.find((error) => error instanceof APIError && (error.status === 401 || error.status === 403))
        ?? failures.find((error) => !canReadOffline(error));
      if (rejection !== undefined) {
        await clearRejectedCache(rejection, token, signal);
        if (!rotationOf && rejection instanceof APIError && rejection.status === 404) {
          await withFlightCache(token, revision, (scope) => forgetSavedFlight(scope, id), signal);
        }
        throw rejection;
      }
      if (!rotationOf) {
        if (selection.status === 'fulfilled' && !selection.value.some((flight) => flight.id === id)) {
          await withFlightCache(token, revision, (scope) => forgetSavedFlight(scope, id), signal);
        } else {
          const saved = await withFlightCache(token, revision, (scope) => readSavedFlight(scope, id), signal);
          if (saved) return { ...saved, tracked: true };
        }
      }
      throw failures[0];
    },
    async radar(id: string, after?: string, providerRevision?: string, receiverRevision?: string,
      swim?: RadarCursor, eurocontrol?: RadarCursor, signal?: AbortSignal) {
      const query = new URLSearchParams({ provider: '1', swim: '1', eurocontrol: '1', snapshot: '1' });
      if (after) query.set('after', after);
      if (receiverRevision !== undefined) query.set('receiverRevision', receiverRevision);
      if (providerRevision) query.set('providerRevision', providerRevision);
      if (swim?.after) query.set('swimAfter', swim.after);
      if (swim?.revision !== undefined) query.set('swimRevision', swim.revision);
      if (eurocontrol?.after) query.set('eurocontrolAfter', eurocontrol.after);
      if (eurocontrol?.revision !== undefined) query.set('eurocontrolRevision', eurocontrol.revision);
      try {
        return await authenticated<RadarBatch>(`${flightPath(id)}/radar?${query}`, { signal });
      } catch (error) {
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) return undefined;
        throw error;
      }
    },
    async track(flightId: string, seat?: string) {
      await authenticated<void>('/v1/tracked', { method: 'POST', body: JSON.stringify({ flightId, seat }) });
      offlineRevision += 1;
    },
    async untrack(id: string) {
      const withActivePushOrigin = Platform.OS === 'android' ? withActiveAndroidPushOrigin : withActiveIOSPushOrigin;
      // The DELETE can outlive a server switch or an alert session. Capture
      // the old identity without holding the retirement lock during the request.
      const token = await installationToken();
      const binding = await withActivePushOrigin(server, async () => {
        if (await savedToken() !== token) return null;
        return captureLiveProgressSession(server);
      }).catch(() => null);
      await requestAs<void>(token, `/v1/tracked/${encodeURIComponent(id)}`, { method: 'DELETE' });
      offlineRevision += 1;
      await withFlightCache(token, undefined, (scope) => forgetSavedFlight(scope, id));
      if (binding) await withActivePushOrigin(server, () => stopLiveActivityInSession(server, binding, id)).catch(() => {});
    },
    share: (flightId: string) => authenticated<{ url: string; expiresAt: string; id?: string; createdAt?: string }>('/v1/shares', {
      method: 'POST', body: JSON.stringify({ flightId }),
    }),
    pickupLinks: (flightId: string) =>
      authenticated<{ links: PickupLink[] }>(`/v1/shares?${new URLSearchParams({ flightId })}`),
    async revokePickupLink(id: string) {
      try {
        await authenticated<void>(`/v1/shares/${encodeURIComponent(id)}`, { method: 'DELETE' });
      } catch (error) {
        if (!(error instanceof APIError && error.status === 404 && !error.routeMissing)) throw error;
      }
    },
    passport: () => authenticated<Passport>(passportPath),
    passportHistory: (cursor: string) => authenticated<PassportHistory>(`${passportPath}&${new URLSearchParams({ cursor })}`),
    async addManualPassportFlight(details: ManualPassportFlight, requestId: string) {
      try {
        const result = await authenticated<{ created: boolean }>(passportManualPath, {
          method: 'POST', body: JSON.stringify({ ...details, requestId }),
        });
        if (typeof result?.created !== 'boolean') throw new Error('Plano returned an invalid response.');
        return result;
      } catch (error) {
        if (error instanceof APIError) {
          if ([404, 405, 501].includes(error.status)) throw new Error('Manual flight logging requires a newer Plano server.');
          if (error.status === 409) throw new Error('Passport can store up to 10,000 logged and imported flights.');
          if ([400, 422].includes(error.status)) throw new Error('Check the flight date, airport codes, and optional details.');
        }
        throw error;
      }
    },
    async updateManualPassportFlight(id: string, details: ManualPassportFlight, expectedObservedAt: string) {
      try {
        await authenticated<void>(`${passportManualPath}/${encodeURIComponent(id)}`, {
          method: 'PUT', body: JSON.stringify({ ...details, expectedObservedAt }),
        });
      } catch (error) {
        if (error instanceof APIError) {
          if (error.status === 409) throw new Error('This flight changed after you opened it. Reopen it in Passport to edit the latest version.');
          if (error.status === 404) throw new Error('This flight is unavailable. Return to Passport to refresh your history.');
          if ([400, 422].includes(error.status)) throw new Error('Check the flight date, airport codes, and optional details.');
        }
        throw error;
      }
    },
    async importPassportCSV(csv: string) {
      try {
        return await authenticated<PassportImportResult>('/v1/passport/import', {
          method: 'POST', body: csv, headers: { 'Content-Type': 'text/csv' },
        });
      } catch (error) {
        if (error instanceof APIError) {
          if ([404, 405, 501].includes(error.status)) throw new Error('This Plano server does not support Flighty CSV imports.');
          if (error.status === 409) throw new Error('Passport can store up to 10,000 imported flights.');
          if (error.status === 413) throw new Error('The CSV must be 512 KiB or smaller.');
          if ([400, 415, 422].includes(error.status)) throw new Error('Choose a valid Flighty CSV export with up to 1,000 rows.');
        }
        throw error;
      }
    },
    removePassportArchive: (id: string) =>
      authenticated<void>(`/v1/passport/archive/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    friends: (signal?: AbortSignal) => authenticated<Friends>('/v1/friends', { signal }),
    createFriendInvite: (alias: string) => authenticated<FriendInvite>('/v1/friends/invites', {
      method: 'POST', body: JSON.stringify({ alias }),
    }),
    async acceptFriendInvite(code: string, alias: string) {
      try {
        return await authenticated<{ friend: Friend }>('/v1/friends/accept', {
          method: 'POST', body: JSON.stringify({ code, alias }),
        });
      } catch (error) {
        if (error instanceof APIError && [400, 403, 404, 409, 410].includes(error.status)) {
          throw new Error('Check the invite code and display name. Ask for a new code if it expired or was used.');
        }
        throw error;
      }
    },
    setFriendFlight: (id: string, flightId: string, shared: boolean) =>
      authenticated<void>(`${friendPath(id)}/flights/${encodeURIComponent(flightId)}`, { method: shared ? 'PUT' : 'DELETE' }),
    setFriendAlerts: (id: string, mode: FriendAlertMode) =>
      authenticated<void>(`${friendPath(id)}/alerts`, { method: 'PUT', body: JSON.stringify({ mode }) }),
    removeFriend: (id: string) => authenticated<void>(friendPath(id), { method: 'DELETE' }),
    connection: (inbound: string, outbound: string, signal?: AbortSignal) => {
      const query = new URLSearchParams({ inbound, outbound });
      return authenticated<Connection>(`/v1/connections?${query}`, { signal });
    },
    async connectionSuggestions(): Promise<ConnectionSuggestion[] | undefined> {
      try {
        return (await authenticated<{ suggestions: ConnectionSuggestion[] }>('/v1/connection-suggestions')).suggestions ?? [];
      } catch (error) {
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) return undefined;
        throw error;
      }
    },
    async connectionMonitors(signal?: AbortSignal): Promise<ConnectionMonitor[]> {
      try {
        return (await authenticated<{ monitors: ConnectionMonitor[] }>('/v1/connection-monitors', { signal })).monitors ?? [];
      } catch (error) {
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) return [];
        throw error;
      }
    },
    async setConnectionMonitoring(inbound: string, outbound: string, enabled: boolean): Promise<boolean> {
      const path = `/v1/connection-monitors?${new URLSearchParams({ inbound, outbound })}`;
      try {
        if (enabled) return (await authenticated<{ monitoring: boolean }>(path, { method: 'PUT' })).monitoring;
        await authenticated<void>(path, { method: 'DELETE' });
        return false;
      } catch (error) {
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) {
          throw new Error('This Plano server doesn’t support connection alerts.');
        }
        throw error;
      }
    },
    importTripIt: (url: string, offset = 0) => authenticated<ImportResult>('/v1/import/tripit', {
      method: 'POST', body: JSON.stringify({ url, offset }),
    }, 28_000),
    async tripItSubscription(signal?: AbortSignal): Promise<TripItSubscriptionStatus> {
      try {
        return await authenticated<TripItSubscriptionStatus>('/v1/import/tripit/subscription', { signal });
      } catch (error) {
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) {
          return { available: false, enabled: false };
        }
        throw error;
      }
    },
    saveTripItSubscription: (url: string) => authenticated<void>('/v1/import/tripit/subscription', {
      method: 'PUT', body: JSON.stringify({ url }),
    }),
    removeTripItSubscription: () => authenticated<void>('/v1/import/tripit/subscription', { method: 'DELETE' }),
    createEmailRelay: () => authenticated<{ url: string; token: string }>('/v1/import/email-relay', {
      method: 'POST', body: '{}',
    }),
    revokeEmailRelay: () => authenticated<void>('/v1/import/email-relay', { method: 'DELETE' }),
    lookupBooking: (locator: string) => authenticated<{ flights: Flight[] }>('/v1/bookings/lookup', {
      method: 'POST', body: JSON.stringify({ locator }),
    }),
    async resolveBooking(details: PartnerBooking) {
      try {
        return await authenticated<ImportResult>('/v1/bookings/resolve', {
          method: 'POST', body: JSON.stringify(details),
        }, 28_000);
      } catch (error) {
        if (error instanceof APIError && [404, 503].includes(error.status)) {
          throw new Error('The partner booking lookup could not return a booking. Check the details or contact your Plano server operator.');
        }
        throw error;
      }
    },
    importFlights: (source: ImportSource, text: string, date?: string) =>
      authenticated<ImportResult>('/v1/import', {
        method: 'POST', body: JSON.stringify({ source, text, date }),
      }, 22_000),
    async syncCalendarFlights(flights: CalendarFlight[]) {
      try {
        const result = await authenticated<{ completed: string[] }>('/v1/import/calendar-sync', {
          method: 'POST', body: JSON.stringify({ flights }),
        }, 22_000);
        if (!Array.isArray(result?.completed) || !result.completed.every((key) => typeof key === 'string')) {
          throw new Error('Plano returned an invalid calendar sync response.');
        }
        return result.completed;
      } catch (error) {
        if (error instanceof APIError && error.routeMissing) return null;
        throw error;
      }
    },
    async revokeDevices(binding?: string) {
      if (binding !== undefined && !androidBindingPattern.test(binding)) throw new Error('Invalid Android alert binding.');
      const token = await savedToken();
      if (!token) return;
      try {
        await request<void>(server, '/v1/devices', {
          method: 'DELETE',
          headers: { Authorization: `Bearer ${token}` },
          body: binding === undefined ? undefined : JSON.stringify({ platform: 'android', kind: 'alert', binding }),
        });
      } catch (cause) {
        if (!(cause instanceof APIError) || cause.status !== 401) throw cause;
      }
    },
    async registerDevice(device: Device, token?: string) {
      const clocked = device.pushClock !== undefined && await supportsIOSPushClock();
      const installation = token ?? await installationToken();
      const submit = (withClock: boolean) => requestAs<void>(installation, '/v1/devices', {
        method: 'POST', body: JSON.stringify(withClock ? device : { ...device, pushClock: undefined }),
      });
      try {
        await submit(clocked);
      } catch (cause) {
        if (!clocked || !(cause instanceof APIError) || cause.status !== 400) throw cause;
        iosPushClockSupport = undefined;
        if (await supportsIOSPushClock()) throw cause;
        await submit(false);
      }
    },
  };
}

export type PlanoAPI = ReturnType<typeof createAPI>;
