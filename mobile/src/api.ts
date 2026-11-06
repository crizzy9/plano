import * as SecureStore from 'expo-secure-store';
import { Platform } from 'react-native';

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
}

export interface Connection {
  risk: string;
  availableMinutes: number;
  transferMinutes: number;
  airport: string;
  instructions: string[];
}

export type ImportSource = 'calendar' | 'email' | 'tripit';

export interface Device {
  platform: 'ios' | 'android';
  token: string;
  kind: 'alert' | 'live_start' | 'live_update';
  flightId?: string;
}

const defaultURL = process.env.EXPO_PUBLIC_API_URL || (__DEV__ ? Platform.select({
  android: 'http://10.0.2.2:8080',
  default: 'http://127.0.0.1:8080',
}) : '') || '';
const hostKey = 'plano.server.v1';
const cleanupKey = 'plano.retired-servers.v1';
let retirementQueue = Promise.resolve();

async function retiredServers(): Promise<string[]> {
  return JSON.parse(await SecureStore.getItemAsync(cleanupKey) || '[]') as string[];
}

function serializeRetirement(operation: () => Promise<void>) {
  const next = retirementQueue.then(operation);
  retirementQueue = next.catch(() => {});
  return next;
}

export function removeRetiredDevices(activeServer: string) {
  if (Platform.OS === 'web' || !activeServer) return Promise.resolve();
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

export async function loadServer(): Promise<string> {
  const saved = Platform.OS === 'web'
    ? globalThis.localStorage?.getItem(hostKey)
    : await SecureStore.getItemAsync(hostKey);
  if (!saved && !defaultURL) return '';
  return normalizeServer(saved || defaultURL);
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
      const pending = await retiredServers();
      if (previous && previous !== url && !pending.includes(previous)) pending.push(previous);
      await SecureStore.setItemAsync(cleanupKey, JSON.stringify(pending.filter((item) => item !== url)));
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

async function request<T>(server: string, path: string, init: RequestInit = {}): Promise<T> {
  if (!server) throw new Error('Connect to a Plano server in Settings.');
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 12_000);
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

export function createAPI(server: string) {
  // The canonical origin is part of the key, so two installations on different
  // hosts can never use each other's bearer token.
  const tokenKey = serverStorageKey('installation', server);
  let tokenPromise: Promise<string> | undefined;

  async function savedToken() {
    return Platform.OS === 'web'
      ? globalThis.sessionStorage?.getItem(tokenKey)
      : SecureStore.getItemAsync(tokenKey);
  }

  function installationToken(enrollmentCode?: string): Promise<string> {
    tokenPromise ??= (async () => {
      const existing = await savedToken();
      if (existing) return existing;
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

  async function authenticated<T>(path: string, init: RequestInit = {}) {
    const token = await installationToken();
    return request<T>(server, path, { ...init, headers: { Authorization: `Bearer ${token}` } });
  }

  return {
    server,
    installationToken,
    async search(number: string, date: string): Promise<Flight[]> {
      const query = new URLSearchParams({ number, date });
      return (await authenticated<{ flights: Flight[] }>(`/v1/flights?${query}`)).flights;
    },
    async tracked(): Promise<Flight[]> {
      return (await authenticated<{ flights: Flight[] }>('/v1/tracked')).flights;
    },
    detail: (id: string) => authenticated<FlightDetail>(flightPath(id)),
    track: (flightId: string, seat?: string) => authenticated<void>('/v1/tracked', {
      method: 'POST', body: JSON.stringify({ flightId, seat }),
    }),
    untrack: (id: string) => authenticated<void>(`/v1/tracked/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    share: (flightId: string) => authenticated<{ url: string; expiresAt: string }>('/v1/shares', {
      method: 'POST', body: JSON.stringify({ flightId }),
    }),
    passport: () => authenticated<Passport>('/v1/passport'),
    connection: (inbound: string, outbound: string) => {
      const query = new URLSearchParams({ inbound, outbound });
      return authenticated<Connection>(`/v1/connections?${query}`);
    },
    importFlights: (source: ImportSource, text: string, date?: string) =>
      authenticated<{ flights: Flight[]; unmatched: string[] }>('/v1/import', {
        method: 'POST', body: JSON.stringify({ source, text, date }),
      }),
    async revokeDevices() {
      const token = await savedToken();
      if (!token) return;
      try {
        await request<void>(server, '/v1/devices', {
          method: 'DELETE', headers: { Authorization: `Bearer ${token}` },
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
