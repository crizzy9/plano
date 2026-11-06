import { useEffect } from 'react';
import * as SecureStore from 'expo-secure-store';
import { AppState, Platform } from 'react-native';
import {
  beginIOSPushIntent, deferAndroidPushRetirement, disableAndroidPushForOrigin, disableIOSPushForOrigin, initializeAndroidPushOrigin,
  removeRetiredDevices, runAndroidDeviceRequest, serverStorageKey, withActiveAndroidPushOrigin, withActiveIOSPushOrigin,
  type Flight, type PlanoAPI,
} from './api';
import {
  androidPushStatus, canFenceIOSPush, clearLiveActivities, configureLiveActivities, disableAndroidPush, enableAndroidPush,
  nextIOSPushRevision, startLiveActivity,
  type RetiredAndroidPush,
} from './live';

function currentPlatform() {
  return Platform.OS === 'ios' || Platform.OS === 'android' ? Platform.OS : undefined;
}

function registerIOSDevice(api: PlanoAPI, token: string, explicit: boolean, active: () => boolean) {
  return withActiveIOSPushOrigin(api.server, async () => {
    if (!active() || !explicit && !await wantsFlightAlerts(api)) return false;
    const liveRevision = canFenceIOSPush ? await nextIOSPushRevision() : undefined;
    await api.registerDevice({ platform: 'ios', token, kind: 'alert', liveRevision });
    if (!active()) return false;
    if (explicit) await SecureStore.setItemAsync(preferenceKey(api), 'on');
    if (explicit || canFenceIOSPush) await configureLiveActivities(api);
    return true;
  });
}

export async function startIOSFlightActivity(api: PlanoAPI, flight: Flight, seat: string) {
  const started = await withActiveIOSPushOrigin(api.server, async () => {
    if (canFenceIOSPush && !await wantsFlightAlerts(api)) {
      throw new Error('Enable flight alerts in Plano Settings before starting a Live Activity.');
    }
    await startLiveActivity(api, flight, seat);
    return true;
  });
  if (!started) throw new Error('The active server changed. Try starting the Live Activity again.');
}

function preferenceKey(api: PlanoAPI) {
  return serverStorageKey('flight-alerts', api.server);
}

async function wantsFlightAlerts(api: PlanoAPI) {
  return !!currentPlatform() && (await SecureStore.getItemAsync(preferenceKey(api))) === 'on';
}

export async function flightAlertsEnabled(api: PlanoAPI) {
  if (!await wantsFlightAlerts(api)) return false;
  return Platform.OS !== 'android' || (await androidPushStatus(api.server))?.enabled === true;
}

async function retireAndroidPush(api: PlanoAPI, retired: RetiredAndroidPush | null | undefined) {
  if (!retired) return;
  await deferAndroidPushRetirement(retired);
  void removeRetiredDevices(api.server).catch(() => {});
}

async function registerAndroidDevice(
  api: PlanoAPI,
  token: string,
  expected: string,
  explicit: boolean,
  active: () => boolean,
) {
  return runAndroidDeviceRequest(api.server, async () => {
    const before = await withActiveAndroidPushOrigin(api.server, async () => {
      const status = await androidPushStatus(api.server);
      if (!active() || status?.generation !== expected || !explicit && !await wantsFlightAlerts(api)) return null;
      return await enableAndroidPush(api.server, expected, explicit) ? status : null;
    });
    if (!before) return false;
    try {
      await api.registerDevice({ platform: 'android', token, kind: 'alert', binding: expected });
      const confirmed = await withActiveAndroidPushOrigin(api.server, async () => {
        const status = await androidPushStatus(api.server);
        if (!status?.enabled || status.generation !== expected) return false;
        if (explicit) await SecureStore.setItemAsync(preferenceKey(api), 'on');
        return true;
      });
      if (!confirmed) await retireAndroidPush(api, { origin: api.server, binding: expected });
      return confirmed === true;
    } catch (cause) {
      if (!before.enabled) {
        const retired = await withActiveAndroidPushOrigin(api.server,
          () => disableAndroidPush(api.server, expected, before.suppressed));
        await retireAndroidPush(api, retired);
      }
      throw cause;
    }
  });
}

