import { useCallback, useState } from 'react';
import { Linking, Platform, Share, StyleSheet, Text, View } from 'react-native';
import Svg, { Circle, Path } from 'react-native-svg';
import type { Connection, Flight, ImportSource, PassportSegment, PlanoAPI } from './api';
import { dateKey, errorText, validDate } from './flight';
import { Page, useRemote, type Screen } from './screens';
import { usePalette } from './theme';
import { Action, Body, ErrorMessage, FlightRow, Heading, Input, Loading, layout } from './ui';

export function Tools({ navigation }: Screen<'Tools'>) {
  return (
    <Page>
      <Action label="Import flights" icon="download" onPress={() => navigation.navigate('Import')} />
      <Action label="Connection assistant" icon="plane" secondary onPress={() => navigation.navigate('Connection')} />
      <Action label="Plano Passport" icon="globe" secondary onPress={() => navigation.navigate('Passport')} />
    </Page>
  );
}

export function ImportFlights({ api, navigation }: Screen<'Import'> & { api: PlanoAPI }) {
  const [source, setSource] = useState<ImportSource>('email');
  const [text, setText] = useState('');
  const [date, setDate] = useState('');
  const [result, setResult] = useState<{ flights: Flight[]; unmatched: string[] }>();
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);

  async function submit(fromCalendar = false) {
    if (pending) return;
    setError(undefined);
    setResult(undefined);
    if (!fromCalendar && (!text.trim() || date && !validDate(date))) {
      setError('Enter flight details and use YYYY-MM-DD if you supply a departure date.');
      return;
    }
    setPending(true);
    try {
      if (fromCalendar) {
        const entries = await calendarFlights();
        if (entries.size === 0) {
          setError('No flight numbers found in your calendar titles for the next 30 days.');
          return;
        }
        const flights = new Map<string, Flight>();
        const unmatched: string[] = [];
        for (const [day, codes] of entries) {
          const response = await api.importFlights('calendar', codes.join('\n'), day);
          for (const flight of response.flights) flights.set(flight.id, flight);
          unmatched.push(...response.unmatched);
          setResult({ flights: [...flights.values()], unmatched: [...unmatched] });
        }
      } else {
        setResult(await api.importFlights(source, text, date || undefined));
      }
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setPending(false);
    }
  }

  return (
    <Page>
      <Body>Import flight numbers and departure dates from a calendar, booking email, or TripIt itinerary.</Body>
      {Platform.OS !== 'web' && (
        <>
          <Action label="Read upcoming calendar flights" icon="download" onPress={() => submit(true)} busy={pending} />
          <Body>Plano checks calendar titles on this device and sends matching flight numbers and dates to your server.</Body>
        </>
      )}
      <Heading>Paste flight details</Heading>
      <View style={layout.actions}>
        {(['email', 'tripit', 'calendar'] as const).map((item) => (
          <Action key={item} label={{ email: 'Email', tripit: 'TripIt', calendar: 'Calendar' }[item]}
            secondary={source !== item} onPress={() => setSource(item)} disabled={pending} />
        ))}
      </View>
      <Input label="Flight details" value={text} onChangeText={setText} multiline editable={!pending}
        placeholder="UA123 2026-11-05" style={styles.multiline} maxLength={16000} />
      <Input label="Departure date (optional)" value={date} onChangeText={setDate} editable={!pending}
        placeholder="YYYY-MM-DD" maxLength={10} />
      <Action label="Import flights" onPress={() => submit()} busy={pending} />
      <ErrorMessage message={error} />
      {result && (
        <View style={layout.section}>
          <Heading>Import results</Heading>
          {result.flights.length === 0 && <Body>No flights matched the details provided.</Body>}
          {result.flights.map((flight) => <FlightRow key={flight.id} flight={flight}
            onPress={() => navigation.navigate('Flight', { id: flight.id, number: flight.number, initial: flight })} />)}
          {result.unmatched.map((item, index) => <Body key={index}>Couldn’t match {item}.</Body>)}
        </View>
      )}
    </Page>
  );
}

