import { useEffect } from 'react';
import * as SecureStore from 'expo-secure-store';
import { AppState, Platform } from 'react-native';
import {
  deferAndroidPushRetirement, disableAndroidPushForOrigin, initializeAndroidPushOrigin, removeRetiredDevices, runAndroidDeviceRequest,
  serverStorageKey, withActiveAndroidPushOrigin, type PlanoAPI,
} from './api';
import {
  androidPushStatus, clearLiveActivities, configureLiveActivities, disableAndroidPush, enableAndroidPush,
  type RetiredAndroidPush,
} from './live';

function currentPlatform() {
  return Platform.OS === 'ios' || Platform.OS === 'android' ? Platform.OS : undefined;
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
  if (!enable) {
    if (platform === 'android') {
      await disableAndroidPushForOrigin(api.server);
    } else {
      await api.revokeDevices();
      await SecureStore.deleteItemAsync(preferenceKey(api));
      await clearLiveActivities();
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
  await api.registerDevice({ platform, token: device.data, kind: 'alert' });
  await SecureStore.setItemAsync(preferenceKey(api), 'on');
  await configureLiveActivities(api);
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
          await api.registerDevice({ platform, token, kind: 'alert' });
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
