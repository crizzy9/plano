export interface FlightRefreshScope {
  origin: string;
  flightId?: string;
  relatedFlightId?: string;
}

interface FlightRefreshHint {
  origin: string;
  flightId: string;
  connectionInboundId?: string;
}

const listeners = new Set<(hint: FlightRefreshHint) => void>();

export function validFlightId(id: unknown): id is string {
  return typeof id === 'string' && id.length > 0 && id.length <= 128 &&
    id !== '.' && id !== '..' && !/[^A-Za-z0-9_.-]/.test(id);
}

export function publishFlightRefresh(origin: string, payload: unknown) {
  if (!origin || !payload || typeof payload !== 'object') return;
  const { flightId, connectionInboundId } = payload as Record<string, unknown>;
  if (!validFlightId(flightId) || connectionInboundId !== undefined &&
      (!validFlightId(connectionInboundId) || connectionInboundId === flightId)) return;
  const hint = { origin, flightId, connectionInboundId };
  for (const listener of listeners) listener(hint);
}

export function subscribeFlightRefresh(scope: FlightRefreshScope, refresh: () => void) {
  const matches = (id: string | undefined) => id !== undefined && (id === scope.flightId || id === scope.relatedFlightId);
  const listener = (hint: FlightRefreshHint) => {
    if (scope.origin === hint.origin && (!scope.flightId || matches(hint.flightId) || matches(hint.connectionInboundId))) {
      refresh();
    }
  };
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