async function calendarFlights() {
  const calendar = await import('expo-calendar/legacy');
  const permission = await calendar.requestCalendarPermissionsAsync();
  if (!permission.granted) throw new Error('Allow calendar access in device settings to import flight dates.');
  const calendars = await calendar.getCalendarsAsync(calendar.EntityTypes.EVENT);
  const byDate = new Map<string, string[]>();
  if (calendars.length === 0) return byDate;
  const start = new Date();
  const end = new Date(start);
  end.setDate(end.getDate() + 30);
  const events = await calendar.getEventsAsync(calendars.map(({ id }) => id), start, end);
  const entries = new Set<string>();
  for (const event of events) {
    const code = event.title.match(/\b(?:[A-Z]{2,3}|[A-Z][0-9]|[0-9][A-Z])[ -]?\d{1,4}[A-Z]?\b/);
    const departure = new Date(event.startDate);
    if (code && Number.isFinite(departure.getTime())) {
      entries.add(`${code[0].replace(/[ -]/g, '')} ${dateKey(departure)}`);
    }
    if (entries.size >= 12) break;
  }
  for (const entry of entries) {
    const [code, day] = entry.split(' ');
    if (!code || !day) continue;
    const codes = byDate.get(day) ?? [];
    codes.push(code);
    byDate.set(day, codes);
  }
  return byDate;
}

export function ConnectionScreen({ api }: { api: PlanoAPI }) {
  const { colors } = usePalette();
  const load = useCallback(() => api.tracked(), [api]);
  const { value, error, pending, refresh } = useRemote(load);
  const [inbound, setInbound] = useState<Flight>();
  const [outbound, setOutbound] = useState<Flight>();
  const [result, setResult] = useState<Connection>();
  const [busy, setBusy] = useState(false);
  const [issue, setIssue] = useState<string>();

  async function check() {
    if (!inbound || !outbound || busy) return;
    setBusy(true);
    setIssue(undefined);
    setResult(undefined);
    try {
      setResult(await api.connection(inbound.id, outbound.id));
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      setBusy(false);
    }
  }

  const candidates = value?.filter((flight) => flight.id !== inbound?.id && flight.origin === inbound?.destination);
  return (
    <Page>
      <Body>Choose two tracked flights that connect at the same airport.</Body>
      <ErrorMessage message={error} />
      {error && <Action secondary label="Refresh flights" onPress={refresh} />}
      {pending && !value && <Loading />}
      <Heading>Arriving flight</Heading>
      {value?.length === 0 && <Body>Track flights before checking a connection.</Body>}
      {inbound ? (
        <FlightRow flight={inbound} onPress={() => {
          if (busy) return;
          setInbound(undefined);
          setOutbound(undefined);
          setResult(undefined);
        }} />
      ) : value?.map((flight) => <FlightRow key={flight.id} flight={flight} onPress={() => setInbound(flight)} />)}
      {inbound && (
        <>
          <Heading>Departing flight</Heading>
          {candidates?.length === 0 && <Body>No tracked flights depart from {inbound.destination}.</Body>}
          {candidates?.map((flight) => (
            <Action key={flight.id} label={`${flight.number} to ${flight.destination}`}
              secondary={flight.id !== outbound?.id} disabled={busy} onPress={() => { setOutbound(flight); setResult(undefined); }} />
          ))}
          {outbound && <Action label="Check connection" onPress={check} busy={busy} />}
        </>
      )}
      <ErrorMessage message={issue} />
      {result && (
        <View style={layout.section}>
          <Heading>{result.risk} connection at {result.airport}</Heading>
          <Text style={[styles.detail, { color: colors.text }]}>{result.availableMinutes} min between flights · {result.transferMinutes} min estimated transfer</Text>
          {result.instructions?.map((instruction, index) => <Body key={index}>{instruction}</Body>)}
          <Action secondary label={`Open ${result.airport} in maps`} onPress={() => {
            const place = encodeURIComponent(`${result.airport} airport`);
            const url = Platform.OS === 'ios' ? `https://maps.apple.com/?q=${place}`
              : `https://www.google.com/maps/search/?api=1&query=${place}`;
            Linking.openURL(url).catch((cause: unknown) => setIssue(errorText(cause)));
          }} />
        </View>
      )}
    </Page>
  );
}

