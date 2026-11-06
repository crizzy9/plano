import type { Flight, FlightStatus, Position } from './api';

export const statuses: Record<FlightStatus, string> = {
  scheduled: 'Scheduled', boarding: 'Boarding', taxi_out: 'Taxiing', airborne: 'In flight',
  landed: 'Landed', arrived: 'At the gate', cancelled: 'Cancelled', diverted: 'Diverted',
};

export function flightStatus(status: FlightStatus) {
  return statuses[status] ?? 'Status unavailable';
}

export function departure(flight: Flight) {
  return flight.actualDeparture || flight.estimatedDeparture || flight.scheduledDeparture;
}

export function arrival(flight: Flight) {
  return flight.actualArrival || flight.estimatedArrival || flight.scheduledArrival;
}

export function departureNote(flight: Flight) {
  return flight.actualDeparture ? 'Departed' : flight.estimatedDeparture ? 'Estimated departure' : 'Scheduled departure';
}

export function arrivalNote(flight: Flight) {
  return flight.actualArrival ? 'Arrived' : flight.estimatedArrival ? 'Estimated arrival' : 'Scheduled arrival';
}

export function formatTime(value: string | undefined) {
  const date = new Date(value ?? '');
  if (!Number.isFinite(date.getTime())) return 'Time unavailable';
  return new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(date);
}

export function dateKey(date = new Date()) {
  return [date.getFullYear(), String(date.getMonth() + 1).padStart(2, '0'),
    String(date.getDate()).padStart(2, '0')].join('-');
}

export function formatDay(value: string) {
  const date = new Date(value);
  return Number.isFinite(date.getTime())
    ? new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', weekday: 'short' }).format(date)
    : 'Date unavailable';
}

export function validDate(value: string) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return false;
  const parsed = new Date(`${value}T00:00:00Z`);
  return Number.isFinite(parsed.getTime()) && parsed.toISOString().slice(0, 10) === value;
}

export function isFinished(flight: Flight) {
  return flight.status === 'arrived' || flight.status === 'cancelled' || flight.status === 'diverted';
}

export function nextFlight(flights: Flight[]) {
  return flights.filter((flight) => !isFinished(flight)).sort((a, b) =>
    Date.parse(departure(a)) - Date.parse(departure(b)));
}

export function progress(flight: Flight, now = Date.now()) {
  if (['landed', 'arrived'].includes(flight.status)) return 1;
  if (flight.status !== 'airborne') return 0;
  const from = Date.parse(departure(flight));
  const to = Date.parse(arrival(flight));
  if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) return undefined;
  return Math.min(0.99, Math.max(0, (now - from) / (to - from)));
}

export function pathDistance(points: Position[]) {
  let km = 0;
  const rad = Math.PI / 180;
  for (let index = 1; index < points.length; index += 1) {
    const from = points[index - 1];
    const to = points[index];
    if (!from || !to) continue;
    const sinLat = Math.sin((to.latitude - from.latitude) * rad / 2);
    const sinLng = Math.sin((to.longitude - from.longitude) * rad / 2);
    const h = sinLat ** 2 + Math.cos(from.latitude * rad) * Math.cos(to.latitude * rad) * sinLng ** 2;
    km += 6371 * 2 * Math.asin(Math.sqrt(Math.min(1, h)));
  }
  return km;
}

export const errorText = (error: unknown) => error instanceof Error ? error.message : 'Something went wrong. Try again.';