export async function changeFlightAlerts(api: PlanoAPI, enable: boolean) {
  const platform = currentPlatform();
  if (!platform) return;
  const iosIntentIsCurrent = platform === 'ios' ? beginIOSPushIntent() : undefined;
  if (!enable) {
    if (platform === 'android') {
      await disableAndroidPushForOrigin(api.server);
    } else if (canFenceIOSPush) {
      await disableIOSPushForOrigin(api.server);
    } else {
      await withActiveIOSPushOrigin(api.server, async () => {
        await api.revokeDevices();
        await SecureStore.deleteItemAsync(preferenceKey(api));
        await clearLiveActivities();
      });
    }
    return;
  }

  if (platform === 'android') await initializeAndroidPushOrigin(api.server);
  const expected = platform === 'android' ? (await androidPushStatus(api.server))?.generation : undefined;
  if (platform === 'android' && !expected) throw new Error('Protected flight alerts require a current Android build of Plano.');
  const notifications = await import('expo-notifications');
  if (platform === 'android') {
    await notifications.setNotificationChannelAsync('flight-alerts', {
      name: 'Flight alerts', importance: notifications.AndroidImportance.HIGH,
    });
  }
  const permission = await notifications.requestPermissionsAsync();
  if (!permission.granted) throw new Error('Allow notifications for Plano in your device settings.');
  const device = await notifications.getDevicePushTokenAsync();
  if (typeof device.data !== 'string' || !device.data) throw new Error('The device didn’t return a push token.');
  if (platform === 'android' && expected) {
    if (!await registerAndroidDevice(api, device.data, expected, true, () => true)) {
      throw new Error('The active server or alert settings changed. Try enabling alerts again.');
    }
    return;
  }
  if (!iosIntentIsCurrent || !await registerIOSDevice(api, device.data, true, iosIntentIsCurrent)) {
    throw new Error('The active server or alert settings changed. Try enabling alerts again.');
  }
}

export function useFlightAlerts(api: PlanoAPI) {
  useEffect(() => {
    const platform = currentPlatform();
    if (!api.server || !platform) return;
    let active = true;
    let cleanup = () => {};

    void import('expo-notifications').then(async (notifications) => {
      if (platform === 'android') await initializeAndroidPushOrigin(api.server);
      if (!active) return;
      notifications.setNotificationHandler({
        handleNotification: async () => ({
          shouldShowBanner: true,
          shouldShowList: true,
          shouldPlaySound: false,
          shouldSetBadge: false,
        }),
      });
      async function register(token: unknown) {
        if (typeof token !== 'string' || !token || !await wantsFlightAlerts(api) || !active || !platform) return;
        if (platform === 'android') {
          const status = await androidPushStatus(api.server);
          if (status && !status.suppressed) {
            await registerAndroidDevice(api, token, status.generation, false, () => active);
          }
        } else {
          await registerIOSDevice(api, token, false, () => active);
        }
      }
      async function refresh() {
        if (!active || !await wantsFlightAlerts(api)) return;
        const permission = await notifications.getPermissionsAsync();
        if (permission.granted) await register((await notifications.getDevicePushTokenAsync()).data);
      }
      const token = notifications.addPushTokenListener((event) => { void register(event.data).catch(() => {}); });
      const state = AppState.addEventListener('change', (next) => {
        if (next === 'active') void refresh().catch(() => {});
      });
      cleanup = () => {
        token.remove();
        state.remove();
        notifications.setNotificationHandler(null);
      };
      void refresh().catch(() => {});
    }).catch(() => {});

    return () => {
      active = false;
      cleanup();
    };
  }, [api]);
}