export function PassportScreen({ api, navigation }: Screen<'Passport'> & { api: PlanoAPI }) {
  const load = useCallback(() => api.passport(), [api]);
  const { value, error, pending, refresh } = useRemote(load);
  const [issue, setIssue] = useState<string>();
  const formatter = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
  const stats = value?.stats;
  const summary = stats
    ? `${formatter.format(stats.flights)} flights · ${formatter.format(stats.miles)} mi · ${formatter.format(stats.hours)} hr`
    : '';
  return (
    <Page refreshing={pending && !!value} refresh={refresh}>
      <Heading>Flight history</Heading>
      <ErrorMessage message={error || issue} />
      {error && <Action secondary label="Reload Passport" onPress={refresh} />}
      {pending && !value && <Loading />}
      {value && value.flights.length === 0 && <Body>Completed flights will appear here.</Body>}
      {!!value?.flights.length && stats && (
        <>
          <Body>{summary}</Body>
          <Body>{stats.aircraft} aircraft · {formatter.format(stats.delayMinutes)} min of delays</Body>
          {value.segments.length > 0 && <PassportGlobe segments={value.segments} />}
          <Action secondary icon="share" label="Share flight stats" onPress={() => {
            Share.share({ message: `My Plano Passport\n${summary}\n${stats.aircraft} aircraft · ${formatter.format(stats.delayMinutes)} min of delays` })
              .catch((cause: unknown) => setIssue(errorText(cause)));
          }} />
          {value.flights.map((flight) => <FlightRow key={flight.id} flight={flight}
            onPress={() => navigation.navigate('Flight', { id: flight.id, number: flight.number, initial: flight })} />)}
        </>
      )}
    </Page>
  );
}

const rad = Math.PI / 180;
const diameter = 216;
const center = diameter / 2;

function globeLine(points: [number, number][], viewLongitude: number): string {
  let path = '';
  let visible = false;
  for (const [lat, lon] of points) {
    const latitude = lat * rad;
    const longitude = (lon - viewLongitude) * rad;
    const x = center + (center - 4) * Math.cos(latitude) * Math.sin(longitude);
    const y = center - (center - 4) * Math.sin(latitude);
    const front = Math.cos(latitude) * Math.cos(longitude) >= 0;
    if (front) path += `${visible ? 'L' : 'M'}${x.toFixed(2)} ${y.toFixed(2)}`;
    visible = front;
  }
  return path;
}

function greatCircle(segment: PassportSegment): [number, number][] {
  const { originLatitude, originLongitude, destinationLatitude, destinationLongitude } = segment;
  const vector = (lat: number, lon: number) => [
    Math.cos(lat * rad) * Math.cos(lon * rad), Math.cos(lat * rad) * Math.sin(lon * rad), Math.sin(lat * rad),
  ] as const;
  const [ax, ay, az] = vector(originLatitude, originLongitude);
  const [bx, by, bz] = vector(destinationLatitude, destinationLongitude);
  const angle = Math.acos(Math.max(-1, Math.min(1, ax * bx + ay * by + az * bz)));
  const divisor = Math.sin(angle);
  if (Math.abs(divisor) < 0.000001) return [[originLatitude, originLongitude], [destinationLatitude, destinationLongitude]];
  return Array.from({ length: 33 }, (_, index) => {
    const portion = index / 32;
    const a = Math.sin((1 - portion) * angle) / divisor;
    const b = Math.sin(portion * angle) / divisor;
    const x = a * ax + b * bx;
    const y = a * ay + b * by;
    const z = a * az + b * bz;
    return [Math.atan2(z, Math.hypot(x, y)) / rad, Math.atan2(y, x) / rad];
  });
}

function PassportGlobe({ segments }: { segments: PassportSegment[] }) {
  const { colors } = usePalette();
  const [longitude, setLongitude] = useState(segments[0]?.originLongitude ?? 0);
  const parallels = [-60, -30, 0, 30, 60].map((lat) =>
    Array.from({ length: 91 }, (_, index): [number, number] => [lat, -180 + 4 * index]));
  const meridians = [-180, -120, -60, 0, 60, 120, 180].map((lon) =>
    Array.from({ length: 46 }, (_, index): [number, number] => [-90 + 4 * index, lon]));
  return (
    <View style={styles.globe}>
      <Svg width="100%" height={236} viewBox={`0 0 ${diameter} ${diameter}`}
        accessibilityRole="image" accessibilityLabel="Globe showing your recorded routes">
        <Circle cx={center} cy={center} r={center - 4} fill={colors.route} />
        {[...parallels, ...meridians].map((points, index) => <Path key={index} d={globeLine(points, longitude)}
          stroke="#547A6E" strokeWidth={0.75} fill="none" />)}
        {segments.map((segment, index) => <Path key={index} d={globeLine(greatCircle(segment), longitude)}
          stroke={colors.routeLine} strokeWidth={1.7} fill="none" strokeLinecap="round" />)}
      </Svg>
      <View style={layout.actions}>
        <Action secondary label="Rotate west" onPress={() => setLongitude((old) => (old - 30 + 360) % 360)} />
        <Action secondary label="Rotate east" onPress={() => setLongitude((old) => (old + 30) % 360)} />
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  multiline: { minHeight: 120, textAlignVertical: 'top' },
  detail: { fontSize: 16, lineHeight: 22 },
  globe: { alignItems: 'center', gap: 8 },
});
