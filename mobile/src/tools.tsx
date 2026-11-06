import { useCallback, useEffect, useState } from 'react';
import { Linking, Platform, StyleSheet, Text, View } from 'react-native';
import type { Connection, Flight, ImportResult, ImportSource, PlanoAPI } from './api';
import { errorText, validDate } from './flight';
import { Page, useRemote, type Screen } from './screens';
import { usePalette } from './theme';
import { calendarFlights, calendarSyncEnabled, setCalendarSyncEnabled } from './calendar-sync';
import { PassportCard } from './passport-card';
import { PassportMaps } from './passport-maps';
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
  const { colors: palette } = usePalette();
  const [calendarEnabled, setCalendarEnabled] = useState(false);
  useEffect(() => {
    let active = true;
    setCalendarEnabled(false);
    calendarSyncEnabled(api.server).then((enabled) => {
      if (active) setCalendarEnabled(enabled);
    }).catch(() => {});
    return () => { active = false; };
  }, [api.server]);
  const [source, setSource] = useState<ImportSource>('email');
  const [text, setText] = useState('');
  const [date, setDate] = useState('');
  const [result, setResult] = useState<ImportResult>();
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);

  const [tripItURL, setTripItURL] = useState('');
  const [tripItOffset, setTripItOffset] = useState(0);
  const [locator, setLocator] = useState('');
  const [relay, setRelay] = useState<{ url: string; token: string }>();
  const [notice, setNotice] = useState<string>();

  async function runImport(action: () => Promise<void>) {
    if (pending) return;
    setError(undefined);
    setNotice(undefined);
    setPending(true);
    try {
      await action();
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setPending(false);
    }
  }

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
          for (let offset = 0; offset < codes.length; offset += 8) {
            const response = await api.importFlights('calendar', codes.slice(offset, offset + 8).join('\n'), day);
            for (const flight of response.flights) flights.set(flight.id, flight);
            unmatched.push(...response.unmatched);
            setResult({ flights: [...flights.values()], unmatched: [...unmatched] });
          }
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
          <Action label={calendarEnabled ? 'Turn off automatic calendar sync' : 'Turn on automatic calendar sync'}
            secondary disabled={pending || !api.server} onPress={() => void runImport(async () => {
              const next = !calendarEnabled;
              await setCalendarSyncEnabled(api.server, next);
              setCalendarEnabled(next);
            })} />
          <Body>With automatic sync on, your device can add matching flights in the background when the operating system permits.</Body>
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
      <Heading>TripIt calendar feed</Heading>
      <Body>Import from the private calendar feed in your TripIt account settings.</Body>
      <Input label="TripIt calendar URL" value={tripItURL} onChangeText={(value) => { setTripItURL(value); setTripItOffset(0); }}
        editable={!pending} autoCapitalize="none" autoCorrect={false} secureTextEntry maxLength={1024} />
      <Action label={tripItOffset ? 'Import next TripIt flights' : 'Import TripIt feed'} secondary disabled={pending || !tripItURL.trim()}
        onPress={() => void runImport(async () => {
          const next = await api.importTripIt(tripItURL.trim(), tripItOffset);
          if (tripItOffset && result) {
            const flights = new Map([...result.flights, ...next.flights].map((flight) => [flight.id, flight]));
            setResult({ ...next, flights: [...flights.values()], unmatched: [...new Set([...result.unmatched, ...next.unmatched])] });
          } else {
            setResult(next);
          }
          setTripItOffset(next.nextOffset ?? 0);
          if (next.nextOffset === undefined) setTripItURL('');
        })} />
      <Heading>Booking lookup</Heading>
      <Body>Find flights already imported on this server using your confirmation code.</Body>
      <Input label="Confirmation code" value={locator} onChangeText={setLocator}
        autoCapitalize="characters" autoCorrect={false} editable={!pending} maxLength={6} />
      <Action label="Find imported booking" secondary disabled={pending || !/^[a-z0-9]{6}$/i.test(locator)}
        onPress={() => void runImport(async () => {
          setResult(undefined);
          const booking = await api.lookupBooking(locator);
          setResult({ flights: booking.flights, unmatched: [] });
          setLocator('');
        })} />
      <Heading>Email relay</Heading>
      <Body>Connect a mail relay to import forwarded booking emails. Keep its token private.</Body>
      <View style={layout.actions}>
        <Action label="Generate relay token" secondary disabled={pending}
          onPress={() => void runImport(async () => setRelay(await api.createEmailRelay()))} />
        <Action label="Revoke relay" secondary disabled={pending}
          onPress={() => void runImport(async () => {
            await api.revokeEmailRelay();
            setRelay(undefined);
            setNotice('Email relay revoked.');
          })} />
      </View>
      {relay && (
        <View style={layout.section}>
          <Text selectable style={{ color: palette.text }}>{relay.url}</Text>
          <Text selectable style={{ color: palette.text }}>{relay.token}</Text>
          <Action label="Hide relay token" secondary onPress={() => setRelay(undefined)} />
        </View>
      )}
      {notice && <Body>{notice}</Body>}
      <ErrorMessage message={error} />
      {result && (
        <View style={layout.section}>
          <Heading>Import results</Heading>
          {result.truncated && <Body>Some entries exceeded the import limit. Split the itinerary and import the remaining flights separately.</Body>}
          {result.nextOffset !== undefined && <Body>The TripIt feed has more flights to import.</Body>}
          {result.flights.length === 0 && <Body>No flights matched the details provided.</Body>}
          {result.flights.map((flight) => <FlightRow key={flight.id} flight={flight}
            onPress={() => navigation.navigate('Flight', { id: flight.id, number: flight.number, initial: flight })} />)}
          {result.unmatched.map((item, index) => <Body key={index}>Couldn’t match {item}.</Body>)}
        </View>
      )}
    </Page>
  );
}

