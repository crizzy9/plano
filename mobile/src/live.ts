import { requireOptionalNativeModule } from 'expo';
import { Platform } from 'react-native';
import type { Flight, PlanoAPI } from './api';
import { arrival, departure } from './flight';

export type AndroidPushGeneration = {
  generation: string;
  enabled: boolean;
  suppressed: boolean;
};

export type RetiredAndroidPush = { origin: string; binding: string };

type NativeLive = {
  configure?: (server: string, token: string) => Promise<void>;
  start?: (state: string) => Promise<void>;
  stop: (flightId: string) => Promise<void>;
  clear: () => Promise<void>;
  selectAndroidPushOrigin?: (origin: string) => Promise<RetiredAndroidPush | null>;
  androidPushStatus?: (origin: string) => Promise<AndroidPushGeneration | null>;
  enableAndroidPush?: (origin: string, generation: string, explicit: boolean) => Promise<boolean>;
  disableAndroidPush?: (origin: string, expected: string | null, suppress: boolean) => Promise<RetiredAndroidPush | null>;
};

const native = Platform.OS === 'web' ? null : requireOptionalNativeModule<NativeLive>('PlanoLive');

export const canStartLiveActivity = Boolean(native?.start);

export async function selectAndroidPushOrigin(origin: string) {
  if (Platform.OS !== 'android') return null;
  if (native?.selectAndroidPushOrigin) return native.selectAndroidPushOrigin(origin);
  await native?.clear();
  return null;
}

export async function androidPushStatus(origin: string) {
  return await native?.androidPushStatus?.(origin) ?? null;
}

export async function enableAndroidPush(origin: string, generation: string, explicit: boolean) {
  if (!native?.enableAndroidPush) throw new Error('Protected flight alerts require a current Android build of Plano.');
  return native.enableAndroidPush(origin, generation, explicit);
}

export async function disableAndroidPush(origin: string, expected: string | null, suppress: boolean) {
  return await native?.disableAndroidPush?.(origin, expected, suppress) ?? null;
}

export async function configureLiveActivities(api: PlanoAPI) {
  if (native?.configure) await native.configure(api.server, await api.installationToken());
}

export async function clearLiveActivities() {
  await native?.clear();
}

export async function stopLiveActivity(flightId: string) {
  await native?.stop(flightId);
}

export async function startLiveActivity(api: PlanoAPI, flight: Flight, seat: string) {
  if (!native?.start) throw new Error('Live Activities require a native Plano build on iOS.');
  await configureLiveActivities(api);
  const epoch = (value: string | undefined) => Math.floor(Date.parse(value ?? '') / 1000) || 0;
  await native.start(JSON.stringify({
    flightId: flight.id, flightNumber: flight.number, status: flight.status, origin: flight.origin,
    destination: flight.destination, departureGate: flight.departureGate, arrivalGate: flight.arrivalGate,
    departureTerminal: flight.departureTerminal, arrivalTerminal: flight.arrivalTerminal,
    departureEpoch: epoch(departure(flight)), arrivalEpoch: epoch(arrival(flight)),
    observedEpoch: epoch(flight.observedAt), seat: seat.trim().toUpperCase() || undefined,
  }));
}
