import { useCallback, useEffect, useState, type ReactNode } from 'react';
import { useFocusEffect } from '@react-navigation/native';
import type { NativeStackScreenProps } from '@react-navigation/native-stack';
import { Alert, Platform, RefreshControl, ScrollView, Share, StyleSheet, Text, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';
import { saveServer, type Flight, type PlanoAPI } from './api';
import { dateKey, errorText, formatDay, formatTime, isFinished, nextFlight, validDate } from './flight';
import { usePalette } from './theme';
import { canStartLiveActivity, clearLiveActivities, configureLiveActivities, startLiveActivity, stopLiveActivity } from './live';
import { Action, Body, Chart, ErrorMessage, FlightRow, FlightStrip, Heading, Input, Loading, RoutePlot, layout } from './ui';

export type Routes = {
  Home: undefined;
  Search: undefined;
  Flight: { id: string; number: string; initial?: Flight };
  Tools: undefined;
  Import: undefined;
  Passport: undefined;
  Connection: undefined;
  Settings: undefined;
};

export type Screen<K extends keyof Routes> = NativeStackScreenProps<Routes, K>;
type APIProps<K extends keyof Routes> = Screen<K> & { api: PlanoAPI };

export function useRemote<T>(fetch: () => Promise<T>, enabled = true, interval?: number) {
  const [value, setValue] = useState<T>();
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);
  const [revision, setRevision] = useState(0);
  const refresh = useCallback(() => setRevision((previous) => previous + 1), []);
  useFocusEffect(useCallback(() => {
    if (!enabled) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    async function load(quiet = false) {
      if (!quiet) setPending(true);
      try {
        const result = await fetch();
        if (active) {
          setValue(result);
          setError(undefined);
        }
      } catch (cause) {
        if (active) setError(errorText(cause));
      } finally {
        if (active) {
          setPending(false);
          if (interval) timer = setTimeout(() => { void load(true); }, interval);
        }
      }
    }
    void load();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [enabled, fetch, interval, revision]));
  return { value, error, pending, refresh };
}

export function Page({ children, refreshing, refresh }: {
  children: ReactNode;
  refreshing?: boolean;
  refresh?: () => void;
}) {
  const { colors } = usePalette();
  const { bottom } = useSafeAreaInsets();
  return (
    <ScrollView keyboardShouldPersistTaps="handled" style={layout.screen}
      contentContainerStyle={[layout.content, { paddingBottom: Math.max(32, bottom + 16) }]}
      refreshControl={refresh ? <RefreshControl refreshing={!!refreshing} onRefresh={refresh} tintColor={colors.tint} /> : undefined}>
      {children}
    </ScrollView>
  );
}

export function Home({ api, navigation }: APIProps<'Home'>) {
  const load = useCallback(() => api.tracked(), [api]);
  const { value, error, pending, refresh } = useRemote(load, !!api.server, 60_000);
  const upcoming = nextFlight(value ?? []);
  const current = upcoming[0];
  const previous = (value ?? []).filter(isFinished).sort((a, b) =>
    Date.parse(b.scheduledDeparture) - Date.parse(a.scheduledDeparture));
  const open = (flight: Flight) => navigation.navigate('Flight', { id: flight.id, number: flight.number, initial: flight });
  return (
    <Page refresh={api.server ? refresh : undefined} refreshing={pending && !!value}>
      <Heading>My flights</Heading>
      {!api.server ? (
        <>
          <Body>Connect to your Plano server to search and track flights.</Body>
          <Action label="Connect a server" onPress={() => navigation.navigate('Settings')} />
        </>
      ) : (
        <>
          <View style={layout.actions}>
            <Action icon="search" label="Find a flight" onPress={() => navigation.navigate('Search')} />
            <Action icon="globe" secondary label="Trip tools" onPress={() => navigation.navigate('Tools')} />
          </View>
          <ErrorMessage message={error} />
          {error && <Action secondary label="Try again" onPress={refresh} busy={pending} />}
          {pending && !value && <Loading />}
          {!error && value?.length === 0 && <Body>Track a flight to see its departure, arrival, and latest updates here.</Body>}
          {current && (
            <View style={layout.section}>
              <Heading>Up next</Heading>
              <FlightStrip flight={current} />
              <Action secondary label={`View ${current.number}`} onPress={() => open(current)} />
            </View>
          )}
          {upcoming.length > 1 && (
            <View style={layout.section}>
              <Heading>Upcoming flights</Heading>
              {upcoming.slice(1).map((flight) => <FlightRow key={flight.id} flight={flight} onPress={() => open(flight)} />)}
            </View>
          )}
          {previous.length > 0 && (
            <View style={layout.section}>
              <Heading>Past flights</Heading>
              {previous.map((flight) => <FlightRow key={flight.id} flight={flight} onPress={() => open(flight)} />)}
            </View>
          )}
        </>
      )}
    </Page>
  );
}

export function Search({ api, navigation }: APIProps<'Search'>) {
  const [number, setNumber] = useState('');
  const [date, setDate] = useState(dateKey);
  const [results, setResults] = useState<Flight[]>();
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);
  async function search() {
    if (pending) return;
    const code = number.trim().toUpperCase().replace(/\s/g, '');
    if (!/^[A-Z0-9]{2,3}\d{1,4}[A-Z]?$/.test(code) || !validDate(date)) {
      setError('Enter a flight number and a valid departure date in YYYY-MM-DD format.');
      return;
    }
    setError(undefined);
    setResults(undefined);
    setPending(true);
    try {
      setResults(await api.search(code, date));
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setPending(false);
    }
  }
  return (
    <Page>
      <Body>Use the departure date at the origin airport.</Body>
      <Input label="Airline and flight number" value={number} onChangeText={setNumber} editable={!pending}
        autoCapitalize="characters" autoCorrect={false} placeholder="UA123" maxLength={12} />
      <Input label="Departure date" value={date} onChangeText={setDate} editable={!pending}
        autoCorrect={false} placeholder="YYYY-MM-DD" maxLength={10} />
      <View style={layout.actions}>
        <Action secondary label="Today" onPress={() => setDate(dateKey())} disabled={pending} />
        <Action secondary label="Tomorrow" onPress={() => {
          const next = new Date();
          next.setDate(next.getDate() + 1);
          setDate(dateKey(next));
        }} disabled={pending} />
      </View>
      <Action icon="search" label="Search flights" onPress={search} busy={pending} />
      <ErrorMessage message={error} />
      {results?.length === 0 && <Body>No matching flights. Check the number and departure date.</Body>}
      {results && results.length > 0 && (
        <View style={layout.section}>
          <Heading>Matching flights</Heading>
          {results.map((flight) => (
            <FlightRow key={flight.id} flight={flight}
              onPress={() => navigation.navigate('Flight', { id: flight.id, number: flight.number, initial: flight })} />
          ))}
        </View>
      )}
    </Page>
  );
}

