import { useCallback, useEffect, useRef, useState } from 'react';
import { Linking, Platform, StyleSheet, Text, View } from 'react-native';
import type { Connection, Flight, ImportResult, ImportSource, PartnerBooking, Passport, PlanoAPI } from './api';
import { errorText, formatDay, validDate } from './flight';
import { Page, useRemote, type Routes, type Screen } from './screens';
import { usePalette } from './theme';
import { calendarFlights, calendarSyncEnabled, setCalendarSyncEnabled } from './calendar-sync';
import { PassportCard } from './passport-card';
import { PassportMaps } from './passport-maps';
import { TripItSync } from './tripit-sync';
import { ReservationLookup } from './reservation-lookup';
import { Action, Body, ErrorMessage, FlightRow, Heading, Input, Loading, layout } from './ui';

export function Tools({ navigation }: Screen<'Tools'>) {
  return (
    <Page>
      <Action label="Import flights" icon="download" onPress={() => navigation.navigate('Import')} />
      <Action label="Connection assistant" icon="plane" secondary onPress={() => navigation.navigate('Connection')} />
      <Action label="Plano Passport" icon="globe" secondary onPress={() => navigation.navigate('Passport')} />
      <Action label="Friends" icon="share" secondary onPress={() => navigation.navigate('Friends')} />
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
  const importActive = useRef(false);

  const [tripItURL, setTripItURL] = useState('');
  const [tripItOffset, setTripItOffset] = useState(0);
  const tripItPage = useRef<ImportResult>(undefined);
  const [locator, setLocator] = useState('');
  const [relay, setRelay] = useState<{ url: string; token: string }>();
  const [notice, setNotice] = useState<string>();

  async function runImport(action: () => Promise<void>) {
    if (importActive.current) return;
    importActive.current = true;
    setError(undefined);
    setNotice(undefined);
    setPending(true);
    try {
      await action();
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      importActive.current = false;
      setPending(false);
    }
  }

  async function submit(fromCalendar = false) {
    if (importActive.current) return;
    setError(undefined);
    setResult(undefined);
    if (!fromCalendar && (!text.trim() || date && !validDate(date))) {
      setError('Enter flight details and use YYYY-MM-DD if you supply a departure date.');
      return;
    }
    importActive.current = true;
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
      importActive.current = false;
      setPending(false);
    }
  }

  async function resolvePartnerBooking(details: PartnerBooking) {
    if (importActive.current) throw new Error('Wait for the current import to finish.');
    importActive.current = true;
    setError(undefined);
    setNotice(undefined);
    setResult(undefined);
    setPending(true);
    try {
      const booking = await api.resolveBooking(details);
      setResult(booking);
    } finally {
      importActive.current = false;
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
      <Heading>Import a TripIt feed once</Heading>
      <Body>Copy the private URL from Calendar Feed in your TripIt account settings.</Body>
      <Input label="TripIt calendar URL" value={tripItURL} onChangeText={(value) => {
        setTripItURL(value);
        setTripItOffset(0);
        tripItPage.current = undefined;
      }} editable={!pending} autoComplete="off" autoCapitalize="none" autoCorrect={false} secureTextEntry maxLength={1024} />
      <Action label={tripItOffset ? 'Import next TripIt flights' : 'Import TripIt feed'} secondary disabled={pending || !tripItURL.trim()}
        onPress={() => void runImport(async () => {
          const next = await api.importTripIt(tripItURL.trim(), tripItOffset);
          const previous = tripItOffset ? tripItPage.current : undefined;
          const combined = previous ? {
            ...next,
            flights: [...new Map([...previous.flights, ...next.flights].map((flight) => [flight.id, flight])).values()],
            unmatched: [...new Set([...previous.unmatched, ...next.unmatched])],
          } : next;
          setResult(combined);
          setTripItOffset(next.nextOffset ?? 0);
          tripItPage.current = next.nextOffset === undefined ? undefined : combined;
          if (next.nextOffset === undefined) setTripItURL('');
        })} />
      <TripItSync api={api} disabled={pending} />
      <Heading>Find an imported booking</Heading>
      <Body>Use your confirmation code to find flights already imported on this server.</Body>
      <Input label="Confirmation code" value={locator} onChangeText={setLocator}
        autoCapitalize="characters" autoCorrect={false} editable={!pending} maxLength={6} />
      <Action label="Find imported booking" secondary disabled={pending || !/^[a-z0-9]{6}$/i.test(locator)}
        onPress={() => void runImport(async () => {
          setResult(undefined);
          const booking = await api.lookupBooking(locator);
          setResult({ flights: booking.flights, unmatched: [] });
          setLocator('');
        })} />
      <ReservationLookup disabled={pending} resolve={resolvePartnerBooking} />
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

export function ConnectionScreen({ api, initialPair }: { api: PlanoAPI; initialPair?: Routes['Connection'] }) {
  const initialInbound = initialPair?.inbound;
  const initialOutbound = initialPair?.outbound;
  const load = useCallback(async () => {
    const flights = await api.tracked();
    if (!initialInbound || !initialOutbound) return flights;
    const missing = [initialInbound, initialOutbound].filter((id) => !flights.some((flight) => flight.id === id));
    if (!missing.length) return flights;
    const selected = await api.tracked(missing);
    return [...flights, ...selected.filter((flight) => missing.includes(flight.id))];
  }, [api, initialInbound, initialOutbound]);
  const { value, error, pending, refresh } = useRemote(load);
  const loadMonitors = useCallback(() => api.connectionMonitors(), [api]);
  const { value: monitors, error: monitorError, refresh: refreshMonitors } = useRemote(loadMonitors, !!api.server, 30_000);
  const [inboundID, setInboundID] = useState(initialPair?.inbound);
  const [outboundID, setOutboundID] = useState(initialPair?.outbound);
  const [checked, setChecked] = useState(!!initialPair);
  const [monitorBusy, setMonitorBusy] = useState(false);
  const [monitorIssue, setMonitorIssue] = useState<string>();
  const monitorRequest = useRef<symbol | undefined>(undefined);
  useEffect(() => () => { monitorRequest.current = undefined; }, [api]);
  useEffect(() => {
    if (!initialPair) return;
    setInboundID(initialPair.inbound);
    setOutboundID(initialPair.outbound);
    setChecked(true);
  }, [initialPair]);

  async function changeMonitoring(inbound: string, outbound: string, enabled: boolean, reportInList = false) {
    if (monitorRequest.current) return;
    const request = Symbol();
    monitorRequest.current = request;
    setMonitorBusy(true);
    setMonitorIssue(undefined);
    try {
      return await api.setConnectionMonitoring(inbound, outbound, enabled);
    } catch (cause) {
      if (!reportInList) throw cause;
      if (monitorRequest.current === request) setMonitorIssue(errorText(cause));
    } finally {
      if (monitorRequest.current === request) {
        monitorRequest.current = undefined;
        setMonitorBusy(false);
        refreshMonitors();
      }
    }
  }

  const tracked = new Map(value?.map((flight) => [flight.id, flight]));
  const inbound = inboundID ? tracked.get(inboundID) : undefined;
  const outbound = outboundID ? tracked.get(outboundID) : undefined;

  function select(inbound?: string, outbound?: string, assess = false) {
    if (monitorRequest.current) return;
    setInboundID(inbound);
    setOutboundID(outbound);
    setChecked(assess);
  }

  const candidates = inbound && value?.filter((flight) => {
    if (flight.id === inbound.id) return false;
    if (inbound.destinationICAO && flight.originICAO) return inbound.destinationICAO === flight.originICAO;
    return !!inbound.destination && flight.origin === inbound.destination;
  });
  return (
    <Page>
      <Body>Choose two tracked flights that connect at the same airport.</Body>
      <ErrorMessage message={error} />
      {error && <Action secondary label="Refresh flights" onPress={refresh} />}
      {pending && !value && <Loading />}
      {monitorError && (
        <View style={layout.section}>
          <ErrorMessage message={`Couldn’t load monitored connections. ${monitorError}`} />
          <Action secondary label="Retry monitored connections" onPress={refreshMonitors} />
        </View>
      )}
      <ErrorMessage message={monitorIssue} />
      {value && !!monitors?.length && (
        <View style={layout.section}>
          <Heading>Monitored connections</Heading>
          {monitors.map((monitor) => {
            const arriving = tracked.get(monitor.inbound);
            const departing = tracked.get(monitor.outbound);
            const key = `${monitor.inbound}:${monitor.outbound}`;
            if (!arriving || !departing) return (
              <View key={key} style={layout.section}>
                <Body>{arriving?.number ?? 'Unavailable flight'} to {departing?.number ?? 'unavailable flight'} · {monitor.risk}</Body>
                <Action secondary disabled={monitorBusy} label="Stop monitoring"
                  onPress={() => { void changeMonitoring(monitor.inbound, monitor.outbound, false, true); }} />
              </View>
            );
            return (
              <Action key={key} secondary disabled={monitorBusy}
                label={`${arriving.number} to ${departing.number} · ${formatDay(arriving.scheduledDeparture, arriving.originTimeZone)} · ${monitor.risk}`}
                onPress={() => select(arriving.id, departing.id, true)} />
            );
          })}
        </View>
      )}
      <Heading>Arriving flight</Heading>
      {value?.length === 0 && <Body>Track flights before checking a connection.</Body>}
      {inbound ? (
        <FlightRow flight={inbound} onPress={() => select()} />
      ) : value?.map((flight) => <FlightRow key={flight.id} flight={flight} onPress={() => select(flight.id)} />)}
      {inbound && (
        <>
          <Heading>Departing flight</Heading>
          {candidates?.length === 0 && <Body>No tracked flights depart from {inbound.destination}.</Body>}
          {candidates?.map((flight) => (
            <Action key={flight.id} label={`${flight.number} to ${flight.destination}`}
              secondary={flight.id !== outbound?.id} disabled={monitorBusy} onPress={() => select(inbound.id, flight.id)} />
          ))}
          {outbound && !checked && <Action label="Check connection" onPress={() => select(inbound.id, outbound.id, true)} />}
          {outbound && checked && (
            <CheckedConnection key={`${inbound.id}:${outbound.id}`} api={api} inbound={inbound} outbound={outbound}
              monitorBusy={monitorBusy} changeMonitoring={changeMonitoring} />
          )}
        </>
      )}
    </Page>
  );
}

function CheckedConnection({ api, inbound, outbound, monitorBusy, changeMonitoring }: {
  api: PlanoAPI;
  inbound: Flight;
  outbound: Flight;
  monitorBusy: boolean;
  changeMonitoring: (inbound: string, outbound: string, enabled: boolean) => Promise<boolean | undefined>;
}) {
  const { colors } = usePalette();
  const inboundID = inbound.id;
  const outboundID = outbound.id;
  const load = useCallback(() => api.connection(inboundID, outboundID), [api, inboundID, outboundID]);
  const { value: result, setValue: setResult, error, pending, refresh } = useRemote(load, true, 30_000);
  const [issue, setIssue] = useState<string>();
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  async function toggleMonitoring() {
    if (!result || monitorBusy) return;
    setIssue(undefined);
    try {
      const monitoring = await changeMonitoring(inboundID, outboundID, !result.monitoring);
      if (mounted.current && monitoring !== undefined) {
        setResult((previous) => previous && { ...previous, monitoring });
      }
    } catch (cause) {
      if (mounted.current) setIssue(errorText(cause));
    } finally {
      if (mounted.current) refresh();
    }
  }

  const airportMap = configuredAirportMap(result?.mapUrl);
  return (
    <View style={layout.section}>
      <ErrorMessage message={error} />
      {error && <Action secondary label="Recheck connection" onPress={refresh} busy={pending} disabled={monitorBusy} />}
      {!result && !error && <Loading />}
      {result && (
        <>
          <Heading>{result.risk} connection at {result.airport}</Heading>
          <Text style={[styles.detail, { color: colors.text }]}>{Math.max(0, result.availableMinutes)} min connection window · {result.transferMinutes} min estimated transfer</Text>
          {(result.boardingReserveMinutes ?? 0) > 0 && <Body>Timing includes an estimated {result.boardingReserveMinutes} min boarding reserve.</Body>}
          {result.precision && transferDetail[result.precision] && <Body>{transferDetail[result.precision]}</Body>}
          {result.forecastTransferMinutes !== undefined && <Body>Historical gates suggest a {result.forecastTransferMinutes} min airport route. The risk estimate allows for whichever route requires more time, including boarding.</Body>}
          <ConnectionGate direction="arrival" reported={result.arrivalGate ?? inbound.arrivalGate} prediction={result.arrivalGatePrediction} />
          <ConnectionGate direction="departure" reported={result.departureGate ?? outbound.departureGate} prediction={result.departureGatePrediction} />
          {result.instructions?.map((instruction, index) => <Body key={index}>{instruction}</Body>)}
          <Action secondary label={airportMap ? `Open ${result.airport} terminal map` : `Open ${result.airport} in maps`} onPress={() => {
            const place = encodeURIComponent(`${result.airport} airport`);
            const fallback = Platform.OS === 'ios' ? `https://maps.apple.com/?q=${place}`
              : `https://www.google.com/maps/search/?api=1&query=${place}`;
            Linking.openURL(airportMap ?? fallback).catch((cause: unknown) => setIssue(errorText(cause)));
          }} />
          {(result.monitoring || result.monitorEligible) && (
            <>
              <Heading>Connection alerts</Heading>
              <Body>{result.monitoring ? 'Plano is monitoring this connection for worsening risk.' : 'Monitor this connection to get an alert if its risk worsens.'}</Body>
              {Platform.OS !== 'web' && <Body>To receive notifications on this device, enable Flight alerts in Plano Settings and allow notifications for Plano.</Body>}
              <Action icon="bell" secondary busy={monitorBusy} label={result.monitoring ? 'Stop monitoring' : 'Monitor connection'}
                onPress={toggleMonitoring} />
            </>
          )}
          <ErrorMessage message={issue} />
        </>
      )}
    </View>
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
          {value.coverage && <PassportCoverage coverage={value.coverage} refresh={refresh} pending={pending} />}
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

function PassportCoverage({ coverage, refresh, pending }: {
  coverage: NonNullable<Passport['coverage']>;
  refresh: () => void;
  pending: boolean;
}) {
  const details = [
    [coverage.missingCoordinatesFlights, 'airport coordinates'],
    [coverage.missingDistanceFlights, 'distance'],
    [coverage.missingDurationFlights, 'flight time'],
    [coverage.missingDelayFlights, 'delay records'],
  ] as const;
  if (!coverage.pendingAirportLookups && !details.some(([count]) => count > 0)) return null;
  return (
    <View style={layout.section}>
      <Heading>Data coverage</Heading>
      <Body>Totals reflect the flight information available on this server.</Body>
      {details.map(([count, label]) => count > 0 && (
        <Body key={label}>{count === 1 ? '1 flight is' : `${count} flights are`} missing {label}.</Body>
      ))}
      {coverage.pendingAirportLookups > 0 && (
        <>
          <Body>Airport lookups pending for {coverage.pendingAirportLookups}
            {coverage.pendingAirportLookups === 1 ? ' airport.' : ' airports.'}</Body>
          <Action label="Reload Passport" secondary onPress={refresh} busy={pending} />
        </>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  multiline: { minHeight: 120, textAlignVertical: 'top' },
  detail: { fontSize: 16, lineHeight: 22 },
});
