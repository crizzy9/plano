import { CryptoDigestAlgorithm, digestStringAsync } from 'expo-crypto';
import { File, Paths } from 'expo-file-system';
import { Platform } from 'react-native';
import type { Flight, FlightDetail, FlightEvent, FlightStatus, Position } from './api';
import { statuses } from './flight';

const cacheName = 'plano-offline-flights-v1.json';
const stagedName = `${cacheName}.tmp`;
const maxCacheBytes = 512 * 1024;
const maxAge = 72 * 60 * 60 * 1000;
const maxOverview = 150;
const maxDetails = 8;
const serverOverviewLimit = 1_000;

interface SavedOverview {
  flights: Flight[];
  savedAt: string;
}

interface SavedFlight {
  detail: FlightDetail;
  savedAt: string;
}

interface SavedFlights {
  scope: string;
  overview?: SavedOverview;
  details: SavedFlight[];
}

export async function offlineFlightScope(server: string, token: string): Promise<string | undefined> {
  if (Platform.OS === 'web' || !server || !token) return undefined;
  try {
    return await digestStringAsync(CryptoDigestAlgorithm.SHA256, JSON.stringify([server, token]));
  } catch {
    return undefined;
  }
}

function object(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined;
}

function text(value: unknown, limit = 128) {
  return typeof value === 'string' && value.length <= limit ? value : undefined;
}

function timestamp(value: unknown) {
  const candidate = text(value, 64);
  return candidate && Number.isFinite(Date.parse(candidate)) ? candidate : undefined;
}

function freshTimestamp(value: unknown, now: number) {
  const candidate = timestamp(value);
  if (!candidate) return undefined;
  const age = now - Date.parse(candidate);
  return age >= 0 && age < maxAge ? candidate : undefined;
}