export function FlightDetailScreen({ api, route, navigation }: APIProps<'Flight'>) {
  const { colors } = usePalette();
  const { id, initial } = route.params;
  const load = useCallback(async () => {
    const [detail, tracked] = await Promise.all([api.detail(id), api.tracked()]);
    return { detail, tracked: tracked.some((item) => item.id === id) };
  }, [api, id]);
  const { value, error, pending, refresh } = useRemote(load, true, 30_000);
  const [actionError, setActionError] = useState<string>();
  const [operation, setOperation] = useState<'track' | 'share' | 'live'>();
  const [seat, setSeat] = useState('');
  const flight = value?.detail.flight ?? initial;
  const detail = value?.detail;
  const points = (detail?.telemetry ?? []).filter(({ latitude, longitude }) =>
    Number.isFinite(latitude) && Number.isFinite(longitude)).sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
  const latestPoint = points.at(-1);
  const prediction = detail?.prediction;

  async function toggleTrack() {
    if (!value || operation) return;
    setOperation('track');
    setActionError(undefined);
    try {
      if (value.tracked) {
        await api.untrack(id);
        refresh();
        await stopLiveActivity(id);
      }
      else await api.track(id, seat.trim().toUpperCase() || undefined);
      refresh();
    } catch (cause) {
      setActionError(errorText(cause));
    } finally {
      setOperation(undefined);
    }
  }
  async function share() {
    if (!flight || operation) return;
    setOperation('share');
    setActionError(undefined);
    try {
      const { url } = await api.share(id);
      await Share.share({ message: `Follow ${flight.number} from ${flight.origin} to ${flight.destination} on Plano.\n${url}` });
    } catch (cause) {
      setActionError(errorText(cause));
    } finally {
      setOperation(undefined);
    }
  }
  return (
    <Page refresh={refresh} refreshing={pending && !!flight}>
      <ErrorMessage message={error} />
      {error && <Action secondary label="Refresh flight" onPress={refresh} />}
      {!flight && pending && <Loading />}
      {flight && (
        <>
          <FlightStrip flight={flight} />
          <Body>{formatDay(flight.scheduledDeparture)} · Times shown in your device’s time zone</Body>
          <Body>Last flight update {formatTime(flight.observedAt)}</Body>
          {value && !value.tracked && !isFinished(flight) && (
            <Input label="Seat (optional)" value={seat} onChangeText={setSeat} placeholder="12A"
              maxLength={8} autoCapitalize="characters" autoCorrect={false} />
          )}
          <View style={layout.actions}>
            {value && <Action label={value.tracked ? 'Stop tracking' : 'Track flight'} onPress={toggleTrack}
              busy={operation === 'track'} disabled={pending || !!operation && operation !== 'track'}
              icon="plane" secondary={value.tracked} />}
            {value?.tracked && !isFinished(flight) && canStartLiveActivity && (
              <Action label="Start Live Activity" icon="bell" secondary busy={operation === 'live'}
                disabled={!!operation && operation !== 'live'} onPress={() => {
                  setOperation('live');
                  setActionError(undefined);
                  void startLiveActivity(api, flight, seat).catch((cause: unknown) => setActionError(errorText(cause)))
                    .finally(() => setOperation(undefined));
                }} />
            )}
            {value?.tracked && <Action label="Share live link" onPress={share} busy={operation === 'share'}
              disabled={!!operation && operation !== 'share'} icon="share" secondary />}
          </View>
          <ErrorMessage message={actionError} />
          {(flight.aircraftType || flight.tailNumber || flight.baggage) && (
            <View style={layout.section}>
              <Heading>Flight details</Heading>
              <View style={layout.fieldRow}>
                {flight.aircraftType && <Body>Aircraft {flight.aircraftType}</Body>}
                {flight.tailNumber && <Body>Tail {flight.tailNumber}</Body>}
                {flight.baggage && <Body>Baggage carousel {flight.baggage}</Body>}
              </View>
            </View>
          )}
          {prediction && (
            <View style={layout.section}>
              <Heading>Delay outlook</Heading>
              {!!prediction.delayMinutes && <Body>Estimated departure delay of {prediction.delayMinutes} min.</Body>}
              {prediction.departure && <Body>Predicted departure at {formatTime(prediction.departure)}.</Body>}
              {prediction.reasons?.map((reason, index) => <Body key={index}>{reason}</Body>)}
              <Body>Plano estimate. Check your airline for official times.</Body>
            </View>
          )}
          {!!detail?.inbound?.length && (
            <View style={layout.section}>
              <Heading>Where’s my plane?</Heading>
              {detail.inbound.map((leg) => <FlightRow key={leg.id} flight={leg}
                onPress={() => navigation.push('Flight', { id: leg.id, number: leg.number, initial: leg })} />)}
            </View>
          )}
          {latestPoint && (
            <View style={layout.section}>
              <Heading>Recorded flight path</Heading>
              <RoutePlot label={`Recorded path from ${flight.origin} to ${flight.destination}`}
                points={points.map(({ latitude, longitude }) => [latitude, longitude])} />
              <Body>Last position at {formatTime(latestPoint.at)}{latestPoint.source && ` · ${latestPoint.source}`}</Body>
              <Chart label="Altitude (ft)" values={points.flatMap((point) => point.altitudeFeet ?? [])} />
              <Chart label="Ground speed (kt)" values={points.flatMap((point) => point.groundSpeedKnots ?? [])} />
            </View>
          )}
          {detail && (
            <View style={layout.section}>
              <Heading>Flight updates</Heading>
              {!detail.events?.length && <Body>New flight updates will appear here.</Body>}
              {detail.events?.map((event, index) => (
                <View key={event.id ?? index} style={[styles.event, { borderColor: colors.divider }]}>
                  <View style={styles.eventTitle}>
                    <Text style={[styles.strong, { color: colors.text }]}>{event.title || event.kind}</Text>
                    <Body>{formatTime(event.at)}</Body>
                  </View>
                  {event.body && <Body>{event.body}</Body>}
                </View>
              ))}
            </View>
          )}
        </>
      )}
    </Page>
  );
}

