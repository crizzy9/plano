import { useEffect } from 'react';
import * as SecureStore from 'expo-secure-store';
import { AppState, Platform } from 'react-native';
import { serverStorageKey, type PlanoAPI } from './api';
import { clearLiveActivities, configureLiveActivities } from './live';

function currentPlatform() {
  return Platform.OS === 'ios' || Platform.OS === 'android' ? Platform.OS : undefined;
}

function preferenceKey(api: PlanoAPI) {
  return serverStorageKey('flight-alerts', api.server);
}

export async function flightAlertsEnabled(api: PlanoAPI) {
  return !!currentPlatform() && (await SecureStore.getItemAsync(preferenceKey(api))) === 'on';
}

export async function changeFlightAlerts(api: PlanoAPI, enable: boolean) {
  const platform = currentPlatform();
  if (!platform) return;
  if (!enable) {
    await api.revokeDevices();
    await SecureStore.deleteItemAsync(preferenceKey(api));
    await clearLiveActivities();
    return;
  }
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

    void import('expo-notifications').then((notifications) => {
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
        if (typeof token !== 'string' || !token || !await flightAlertsEnabled(api) || !active || !platform) return;
        await api.registerDevice({ platform, token, kind: 'alert' });
      }
      async function refresh() {
        if (!active || !await flightAlertsEnabled(api)) return;
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