function finite(value: unknown) {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function flightId(value: unknown) {
  const id = text(value);
  return id && /^[A-Za-z0-9_.-]+$/.test(id) ? id : undefined;
}

function registrySnapshot(value: unknown): Flight['aircraftRegistry'] {
  const registry = object(value);
  if (registry?.source !== 'transport_canada_ccar' && registry?.source !== 'australia_casa') return undefined;
  const model = text(registry.model);
  const age = finite(registry.age);
  if (!model && age === undefined) return undefined;
  return { source: registry.source, model: model || undefined, age };
}

function flightSnapshot(value: unknown): Flight | undefined {
  const source = object(value);
  if (!source) return undefined;
  const id = flightId(source.id);
  const number = text(source.number, 32);
  const origin = text(source.origin, 16);
  const destination = text(source.destination, 16);
  const code = text(source.status, 32);
  const status = code && Object.hasOwn(statuses, code) ? code as FlightStatus : 'unknown';
  const observedAt = timestamp(source.observedAt);
  if (!id || !number || origin === undefined || destination === undefined || !observedAt) return undefined;
  return {
    id, number, origin, destination, status, observedAt, source: text(source.source, 64),
    originICAO: text(source.originICAO, 16), destinationICAO: text(source.destinationICAO, 16),
    originTimeZone: text(source.originTimeZone, 64), destinationTimeZone: text(source.destinationTimeZone, 64),
    scheduledDeparture: timestamp(source.scheduledDeparture), scheduledArrival: timestamp(source.scheduledArrival),
    estimatedDeparture: timestamp(source.estimatedDeparture), estimatedArrival: timestamp(source.estimatedArrival),
    actualDeparture: timestamp(source.actualDeparture), actualTakeoff: timestamp(source.actualTakeoff),
    actualLanding: timestamp(source.actualLanding), actualArrival: timestamp(source.actualArrival),
    departureGate: text(source.departureGate, 32), arrivalGate: text(source.arrivalGate, 32),
    departureTerminal: text(source.departureTerminal, 32), arrivalTerminal: text(source.arrivalTerminal, 32),
    tailNumber: text(source.tailNumber, 32), aircraftType: text(source.aircraftType, 64),
    aircraftModel: text(source.aircraftModel, 128),
    aircraftRegistry: registrySnapshot(source.aircraftRegistry),
    aircraftAge: finite(source.aircraftAge), inboundId: flightId(source.inboundId), baggage: text(source.baggage, 32),
  };
}

function eventSnapshot(value: unknown): FlightEvent | undefined {
  const source = object(value);
  const at = timestamp(source?.at);
  if (!source || !at) return undefined;
  const body = text(source.body, 240);
  return {
    at, id: text(source.id), type: text(source.type, 32), kind: text(source.kind, 32),
    title: text(source.title), body: body && !/https?:\/\//i.test(body) ? body : undefined,
    occurredAt: timestamp(source.occurredAt), observedAt: timestamp(source.observedAt),
  };
}

function positionSnapshot(value: unknown): Position | undefined {
  const source = object(value);
  const latitude = finite(source?.latitude);
  const longitude = finite(source?.longitude);
  const at = timestamp(source?.at);
  if (!source || !at || latitude === undefined || Math.abs(latitude) > 90 ||
      longitude === undefined || Math.abs(longitude) > 180) return undefined;
  const trend = source.verticalTrend;
  return {
    latitude, longitude, at, altitudeFeet: finite(source.altitudeFeet), groundSpeedKnots: finite(source.groundSpeedKnots),
    heading: finite(source.heading), verticalRateFpm: finite(source.verticalRateFpm), source: text(source.source, 64),
    verticalTrend: trend === 'climbing' || trend === 'descending' || trend === 'level' ? trend : undefined,
  };
}

function snapshots<T>(value: unknown, cap: number, convert: (entry: unknown) => T | undefined, newestLast = false) {
  if (!Array.isArray(value)) return [];
  return value.slice(newestLast ? -cap : 0, newestLast ? undefined : cap)
    .map(convert).filter((entry): entry is T => entry !== undefined);
}

function detailSnapshot(value: unknown): FlightDetail | undefined {
  const source = object(value);
  const flight = flightSnapshot(source?.flight);
  if (!source || !flight || source.rotationSnapshot === true) return undefined;
  return {
    flight,
    events: snapshots(source.events, 24, eventSnapshot),
    telemetry: snapshots(source.telemetry, 96, positionSnapshot, true),
    receiverRevision: text(source.receiverRevision, 128),
    inbound: snapshots(source.inbound, 12, flightSnapshot),
  };
}

function readFile(name: string) {
  try {
    const file = new File(Paths.cache, name);
    const size = file.size;
    if (size > 0 && size <= maxCacheBytes) return object(JSON.parse(file.textSync()));
  } catch {
    // Cache misses, partial files, and I/O errors all fall back to the network.
  }
  return undefined;
}

function readCache(scope: string): SavedFlights {
  const empty: SavedFlights = { scope, details: [] };
  if (Platform.OS === 'web') return empty;
  const saved = readFile(cacheName);
  if (saved?.scope !== scope) return empty;
  const now = Date.now();
  const savedOverview = object(saved.overview);
  const overviewAt = freshTimestamp(savedOverview?.savedAt, now);
  return {
    scope,
    overview: overviewAt && Array.isArray(savedOverview?.flights) ? {
      savedAt: overviewAt, flights: snapshots(savedOverview.flights, maxOverview, flightSnapshot),
    } : undefined,
    details: snapshots(saved.details, maxDetails, (value) => {
      const record = object(value);
      const savedAt = freshTimestamp(record?.savedAt, now);
      const detail = detailSnapshot(record?.detail);
      return savedAt && detail ? { detail, savedAt } : undefined;
    }),
  };
}

function writeCache(cache: SavedFlights): boolean {
  if (Platform.OS === 'web') return false;
  try {
    const encoder = new TextEncoder();
    let bytes = encoder.encode(JSON.stringify(cache));
    while (bytes.byteLength > maxCacheBytes) {
      if (cache.details.length > 1) cache.details.pop();
      else if (cache.overview?.flights.length) {
        cache.overview.flights = cache.overview.flights.slice(0, Math.floor(cache.overview.flights.length / 2));
      } else return false;
      bytes = encoder.encode(JSON.stringify(cache));
    }
    const staged = new File(Paths.cache, stagedName);
    staged.create({ overwrite: true });
    staged.write(bytes);
    if (staged.size !== bytes.byteLength) return false;
    staged.moveSync(new File(Paths.cache, cacheName), { overwrite: true });
    return true;
  } catch {
    // Device storage is best effort and does not affect live results.
    return false;
  }
}

export function readSavedOverview(scope: string): SavedOverview | undefined {
  return readCache(scope).overview;
}

export function saveOverview(scope: string, flights: Flight[]): void {
  const cache = readCache(scope);
  const tracked = new Set(flights.map((flight) => flight.id));
  // The server caps the overview. Only a complete list proves that an
  // omitted flight, such as a detail opened from Passport, was untracked.
  const details = flights.length < serverOverviewLimit
    ? cache.details.filter(({ detail }) => tracked.has(detail.flight.id))
    : cache.details;
  const lostTracking = cache.overview?.flights.some((flight) => !tracked.has(flight.id)) ||
    details.length < cache.details.length;
  cache.overview = { flights: snapshots(flights, maxOverview, flightSnapshot), savedAt: new Date().toISOString() };
  cache.details = details;
  if (!writeCache(cache) && lostTracking) clearSavedFlights(scope);
}

export function readSavedFlight(scope: string, id: string): SavedFlight | undefined {
  const { overview, details } = readCache(scope);
  const saved = details.find(({ detail }) => detail.flight.id === id);
  const flight = overview?.flights.find((item) => item.id === id);
  if (!flight || !overview) return saved;
  if (saved && Date.parse(saved.detail.flight.observedAt) >= Date.parse(flight.observedAt)) return saved;
  const sameAircraft = saved && saved.detail.flight.tailNumber === flight.tailNumber &&
    saved.detail.flight.actualTakeoff === flight.actualTakeoff;
  return { detail: { ...(sameAircraft ? saved?.detail : undefined), flight }, savedAt: overview.savedAt };
}

export function saveFlightDetail(scope: string, detail: FlightDetail): void {
  const snapshot = detailSnapshot(detail);
  if (!snapshot) return;
  const cache = readCache(scope);
  const previous = cache.details.find((entry) => entry.detail.flight.id === snapshot.flight.id);
  if (previous && Date.parse(previous.detail.flight.observedAt) > Date.parse(snapshot.flight.observedAt)) return;
  cache.details = [{ detail: snapshot, savedAt: new Date().toISOString() },
    ...cache.details.filter((entry) => entry.detail.flight.id !== snapshot.flight.id)].slice(0, maxDetails);
  writeCache(cache);
}

export function forgetSavedFlight(scope: string, id: string): void {
  const cache = readCache(scope);
  const overview = cache.overview;
  const details = cache.details.filter(({ detail }) => detail.flight.id !== id);
  const flights = overview?.flights.filter((flight) => flight.id !== id);
  if (details.length === cache.details.length && flights?.length === overview?.flights.length) return;
  if (overview && flights) overview.flights = flights;
  if (!writeCache({ ...cache, details })) clearSavedFlights(scope);
}

export function clearSavedFlights(scope: string): void {
  if (Platform.OS === 'web') return;
  for (const name of [cacheName, stagedName]) {
    if (readFile(name)?.scope !== scope) continue;
    try {
      new File(Paths.cache, name).delete();
    } catch {
      // Cache files can be evicted independently by the system.
    }
  }
}