export function ServerSettings({ api, error, onChange }: {
  api: PlanoAPI;
  error?: string;
  onChange: (url: string) => void;
}) {
  const [server, setServer] = useState(api.server);
  const [issue, setIssue] = useState(error);
  const [enrollmentCode, setEnrollmentCode] = useState('');
  const [pending, setPending] = useState(false);
  const [connected, setConnected] = useState(false);
  useEffect(() => { setServer(api.server); }, [api.server]);

  async function connect() {
    if (pending) return;
    setIssue(undefined);
    setConnected(false);
    setPending(true);
    try {
      const url = await saveServer(server, enrollmentCode.trim(), api.server);
      if (api.server !== url) await clearLiveActivities();
      setEnrollmentCode('');
      setConnected(true);
      onChange(url);
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      setPending(false);
    }
  }

  return (
    <Page>
      <Heading>Plano server</Heading>
      <Body>Your tracked flights and alerts are tied to the server you connect to. Switching servers opens a separate flight list. Plano removes alert registrations from the previous server when it can reach it.</Body>
      <Input label="Server address" value={server} onChangeText={setServer} editable={!pending}
        autoCapitalize="none" autoCorrect={false} keyboardType="url" placeholder="https://plano.example.org" />
      <Input label="Enrollment code (if required)" value={enrollmentCode} onChangeText={setEnrollmentCode}
        editable={!pending} secureTextEntry autoCapitalize="none" autoCorrect={false} />
      <Action label="Connect to server" onPress={connect} busy={pending} disabled={!server.trim()} />
      <ErrorMessage message={issue} />
      {connected && <Text accessibilityLiveRegion="polite">Connected to your Plano server.</Text>}
      {api.server && Platform.OS !== 'web' && <AlertsSettings api={api} />}
    </Page>
  );
}

