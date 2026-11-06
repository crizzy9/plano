import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { NavigationContainerRefWithCurrent } from '@react-navigation/native';
import type { NotificationResponse } from 'expo-notifications';
import { Linking, Platform } from 'react-native';
import { createAPI, loadServer, type Flight } from './api';
import type { Routes } from './screens';

interface AlertRoute {
  flightId: string;
  connectionInboundId?: string;
}

const notificationPrefix = 'plano://notification/';

function validID(id: unknown): id is string {
  return typeof id === 'string' && id.length > 0 && id.length <= 128 &&
    id !== '.' && id !== '..' && !/[^A-Za-z0-9_.-]/.test(id);
}

export function notificationRouteFromURL(url: string | null): AlertRoute | undefined {
  if (!url?.startsWith(notificationPrefix)) return;
  const segments = url.slice(notificationPrefix.length).split('/');
  const [kind, flightId, connectionInboundId] = segments;
  if (!validID(flightId)) return;
  if (kind === 'flight' && segments.length === 2) return { flightId };
  if (kind === 'connection' && segments.length === 3 && validID(connectionInboundId) && connectionInboundId !== flightId) {
    return { flightId, connectionInboundId };
  }
}

export function notificationRouteFromResponse(response: NotificationResponse, defaultAction: string): AlertRoute | undefined {
  if (response.actionIdentifier !== defaultAction) return;
  const trigger = response.notification.request.trigger;
  if (!trigger || !('type' in trigger) || trigger.type !== 'push') return;
  // Direct APNs custom keys are in userInfo, exposed here by Expo.
  const flightId = trigger.payload?.flightId;
  const connectionInboundId = trigger.payload?.connectionInboundId;
  if (!validID(flightId)) return;
  if (connectionInboundId === undefined) return { flightId };
  if (validID(connectionInboundId) && connectionInboundId !== flightId) return { flightId, connectionInboundId };
}

export function notificationDestination(route: AlertRoute, tracked: Flight[]) {
  const flight = tracked.find((candidate) => candidate.id === route.flightId);
  if (!flight) return;
  const inbound = route.connectionInboundId && tracked.find((candidate) => candidate.id === route.connectionInboundId);
  if (inbound && (inbound.actualArrival || inbound.estimatedArrival || inbound.scheduledArrival) &&
    (flight.actualDeparture || flight.estimatedDeparture || flight.scheduledDeparture)) {
    const sameAirport = inbound.destinationICAO && flight.originICAO
      ? inbound.destinationICAO === flight.originICAO
      : !!inbound.destination && inbound.destination === flight.origin;
    if (sameAirport) return { name: 'Connection' as const, params: { inbound: inbound.id, outbound: flight.id } };
  }
  return { name: 'Flight' as const, params: { id: flight.id, number: flight.number, initial: flight } };
}

interface PendingTap {
  route: AlertRoute;
  origin: string | undefined;
}

export function useNotificationRouting(
  server: string | undefined,
  ready: boolean,
  navigation: NavigationContainerRefWithCurrent<Routes>,
) {
  const origin = useRef(server);
  const firstOrigin = useRef(server);
  useLayoutEffect(() => {
    origin.current = server;
    firstOrigin.current ??= server;
  }, [server]);
  const latest = useRef<PendingTap | undefined>(undefined);
  const [pending, setPending] = useState<PendingTap>();

  useEffect(() => {
    if (Platform.OS !== 'android' && Platform.OS !== 'ios') return;
    let active = true;
    let received = false;
    let removeResponses: (() => void) | undefined;
    function receive(route: AlertRoute | undefined, sourceOrigin: string | undefined) {
      if (!active || !route) return;
      received = true;
      latest.current = { route, origin: sourceOrigin };
      setPending(latest.current);
    }

    const urls = Linking.addEventListener('url', ({ url }) => receive(notificationRouteFromURL(url), origin.current));
    void Linking.getInitialURL().then((url) => {
      if (!received) receive(notificationRouteFromURL(url), firstOrigin.current);
    }).catch(() => {});

    if (Platform.OS === 'ios') {
      void import('expo-notifications').then((notifications) => {
        if (!active) return;
        let handled: string | undefined;
        const identity = ({ notification }: NotificationResponse) => `${notification.request.identifier}\u0000${notification.date}`;
        function onResponse(response: NotificationResponse, initial = false) {
          const route = notificationRouteFromResponse(response, notifications.DEFAULT_ACTION_IDENTIFIER);
          if (!route) return;
          const id = identity(response);
          const last = notifications.getLastNotificationResponse();
          if (last && identity(last) === id && last.actionIdentifier === response.actionIdentifier) {
            notifications.clearLastNotificationResponse();
          }
          if (id === handled) return;
          handled = id;
          if (!initial || !received) receive(route, initial ? firstOrigin.current : origin.current);
        }
        const responses = notifications.addNotificationResponseReceivedListener(onResponse);
        removeResponses = () => responses.remove();
        const last = notifications.getLastNotificationResponse();
        if (last) onResponse(last, true);
      }).catch(() => {});
    }

    return () => {
      active = false;
      urls.remove();
      removeResponses?.();
    };
  }, []);

  useEffect(() => {
    if (!ready || !pending) return;
    const discard = () => {
      if (latest.current === pending) latest.current = undefined;
      setPending((tap) => tap === pending ? undefined : tap);
    };
    const sourceOrigin = pending.origin ?? firstOrigin.current;
    if (!server || sourceOrigin !== server) {
      discard();
      return;
    }
    const tap = pending;
    const serverAtTap = server;
    let active = true;
    async function navigate() {
      const { flightId, connectionInboundId } = tap.route;
      const ids = connectionInboundId ? [flightId, connectionInboundId] : [flightId];
      const tracked = await createAPI(serverAtTap, { access: 'existing-installation' }).tracked(ids);
      const currentServer = await loadServer();
      if (!active || latest.current !== tap || origin.current !== serverAtTap || currentServer !== serverAtTap || !navigation.isReady()) return;
      const destination = notificationDestination(tap.route, tracked);
      if (destination?.name === 'Connection') navigation.navigate('Connection', destination.params);
      else if (destination?.name === 'Flight') navigation.navigate('Flight', destination.params);
    }
    void navigate().catch(() => {}).finally(() => { if (active) discard(); });
    return () => { active = false; };
  }, [navigation, pending, ready, server]);
}