const transferDetail: Record<string, string> = {
  gates: 'Gate-to-gate transfer estimate.',
  mixed: 'Transfer estimate based on the available gate and terminal.',
  terminals: 'Terminal-to-terminal transfer estimate.',
  generic: 'General transfer estimate.',
};

function configuredAirportMap(value?: string) {
  if (!value || /[\\\s]/.test(value)) return undefined;
  try {
    const url = new URL(value);
    return url.protocol === 'https:' && url.hostname && !url.username && !url.password ? value : undefined;
  } catch {
    return undefined;
  }
}

function ConnectionGate({ direction, reported, prediction }: {
  direction: 'arrival' | 'departure';
  reported?: string;
  prediction?: Connection['arrivalGatePrediction'];
}) {
  if (reported) return <Body>Reported {direction} gate {reported}</Body>;
  if (!prediction) return null;
  const share = new Intl.NumberFormat(undefined, { style: 'percent' }).format(prediction.share);
  return (
    <View>
      <Body>Estimated {direction} gate {prediction.gate}</Body>
      <Body>This gate was used in {share} of {prediction.samples} comparable flights.</Body>
    </View>
  );
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
  const airportMap = configuredAirportMap(result?.mapUrl);
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
          {result.precision && transferDetail[result.precision] && <Body>{transferDetail[result.precision]}</Body>}
          <ConnectionGate direction="arrival" reported={inbound?.arrivalGate} prediction={result.arrivalGatePrediction} />
          <ConnectionGate direction="departure" reported={outbound?.departureGate} prediction={result.departureGatePrediction} />
          {result.instructions?.map((instruction, index) => <Body key={index}>{instruction}</Body>)}
          <Action secondary label={airportMap ? `Open ${result.airport} terminal map` : `Open ${result.airport} in maps`} onPress={() => {
            const place = encodeURIComponent(`${result.airport} airport`);
            const fallback = Platform.OS === 'ios' ? `https://maps.apple.com/?q=${place}`
              : `https://www.google.com/maps/search/?api=1&query=${place}`;
            Linking.openURL(airportMap ?? fallback).catch((cause: unknown) => setIssue(errorText(cause)));
          }} />
        </View>
      )}
    </Page>
  );
}

export function PassportScreen({ api, navigation }: Screen<'Passport'> & { api: PlanoAPI }) {
  const load = useCallback(() => api.passport(), [api]);
  const { value, error, pending, refresh } = useRemote(load);
  return (
    <Page refreshing={pending && !!value} refresh={refresh}>
      <Heading>Flight history</Heading>
      <ErrorMessage message={error} />
      {error && <Action secondary label="Reload Passport" onPress={refresh} />}
      {pending && !value && <Loading />}
      {value && value.flights.length === 0 && <Body>Completed flights will appear here.</Body>}
      {!!value?.flights.length && (
        <>
          <PassportMaps segments={value.segments} />
          <PassportCard stats={value.stats} />
          <View style={layout.section}>
            <Heading>Completed flights</Heading>
            {value.flights.map((flight) => <FlightRow key={flight.id} flight={flight}
              onPress={() => navigation.navigate('Flight', { id: flight.id, number: flight.number, initial: flight })} />)}
          </View>
        </>
      )}
    </Page>
  );
}

const styles = StyleSheet.create({
  multiline: { minHeight: 120, textAlignVertical: 'top' },
  detail: { fontSize: 16, lineHeight: 22 },
});
