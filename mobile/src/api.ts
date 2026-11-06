import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';
import { disableAndroidPush, selectAndroidPushOrigin, type RetiredAndroidPush } from './live';

export type FlightStatus = 'scheduled' | 'boarding' | 'taxi_out' | 'airborne'
  | 'landed' | 'arrived' | 'cancelled' | 'diverted';

export interface Flight {
  id: string;
  number: string;
  origin: string;
  destination: string;
  scheduledDeparture?: string;
  scheduledArrival?: string;
  originTimeZone?: string;
  destinationTimeZone?: string;
  estimatedDeparture?: string;
  estimatedArrival?: string;
  actualDeparture?: string;
  actualArrival?: string;
  departureGate?: string;
  arrivalGate?: string;
  departureTerminal?: string;
  arrivalTerminal?: string;
  tailNumber?: string;
  aircraftType?: string;
  aircraftAge?: number;
  status: FlightStatus;
  baggage?: string;
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
  at: string;
  source?: string;
}

export interface RadarBatch {
  positions: Position[];
  cursor?: string;
}

export interface Prediction {
  delayMinutes?: number;
  departureDelayMinutes?: number;
  reason?: string;
  explanation?: string;
  reasons?: string[];
  departure?: string;
  method?: string;
}

export interface FlightDetail {
  flight: Flight;
  events?: FlightEvent[];
  telemetry?: Position[];
  inbound?: Flight[];
  prediction?: Prediction | null;
}

