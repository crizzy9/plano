import { requireOptionalNativeModule } from 'expo';
import { Platform } from 'react-native';
import type { Flight, PlanoAPI } from './api';
import { arrival } from './flight';

export type AndroidPushGeneration = {
  generation: string;
  enabled: boolean;
  suppressed: boolean;
};

export type RetiredAndroidPush = { origin: string; binding: string };

type NativeLive = {
  addListener?: (event: 'onFlightAlert', listener: (payload: unknown) => void) => { remove: () => void };
  configure?: (server: string, token: string) => Promise<void>;
  start?: (state: string) => Promise<void>;
  iosLiveActivityBinding?: (origin: string) => Promise<string | null>;
  stopLiveActivityInSession?: (origin: string, binding: string, flightId: string) => Promise<void>;
  clear: () => Promise<void>;
  selectAndroidPushOrigin?: (origin: string) => Promise<RetiredAndroidPush | null>;
  androidPushStatus?: (origin: string) => Promise<AndroidPushGeneration | null>;
  enableAndroidPush?: (origin: string, generation: string, explicit: boolean) => Promise<boolean>;
  disableAndroidPush?: (origin: string, expected: string | null, suppress: boolean) => Promise<RetiredAndroidPush | null>;
  androidWidgetDetailsEnabled?: () => Promise<boolean>;
  setAndroidWidgetDetailsEnabled?: (enabled: boolean) => Promise<void>;
  resolveWidgetFlight?: (origin: string, key: string) => Promise<string | null>;
  androidWidgetFlightPinned?: (origin: string, flightId: string) => Promise<boolean | null>;
  setAndroidWidgetFlightPinned?: (origin: string, flightId: string, pinned: boolean) => Promise<boolean>;
  nextIOSPushRevision?: () => Promise<string>;
  retireIOSPush?: (origin: string, token: string) => Promise<string>;
};

const native = Platform.OS === 'web' ? null : requireOptionalNativeModule<NativeLive>('PlanoLive');

export function addAndroidFlightAlertListener(listener: (payload: unknown) => void) {
  if (Platform.OS === 'android') return native?.addListener?.('onFlightAlert', listener);
}

export const canStartLiveActivity = Boolean(native?.start);
export const canCustomizeAndroidWidget = Platform.OS === 'android'
  && !!native?.androidWidgetDetailsEnabled && !!native?.setAndroidWidgetDetailsEnabled;
export const canPinAndroidWidgetFlight = Platform.OS === 'android'
  && !!native?.androidWidgetFlightPinned && !!native?.setAndroidWidgetFlightPinned;
export const canFenceIOSPush = Platform.OS === 'ios' && !!native?.nextIOSPushRevision && !!native?.retireIOSPush;

function iosPushRevision(value: string) {
  const revision = Number(value);
  if (!/^[1-9][0-9]*$/.test(value) || !Number.isSafeInteger(revision)) throw new Error('Invalid iOS alert revision.');
  return revision;
}

export async function nextIOSPushRevision() {
  if (!native?.nextIOSPushRevision) throw new Error('Protected flight alerts require a current iOS build of Plano.');
  return iosPushRevision(await native.nextIOSPushRevision());
}

export async function retireIOSPush(origin: string, token: string) {
  if (!native?.retireIOSPush) throw new Error('Protected flight alerts require a current iOS build of Plano.');
  return iosPushRevision(await native.retireIOSPush(origin, token));
}

export async function androidWidgetDetailsEnabled() {
  if (!native?.androidWidgetDetailsEnabled) throw new Error('Widget settings require a current Android build of Plano.');
  return native.androidWidgetDetailsEnabled();
}

export async function setAndroidWidgetDetailsEnabled(enabled: boolean) {
  if (!native?.setAndroidWidgetDetailsEnabled) throw new Error('Widget settings require a current Android build of Plano.');
  await native.setAndroidWidgetDetailsEnabled(enabled);
}

export async function resolveAndroidWidgetFlight(origin: string, key: string) {
  if (Platform.OS !== 'android') return null;
  return await native?.resolveWidgetFlight?.(origin, key) ?? null;
}

export async function androidWidgetFlightPinned(origin: string, flightId: string) {
  if (Platform.OS !== 'android') return null;
  return await native?.androidWidgetFlightPinned?.(origin, flightId) ?? null;
}

export async function setAndroidWidgetFlightPinned(origin: string, flightId: string, pinned: boolean) {
  if (Platform.OS !== 'android' || !native?.setAndroidWidgetFlightPinned) {
    throw new Error('Widget selection requires a current Android build of Plano.');
  }
  return native.setAndroidWidgetFlightPinned(origin, flightId, pinned);
}

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

export async function captureLiveProgressSession(origin: string) {
  if (!native?.stopLiveActivityInSession) return null;
  if (Platform.OS === 'android') {
    const status = await androidPushStatus(origin);
    return status?.enabled ? status.generation : null;
  }
  return await native.iosLiveActivityBinding?.(origin) ?? null;
}

export async function stopLiveActivityInSession(origin: string, binding: string, flightId: string) {
  await native?.stopLiveActivityInSession?.(origin, binding, flightId);
}

export async function startLiveActivity(api: PlanoAPI, flight: Flight, seat: string) {
  if (!native?.start) throw new Error('Live Activities require a native Plano build on iOS.');
  await configureLiveActivities(api);
  const epoch = (value: string | undefined) => Math.floor(Date.parse(value ?? '') / 1000) || 0;
  const progressStart = flight.actualDeparture || flight.actualTakeoff || flight.estimatedDeparture || flight.scheduledDeparture;
  await native.start(JSON.stringify({
    flightId: flight.id, flightNumber: flight.number, status: flight.status, origin: flight.origin,
    destination: flight.destination, departureGate: flight.departureGate, arrivalGate: flight.arrivalGate,
    departureTerminal: flight.departureTerminal, arrivalTerminal: flight.arrivalTerminal,
    departureEpoch: epoch(progressStart), arrivalEpoch: epoch(arrival(flight)),
    observedEpoch: epoch(flight.observedAt), seat: seat.trim().toUpperCase() || undefined,
  }));
}