function AlertsSettings({ api }: { api: PlanoAPI }) {
  const [pending, setPending] = useState(false);
  const [issue, setIssue] = useState<string>();
  const [enabled, setEnabled] = useState(false);
  async function enable() {
    setPending(true);
    setIssue(undefined);
    try {
      if (Platform.OS !== 'ios' && Platform.OS !== 'android') return;
      const notifications = await import('expo-notifications');
      if (Platform.OS === 'android') {
        await notifications.setNotificationChannelAsync('flight-alerts', {
          name: 'Flight alerts', importance: notifications.AndroidImportance.HIGH,
        });
      }
      const permission = await notifications.requestPermissionsAsync();
      if (!permission.granted) {
        Alert.alert('Flight alerts are off', 'Allow notifications for Plano in your device settings.');
        return;
      }
      const device = await notifications.getDevicePushTokenAsync();
      if (typeof device.data !== 'string' || !device.data) throw new Error('The device didn’t return a push token.');
      await api.registerDevice({ platform: Platform.OS, token: device.data, kind: 'alert' });
      await configureLiveActivities(api);
      setEnabled(true);
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      setPending(false);
    }
  }
  return (
    <View style={layout.section}>
      <Heading>Flight alerts</Heading>
      <Body>Get gate, delay, and arrival updates from this server.</Body>
      <Action icon="bell" secondary label={enabled ? 'Refresh alert registration' : 'Enable flight alerts'}
        onPress={enable} busy={pending} />
      {enabled && <Body>Flight alerts are registered with this server.</Body>}
      <ErrorMessage message={issue} />
    </View>
  );
}

const styles = StyleSheet.create({
  event: { borderBottomWidth: StyleSheet.hairlineWidth, paddingBottom: 12, gap: 5 },
  eventTitle: { flexDirection: 'row', justifyContent: 'space-between', flexWrap: 'wrap', gap: 8 },
  strong: { fontSize: 16, fontWeight: '600' },
});