export interface Friend {
  id: string;
  alias: string;
  direction: 'outgoing' | 'incoming';
  flightIds: string[];
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

export interface Passport {
  flights: Flight[];
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
  availableMinutes: number;
  transferMinutes: number;
  airport: string;
  instructions: string[];
  precision?: string;
  mapUrl?: string;
  arrivalGatePrediction?: GatePrediction;
  departureGatePrediction?: GatePrediction;
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

export type ImportSource = 'calendar' | 'email' | 'tripit';

export interface Device {
  platform: 'ios' | 'android';
  token: string;
  kind: 'alert' | 'live_start' | 'live_update';
  flightId?: string;
  binding?: string;
}

const defaultURL = process.env.EXPO_PUBLIC_API_URL || (__DEV__ ? Platform.select({
  android: 'http://10.0.2.2:8080',
  default: 'http://127.0.0.1:8080',
}) : '') || '';
const hostKey = 'plano.server.v1';
const cleanupKey = 'plano.retired-servers.v1';
const androidCleanupKey = 'plano.retired-android-push.v1';
const androidBindingPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const androidDeviceRequests = new Map<string, Promise<void>>();
let drainingAndroidRetirements: Promise<void> | undefined;
const calendarBackgroundStore = {
  keychainService: 'plano.background.v1',
  keychainAccessible: SecureStore.AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY,
};
let retirementQueue = Promise.resolve();

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

export function removeRetiredDevices(activeServer: string) {
  if (Platform.OS === 'web' || !activeServer) return Promise.resolve();
  if (Platform.OS === 'android') {
    drainingAndroidRetirements ??= drainAndroidRetirements().finally(() => { drainingAndroidRetirements = undefined; });
    return drainingAndroidRetirements;
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

export async function prepareCalendarBackground(server: string) {
  await serializeRetirement(async () => {
    if (Platform.OS === 'web' || await loadServer() !== server) {
      throw new Error('Choose a Plano server before enabling background calendar sync.');
    }
    const key = serverStorageKey('installation', server);
    const token = await SecureStore.getItemAsync(key);
    if (!token) throw new Error('Connect to your Plano server before enabling background calendar sync.');
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

export async function saveServer(value: string, enrollmentCode?: string, previous?: string): Promise<string> {
  const url = normalizeServer(value);
  const health = await request<{ status?: string; mode?: string }>(url, '/healthz');
  if (health?.status !== 'ok' || !['demo', 'live'].includes(health.mode ?? '')) {
    throw new Error('This address did not return a Plano health response.');
  }
  await createAPI(url).installationToken(enrollmentCode);
  if (Platform.OS === 'web') {
    globalThis.localStorage?.setItem(hostKey, url);
  } else {
    await serializeRetirement(async () => {
      const current = await loadServer();
      if (previous !== undefined && current !== previous && current !== url) {
        throw new Error('The active Plano server changed. Try connecting again.');
      }
      if (Platform.OS === 'android') await recordAndroidRetirement(await selectAndroidPushOrigin(url));
      const previousBackground = await loadServer({ savedOnly: true });
      if (previousBackground && previousBackground !== url) await removeCalendarBackground(previousBackground);
      if (Platform.OS !== 'android') {
        const pending = await retiredServers();
        if (previous && previous !== url && !pending.includes(previous)) pending.push(previous);
        await SecureStore.setItemAsync(cleanupKey, JSON.stringify(pending.filter((item) => item !== url)));
      }
      await SecureStore.setItemAsync(hostKey, url);
    });
  }
  return url;
}

class APIError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

async function request<T>(server: string, path: string, init: RequestInit = {}, timeoutMs = 12_000): Promise<T> {
  if (!server) throw new Error('Connect to a Plano server in Settings.');
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const response = await fetch(server + path, {
      ...init,
      headers: { Accept: 'application/json', ...(init.body ? { 'Content-Type': 'application/json' } : {}), ...init.headers },
      signal: controller.signal,
      redirect: 'error',
    });
    if (!response.ok) {
      if (response.status === 401) throw new APIError(401, 'This installation needs to reconnect.');
      if (response.status === 403) throw new APIError(403, 'This server requires an enrollment code. Enter it in Settings.');
      if (response.status === 404) throw new APIError(404, 'That information is unavailable on this server.');
      if (response.status === 429) throw new APIError(429, 'Too many requests. Try again in a moment.');
      if (response.status >= 500) throw new APIError(response.status, 'Plano’s server is temporarily unavailable.');
      throw new APIError(response.status, 'Plano couldn’t complete that request.');
    }
    const raw = await response.text();
    return (raw ? JSON.parse(raw) : undefined) as T;
  } catch (error) {
    if (error instanceof APIError) throw error;
    throw new Error('Couldn’t reach Plano. Check the server address and your connection.');
  } finally {
    clearTimeout(timeout);
  }
}

export function serverStorageKey(scope: string, server: string) {
  return `plano.${scope}.` + Array.from(server)
    .map((character) => character.codePointAt(0)?.toString(16).padStart(2, '0')).join('');
}

const flightPath = (id: string) => `/v1/flights/${encodeURIComponent(id)}`;
const friendPath = (id: string) => `/v1/friends/${encodeURIComponent(id)}`;

export function createAPI(server: string, options: { allowEnrollment?: boolean } = {}) {
  // The canonical origin is part of the key, so two installations on different
  // hosts can never use each other's bearer token.
  const tokenKey = serverStorageKey('installation', server);
  let tokenPromise: Promise<string> | undefined;

  async function savedToken() {
    return Platform.OS === 'web'
      ? globalThis.sessionStorage?.getItem(tokenKey)
      : SecureStore.getItemAsync(tokenKey, options.allowEnrollment === false ? calendarBackgroundStore : undefined);
  }

  function installationToken(enrollmentCode?: string): Promise<string> {
    tokenPromise ??= (async () => {
      const existing = await savedToken();
      if (existing) return existing;
      if (options.allowEnrollment === false) throw new Error('This server has no saved Plano installation.');
      const { token } = await request<{ token: string }>(server, '/v1/installations', {
        method: 'POST', body: JSON.stringify({ enrollmentCode: enrollmentCode || undefined }),
      });
      if (!token) throw new Error('Plano couldn’t register this installation.');
      if (Platform.OS === 'web') globalThis.sessionStorage?.setItem(tokenKey, token);
      else await SecureStore.setItemAsync(tokenKey, token);
      return token;
    })().catch((error: unknown) => {
      tokenPromise = undefined;
      throw error;
    });
    return tokenPromise;
  }

  async function authenticated<T>(path: string, init: RequestInit = {}, timeoutMs?: number) {
    const token = await installationToken();
    if (options.allowEnrollment === false && await loadServer({ savedOnly: true }) !== server) {
      throw new Error('Background access for this server ended.');
    }
    return request<T>(server, path, { ...init, headers: { Authorization: `Bearer ${token}` } }, timeoutMs);
  }

  return {
    server,
    hasSavedInstallationToken: async () => Boolean(await savedToken()),
    installationToken,
    async search(number: string, date: string): Promise<Flight[]> {
      const query = new URLSearchParams({ number, date });
      return (await authenticated<{ flights: Flight[] }>(`/v1/flights?${query}`)).flights;
    },
    async tracked(): Promise<Flight[]> {
      return (await authenticated<{ flights: Flight[] }>('/v1/tracked')).flights;
    },
    detail: (id: string) => authenticated<FlightDetail>(flightPath(id)),
    async radar(id: string, after?: string) {
      const query = after ? `?${new URLSearchParams({ after })}` : '';
      try {
        return await authenticated<RadarBatch>(`${flightPath(id)}/radar${query}`);
      } catch (error) {
        if (error instanceof APIError && [404, 405, 501].includes(error.status)) return undefined;
        throw error;
      }
    },
    track: (flightId: string, seat?: string) => authenticated<void>('/v1/tracked', {
      method: 'POST', body: JSON.stringify({ flightId, seat }),
    }),
    untrack: (id: string) => authenticated<void>(`/v1/tracked/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    share: (flightId: string) => authenticated<{ url: string; expiresAt: string; id?: string; createdAt?: string }>('/v1/shares', {
      method: 'POST', body: JSON.stringify({ flightId }),
    }),
    pickupLinks: (flightId: string) =>
      authenticated<{ links: PickupLink[] }>(`/v1/shares?${new URLSearchParams({ flightId })}`),
    revokePickupLink: (id: string) => authenticated<void>(`/v1/shares/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    passport: () => authenticated<Passport>('/v1/passport'),
    friends: () => authenticated<Friends>('/v1/friends'),
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
    removeFriend: (id: string) => authenticated<void>(friendPath(id), { method: 'DELETE' }),
    connection: (inbound: string, outbound: string) => {
      const query = new URLSearchParams({ inbound, outbound });
      return authenticated<Connection>(`/v1/connections?${query}`);
    },
    importTripIt: (url: string, offset = 0) => authenticated<ImportResult>('/v1/import/tripit', {
      method: 'POST', body: JSON.stringify({ url, offset }),
    }, 28_000),
    createEmailRelay: () => authenticated<{ url: string; token: string }>('/v1/import/email-relay', {
      method: 'POST', body: '{}',
    }),
    revokeEmailRelay: () => authenticated<void>('/v1/import/email-relay', { method: 'DELETE' }),
    lookupBooking: (locator: string) => authenticated<{ flights: Flight[] }>('/v1/bookings/lookup', {
      method: 'POST', body: JSON.stringify({ locator }),
    }),
    importFlights: (source: ImportSource, text: string, date?: string) =>
      authenticated<ImportResult>('/v1/import', {
        method: 'POST', body: JSON.stringify({ source, text, date }),
      }),
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
    registerDevice: (device: Device) => authenticated<void>('/v1/devices', {
      method: 'POST', body: JSON.stringify(device),
    }),
  };
}

export type PlanoAPI = ReturnType<typeof createAPI>;
