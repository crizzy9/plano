import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { useFocusEffect } from '@react-navigation/native';
import type { NativeStackScreenProps } from '@react-navigation/native-stack';
import { AccessibilityInfo, AppState, Platform, RefreshControl, ScrollView, Share, StyleSheet, Text, View } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';
import {
  saveServer, withActiveAndroidPushOrigin, type AirportWeatherForecast, type ATFMSlot, type Flight, type FlightNotice, type HistoricalDepartureRisk,
  type PassportFlight, type PlanoAPI, type Position, type RadarCursor,
} from './api';
import { dateKey, errorText, formatDay, formatTime, isFinished, nextFlight, validDate } from './flight';
import { usePalette } from './theme';
import { PickupLinks } from './pickup-links';
import { changeFlightAlerts, flightAlertsEnabled, startIOSFlightActivity } from './alerts';
import {
  androidWidgetDetailsEnabled, androidWidgetFlightPinned, canCustomizeAndroidWidget, canPinAndroidWidgetFlight,
  canStartLiveActivity, setAndroidWidgetDetailsEnabled, setAndroidWidgetFlightPinned,
} from './live';
import { Action, Body, ErrorMessage, FlightRow, FlightStrip, Heading, Input, Loading, layout } from './ui';
import { RadarMetricChart, RadarRouteMap } from './radar-visuals';

export type Routes = {
  Home: undefined;
  Search: undefined;
  Flight: { id: string; number: string; initial?: Flight; rotationOf?: string; initialSavedAt?: string };
  Tools: undefined;
  Import: undefined;
  Passport: undefined;
  PassportEditor: { date?: string; number?: string; flight?: PassportFlight; fromSearch?: boolean } | undefined;
  Friends: undefined;
  Connection: { inbound: string; outbound: string } | undefined;
  Settings: undefined;
};

export type Screen<K extends keyof Routes> = NativeStackScreenProps<Routes, K>;
type APIProps<K extends keyof Routes> = Screen<K> & { api: PlanoAPI };

export function useRemote<T>(fetch: () => Promise<T>, enabled = true, interval?: number) {
  const [value, setValue] = useState<T>();
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);
  const [revision, setRevision] = useState(0);
  const requestVersion = useRef(0);
  const refresh = useCallback(() => {
    requestVersion.current += 1;
    setRevision((previous) => previous + 1);
  }, []);
  useFocusEffect(useCallback(() => {
    if (!enabled) return;
    let active = true;
    const version = requestVersion.current;
    const current = () => active && requestVersion.current === version;
    let timer: ReturnType<typeof setTimeout>;
    async function load(quiet = false) {
      if (!current()) return;
      if (!quiet) setPending(true);
      try {
        const result = await fetch();
        if (current()) {
          setValue(result);
          setError(undefined);
        }
      } catch (cause) {
        if (current()) setError(errorText(cause));
      } finally {
        if (current()) {
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
  return { value, setValue, error, pending, refresh };
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
  const load = useCallback(() => api.flightOverview(), [api]);
  const { value: overview, error, pending, refresh } = useRemote(load, !!api.server, 60_000);
  const value = overview?.savedAt && error ? undefined : overview?.flights;
  const savedAt = value && overview?.savedAt;
  const upcoming = nextFlight(value ?? []);
  const current = upcoming[0];
  const previous = (value ?? []).filter(isFinished).sort((a, b) =>
    Date.parse(b.scheduledDeparture ?? b.observedAt) - Date.parse(a.scheduledDeparture ?? a.observedAt));
  const open = (flight: Flight) => navigation.navigate('Flight', {
    id: flight.id, number: flight.number, initial: flight, initialSavedAt: savedAt,
  });
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
          <ErrorMessage message={savedAt ? savedFlightMessage(savedAt) : error} />
          {(error || savedAt) && <Action secondary label="Refresh flights" onPress={refresh} busy={pending} />}
          {pending && !value && <Loading />}
          {!error && value?.length === 0 && <Body>{savedAt ? 'No flights were saved.' : 'Track a flight to see its departure, arrival, and latest updates here.'}</Body>}
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
  const [results, setResults] = useState<{ flights: Flight[]; date: string; number: string }>();
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
      setResults({ flights: await api.search(code, date), date, number: code });
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
      {results?.flights.length === 0 && (
        <View style={layout.section}>
          <Body>No matching flights. Check the number and departure date.</Body>
          {results.date < new Date().toISOString().slice(0, 10) && (
            <Action secondary label="Log past flight in Passport"
              onPress={() => navigation.navigate('PassportEditor', { date: results.date, number: results.number, fromSearch: true })} />
          )}
        </View>
      )}
      {results && results.flights.length > 0 && (
        <View style={layout.section}>
          <Heading>Matching flights</Heading>
          {results.flights.map((flight) => (
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
  const { id, initial, initialSavedAt, rotationOf } = route.params;
  const load = useCallback(() => api.flightScreen(id, rotationOf), [api, id, rotationOf]);
  const { value: result, setValue, error, pending, refresh } = useRemote(load, true, 30_000);
  const value = result?.savedAt && error ? undefined : result;
  const savedAt = value?.savedAt ?? (!value && !error ? initialSavedAt : undefined);
  const [actionError, setActionError] = useState<string>();
  const [operation, setOperation] = useState<'track' | 'share' | 'live' | 'widget'>();
  const widgetEditing = useRef(false);
  const [seat, setSeat] = useState('');
  const [pickupRevision, setPickupRevision] = useState(0);
  const flight = value?.detail.flight ?? (error && (result?.savedAt || initialSavedAt) ? undefined : initial);
  const registry = flight?.aircraftRegistry?.source === 'transport_canada_ccar' ? flight.aircraftRegistry : undefined;
  const registryModel = flight?.aircraftModel ? undefined : registry?.model;
  const registryAge = flight?.aircraftAge == null ? registry?.age : undefined;
  const aircraft = flight?.aircraftModel || registryModel || flight?.aircraftType;
  const aircraftAge = flight?.aircraftAge ?? registryAge;
  const detail = value?.detail;
  const radarTracked = value?.tracked === true && !savedAt;
  const radarKey = flight && [api.server, id, flight.originICAO ?? flight.origin, flight.destinationICAO ?? flight.destination,
    flight.scheduledDeparture, flight.tailNumber, flight.actualTakeoff, detail?.receiverRevision, radarTracked].join('|');
  const rotationSnapshot = detail?.rotationSnapshot === true;
  const widgetEligible = canPinAndroidWidgetFlight && !!api.server && !savedAt && !error && !rotationOf && !rotationSnapshot
    && value?.tracked === true && flight?.id === id;
  const widgetFinished = !!flight && isFinished(flight);
  const loadWidgetPin = useCallback(async () => ({
    origin: api.server, flightId: id,
    pinned: await withActiveAndroidPushOrigin(api.server, () => androidWidgetFlightPinned(api.server, id)) ?? null,
  }), [api.server, id]);
  const { value: widgetPin, error: widgetPinError, pending: widgetPending, refresh: refreshWidgetPin, setValue: setWidgetPin } =
    useRemote(loadWidgetPin, widgetEligible);
  const widgetPinned = widgetEligible && !widgetPinError && widgetPin?.origin === api.server && widgetPin.flightId === id
    ? widgetPin.pinned : null;
  useEffect(() => {
    if (!widgetEligible) return;
    const subscription = AppState.addEventListener('focus', refreshWidgetPin);
    return () => subscription.remove();
  }, [widgetEligible, refreshWidgetPin]);
  const weather = detail?.weather?.filter((report) => Date.parse(report.validUntil) > Date.now()) ?? [];
  const forecasts = value?.tracked && !savedAt && !error && !rotationOf && !rotationSnapshot && detail?.flight.id === id
    ? detail.weatherForecasts : undefined;
  const forecastRows = useMemo(() => forecasts?.map((forecast) => ({
    forecast, issued: Date.parse(forecast.issuedAt), starts: Date.parse(forecast.startsAt), expires: Date.parse(forecast.endsAt),
  })) ?? [], [forecasts]);
  const notices = value?.tracked && detail?.flight.id === id ? detail.notices : undefined;
  const noticeRows = useMemo(() => notices?.slice(0, 8).map((notice) => ({ notice, expires: Date.parse(notice.validUntil) })) ?? [], [notices]);
  const slots = value?.tracked && !savedAt && !error && !rotationSnapshot && detail?.flight.id === id && flight &&
    !flight.actualTakeoff && !flight.actualLanding && !flight.actualArrival &&
    !['airborne', 'landed', 'arrived', 'cancelled', 'diverted'].includes(flight.status) ? detail.atfmSlots : undefined;
  const slotRows = useMemo(() => slots?.map((slot) => ({
    slot, takeoff: Date.parse(slot.takeoffAt), observed: Date.parse(slot.observedAt), expires: Date.parse(slot.validUntil),
  })) ?? [], [slots]);
  const predictionExpiry = detail?.prediction?.validUntil;
  const predictionExpiresAt = predictionExpiry === undefined ? Infinity : Date.parse(predictionExpiry);
  const historicalRisk = !savedAt && !error && !rotationOf && !rotationSnapshot && detail?.flight.id === id
    ? detail.historicalDepartureRisk : undefined;
  const historicalRiskExpiresAt = validHistoricalDepartureRisk(historicalRisk) && flight &&
    (flight.status === 'scheduled' || flight.status === 'boarding') &&
    !flight.actualDeparture && !flight.actualTakeoff && !flight.actualLanding && !flight.actualArrival
    ? Date.parse(flight.scheduledDeparture ?? '') : NaN;
  const expiries = useMemo(() => [predictionExpiresAt, historicalRiskExpiresAt,
    ...noticeRows.map(({ expires }) => expires), ...slotRows.map(({ expires }) => expires),
    ...forecastRows.flatMap(({ issued, expires }) => [issued, expires])]
    .filter(Number.isFinite), [predictionExpiresAt, historicalRiskExpiresAt, noticeRows, slotRows, forecastRows]);
  const now = useExpiryClock(expiries);
  const prediction = predictionExpiresAt > now ? detail?.prediction : undefined;
  const currentHistoricalRisk = historicalRiskExpiresAt > now ? historicalRisk : undefined;
  const currentNotices = noticeRows.filter(({ expires }) => expires > now).map(({ notice }) => notice);
  const currentSlots = slotRows.filter(({ takeoff, expires }) => Number.isFinite(takeoff) && expires > now);
  const currentForecasts = forecastRows.filter(({ issued, starts, expires }) => issued <= now && starts < expires && expires > now);

  async function toggleTrack() {
    if (!value || savedAt || rotationSnapshot || operation) return;
    setOperation('track');
    setActionError(undefined);
    try {
      if (value.tracked) {
        await api.untrack(id);
        refresh();
        setValue((old) => old && ({
          ...old, tracked: false,
          detail: { ...old.detail, telemetry: undefined, weather: undefined, weatherForecasts: undefined, notices: undefined, atfmSlots: undefined },
        }));
      } else {
        await api.track(id, seat.trim().toUpperCase() || undefined);
        refresh();
      }
    } catch (cause) {
      setActionError(errorText(cause));
    } finally {
      setOperation(undefined);
    }
  }
  async function share() {
    if (!flight || savedAt || operation) return;
    setOperation('share');
    setActionError(undefined);
    try {
      const { url } = await api.share(id);
      setPickupRevision((revision) => revision + 1);
      await Share.share({ message: `Follow ${flight.number} from ${flight.origin} to ${flight.destination} on Plano.\n${url}` });
    } catch (cause) {
      setActionError(errorText(cause));
    } finally {
      setOperation(undefined);
    }
  }
  async function toggleWidgetPin() {
    if (widgetPinned === null || widgetFinished && !widgetPinned || pending || widgetPending || operation || widgetEditing.current) return;
    widgetEditing.current = true;
    setOperation('widget');
    setActionError(undefined);
    const origin = api.server;
    const pinned = !widgetPinned;
    try {
      const updated = await withActiveAndroidPushOrigin(origin, () => setAndroidWidgetFlightPinned(origin, id, pinned));
      if (updated) {
        setWidgetPin({ origin, flightId: id, pinned });
        AccessibilityInfo.announceForAccessibility(pinned ? 'Flight selected for the widget.' : 'Automatic widget selection enabled.');
      } else {
        setActionError('The widget selection changed. Try again.');
      }
    } catch {
      setActionError('Couldn’t change the widget flight. Try again.');
    } finally {
      refreshWidgetPin();
      widgetEditing.current = false;
      setOperation(undefined);
    }
  }
  return (
    <Page refresh={refresh} refreshing={pending && !!flight}>
      <ErrorMessage message={savedAt ? savedFlightMessage(savedAt) : error} />
      {(error || savedAt) && <Action secondary label="Refresh flight" onPress={refresh} busy={pending} />}
      {!flight && pending && <Loading />}
      {flight && (
        <>
          {rotationSnapshot && <Body>Aircraft rotation snapshot</Body>}
          <FlightStrip flight={flight} />
          <Body>{formatDay(flight.scheduledDeparture, flight.originTimeZone)} · Airport local times when available</Body>
          <Body>{rotationSnapshot ? 'Last reported' : 'Last flight update'} {formatTime(flight.observedAt)}</Body>
          {value && !savedAt && !rotationSnapshot && !value.tracked && !isFinished(flight) && (
            <Input label="Seat (optional)" value={seat} onChangeText={setSeat} placeholder="12A"
              maxLength={8} autoCapitalize="characters" autoCorrect={false} />
          )}
          {value && !savedAt && !rotationSnapshot && (
            <View style={layout.actions}>
              <Action label={value.tracked ? 'Stop tracking' : 'Track flight'} onPress={toggleTrack}
                busy={operation === 'track'} disabled={pending || !!operation && operation !== 'track'}
                icon="plane" secondary={value.tracked} />
              {value.tracked && !isFinished(flight) && canStartLiveActivity && (
                <Action label="Start Live Activity" icon="bell" secondary busy={operation === 'live'}
                  disabled={!!operation && operation !== 'live'} onPress={() => {
                    setOperation('live');
                    setActionError(undefined);
                    void startIOSFlightActivity(api, flight, seat).catch((cause: unknown) => setActionError(errorText(cause)))
                      .finally(() => setOperation(undefined));
                  }} />
              )}
              {value.tracked && <Action label="Share live link" onPress={share} busy={operation === 'share'}
                disabled={!!operation && operation !== 'share'} icon="share" secondary />}
              {widgetPinned !== null && (!widgetFinished || widgetPinned) && (
                <Action label={widgetPinned ? 'Use automatic widget selection' : 'Show on flight widget'}
                  onPress={toggleWidgetPin} busy={operation === 'widget'} disabled={pending || widgetPending || !!operation} secondary />
              )}
            </View>
          )}
          {widgetPinned && (
            <Body>{widgetFinished ? 'The widget is still set to this flight.' : 'The widget shows this flight when a fresh update is available.'}</Body>
          )}
          <ErrorMessage message={actionError} />
          {value?.tracked && !savedAt && <PickupLinks key={id} api={api} flightId={id} refreshRevision={pickupRevision} />}
          {(aircraft || aircraftAge != null || flight.tailNumber || flight.baggage) && (
            <View style={layout.section}>
              <Heading>Flight details</Heading>
              <View style={layout.fieldRow}>
                {aircraft && <Body>Aircraft {aircraft}</Body>}
                {flight.tailNumber && <Body>Tail {flight.tailNumber}</Body>}
                {aircraftAge != null && <Body>Aircraft age about {aircraftAge} years</Body>}
                {flight.baggage && <Body>Baggage carousel {flight.baggage}</Body>}
              </View>
              {(!!registryModel || registryAge != null) && <CanadianAircraftNotice />}
            </View>
          )}
          {currentSlots.length > 0 && (
            <View style={layout.section}>
              <Heading>Assigned takeoff time</Heading>
              {currentSlots.map(({ slot, takeoff, observed }) => (
                <View key={slot.source} style={[styles.event, { borderColor: colors.divider }]}>
                  <Text style={[styles.strong, { color: colors.text }]}>
                    {atfmSlotLabels[slot.source]} · {airportDateTime(takeoff, flight.originTimeZone)}
                  </Text>
                  {slot.reason && <Body>{slot.reason}</Body>}
                  {Number.isFinite(observed) && <Body>Reported {airportDateTime(observed, flight.originTimeZone)}</Body>}
                </View>
              ))}
              <Body>Check your airline for gate and boarding times.</Body>
            </View>
          )}
          {(prediction || currentHistoricalRisk || weather.length > 0 || currentForecasts.length > 0) && (
            <View style={layout.section}>
              <Heading>Delay outlook</Heading>
              {prediction && (
                <>
                  {!!prediction.delayMinutes && <Body>Estimated departure delay of {prediction.delayMinutes} min.</Body>}
                  {prediction.departure && <Body>Predicted departure at {formatTime(prediction.departure, flight.originTimeZone)}.</Body>}
                  {prediction.reasons?.map((reason, index) => <Body key={index}>{reason}</Body>)}
                  <Body>Plano estimate. Check your airline for official times.</Body>
                </>
              )}
              {currentHistoricalRisk && (
                <View style={styles.historicalRisk}>
                  <Text accessibilityRole="header" style={[styles.strong, { color: colors.text }]}>Historical departure risk</Text>
                  <Body>{riskPercent.format(currentHistoricalRisk.probability)} estimated chance of departing at least 15 min late for flights that operate.</Body>
                  <Body>Based on {riskSampleCount.format(currentHistoricalRisk.sampleSize)} past departures from {flight.origin} in this scheduled time window. Records through {riskTrainingDay.format(new Date(currentHistoricalRisk.trainingThrough))}.</Body>
                </View>
              )}
              {weather.length > 0 && (
                <Text accessibilityRole="header" style={[styles.weatherHeading, { color: colors.text }]}>Observed airport weather</Text>
              )}
              {weather.map((report) => {
                const timeZone = report.operation === 'departure' ? flight.originTimeZone : flight.destinationTimeZone;
                return (
                  <View key={`${report.airport}:${report.operation}`}>
                    <Body>{report.airport} · {report.operation === 'departure' ? 'Departure' : 'Arrival'}</Body>
                    <Body>{report.conditions}</Body>
                    <Body>NOAA report at {formatTime(report.observedAt, timeZone)}</Body>
                  </View>
                );
              })}
              {currentForecasts.length > 0 && (
                <Text accessibilityRole="header" style={[styles.weatherHeading, { color: colors.text }]}>Airport weather forecast</Text>
              )}
              {currentForecasts.map(({ forecast, issued, starts, expires }) => {
                const departure = forecast.operation === 'departure';
                const zone = departure ? flight.originTimeZone : flight.destinationTimeZone;
                return (
                  <View key={`${forecast.airport}:${forecast.operation}:${forecast.startsAt}:${forecast.endsAt}:${forecast.qualifier}:${forecast.probability ?? ''}:${forecast.conditions}`}
                    style={[styles.event, { borderColor: colors.divider }]}>
                    <Text style={[styles.strong, { color: colors.text }]}>
                      {forecast.airport} · {departure ? 'Departure' : 'Arrival'} airport
                    </Text>
                    <Body>{forecastConditionLabel(forecast)} · {forecast.conditions}</Body>
                    <Body>{airportDateTime(starts, zone)} to {airportDateTime(expires, zone)}</Body>
                    <Body>NOAA forecast issued {airportDateTime(issued, zone)}</Body>
                  </View>
                );
              })}
            </View>
          )}
          {currentNotices.length > 0 && <AirportNotices flight={flight} notices={currentNotices} />}
          {detail && (!!detail.inbound?.length || value?.tracked && !flight.actualDeparture &&
            !isFinished(flight) && !['taxi_out', 'airborne', 'landed', 'diverted'].includes(flight.status)) && (
            <AircraftRotation flight={flight} legs={detail.inbound ?? []} open={savedAt ? undefined : (leg) =>
              navigation.push('Flight', { id: leg.id, number: leg.number, initial: leg, rotationOf: value?.tracked ? flight.id : undefined })} />
          )}
          <FlightRadar key={radarKey} api={api} flight={flight} tracked={radarTracked} history={detail?.telemetry}
            detailRevision={detail?.receiverRevision} refreshFlight={refresh} />
          {detail && (!rotationSnapshot || !!detail.events?.length) && (
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

const riskPercent = new Intl.NumberFormat(undefined, { style: 'percent' });
const riskSampleCount = new Intl.NumberFormat();
const riskTrainingDay = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', year: 'numeric', timeZone: 'UTC' });
const atfmSlotLabels: Record<ATFMSlot['source'], string> = { faa_swim: 'FAA EDCT', eurocontrol_nm: 'EUROCONTROL CTOT' };

function validHistoricalDepartureRisk(risk: HistoricalDepartureRisk | undefined): boolean {
  return !!risk && Number.isFinite(risk.probability) && risk.probability >= 0 && risk.probability <= 1 &&
    Number.isSafeInteger(risk.sampleSize) && risk.sampleSize > 0 &&
    typeof risk.trainingThrough === 'string' && validDate(risk.trainingThrough) &&
    typeof risk.modelVersion === 'string' && risk.modelVersion.trim().length > 0;
}

function savedFlightMessage(savedAt: string) {
  return `Showing flight data saved ${formatDay(savedAt)} at ${formatTime(savedAt)}. Live changes may be missing.`;
}

const rotationFreshness = 30 * 60_000;

function CanadianAircraftNotice() {
  const { colors } = usePalette();
  const [showWarranty, setShowWarranty] = useState(false);
  return (
    <View style={styles.registryNotice}>
      <Text accessibilityRole="header" style={[styles.strong, { color: colors.text }]}>Transport Canada aircraft data</Text>
      <Body>Reproduced and distributed with the permission of the Government of Canada.</Body>
      <Body>This product has been produced by or for Plano and includes data provided by the Government of Canada.</Body>
      <Body>The incorporation of data sourced from the Government of Canada within this product shall not be construed as constituting an endorsement by the Government of Canada of our product.</Body>
      <Action secondary label={showWarranty ? 'Hide registry warranty terms' : 'Show registry warranty terms'}
        onPress={() => setShowWarranty((shown) => !shown)} />
      {showWarranty && (
        <Body>CANADA MAKES NO REPRESENTATION AND GIVES NO WARRANTY OF ANY KIND WITH RESPECT TO THE ACCURACY, USEFULNESS, NOVELTY, VALIDITY, SCOPE, COMPLETENESS OR CURRENCY OF THE DATA MADE AVAILABLE THROUGH THE GC OPEN DATA PORTAL AND EXPRESSLY DISCLAIMS ANY IMPLIED WARRANTY OF MERCHANTABILITY OR FITNESS FOR A PARTICULAR PURPOSE OF THE DATA MADE AVAILABLE THROUGH THE GC OPEN DATA PORTAL.</Body>
      )}
    </View>
  );
}

function directRotationLeg(flight: Flight, leg: Flight | undefined) {
  if (!leg || leg.id === flight.id || ['cancelled', 'diverted', 'unknown'].includes(leg.status)) return;
  const airport = leg.destinationICAO || leg.destination;
  const sameAirport = !!airport && (airport === (flight.originICAO || flight.origin) ||
    (!(leg.destinationICAO && flight.originICAO) && !!leg.destination && leg.destination === flight.origin));
  if (!sameAirport) return;
  const linked = flight.inboundId === leg.id;
  if (flight.tailNumber && leg.tailNumber) {
    if (flight.tailNumber.toUpperCase() !== leg.tailNumber.toUpperCase()) return;
    return { leg, source: linked ? 'Provider-linked inbound' : `Matched by tail ${leg.tailNumber}` };
  }
  if (linked) return { leg, source: 'Provider-linked inbound' };
}

function airportDateTime(time: number, zone?: string) {
  const options: Intl.DateTimeFormatOptions = { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' };
  if (zone) {
    try { return new Intl.DateTimeFormat(undefined, { ...options, timeZone: zone }).format(time); } catch { /* use UTC */ }
  }
  return new Intl.DateTimeFormat(undefined, { ...options, timeZone: 'UTC', timeZoneName: 'short' }).format(time);
}

function forecastConditionLabel({ qualifier, probability }: AirportWeatherForecast) {
  const chance = probability !== undefined && probability > 0 && probability <= 100
    ? `${riskPercent.format(probability / 100)} chance` : undefined;
  switch (qualifier) {
    case 'temporary': return chance ? `${chance} of temporary conditions` : 'Temporary conditions';
    case 'probability': return chance ?? 'Possible conditions';
    case 'becoming': return 'Possible during a weather transition';
    case 'prevailing': return 'Expected conditions';
  }
}

function inboundArrival(leg: Flight, now: number, stale: boolean) {
  const gateIn = Date.parse(leg.actualArrival ?? '');
  if (Number.isFinite(gateIn)) return { label: 'Recorded gate arrival', time: gateIn };
  const wheelsOn = Date.parse(leg.actualLanding ?? '');
  const estimate = Date.parse(leg.estimatedArrival ?? '');
  if (Number.isFinite(estimate) && (!Number.isFinite(wheelsOn) || estimate > wheelsOn)) {
    return { label: stale || estimate <= now ? 'Last estimated gate arrival' : 'Estimated gate arrival', time: estimate };
  }
  if (Number.isFinite(wheelsOn)) return { label: 'Recorded landing', time: wheelsOn };
  const scheduled = Date.parse(leg.scheduledArrival ?? '');
  if (Number.isFinite(scheduled)) return { label: 'Scheduled gate arrival', time: scheduled };
}

function AircraftRotation({ flight, legs, open }: { flight: Flight; legs: Flight[]; open?: (leg: Flight) => void }) {
  const { colors } = usePalette();
  const immediate = directRotationLeg(flight, legs[0]);
  const earlier = immediate ? legs.slice(1) : legs;
  const observed = Date.parse(immediate?.leg.observedAt ?? '');
  const staleAt = observed + rotationFreshness;
  const estimate = Date.parse(immediate?.leg.estimatedArrival ?? '');
  const expiries = useMemo(() => [observed, staleAt, estimate].filter(Number.isFinite), [observed, staleAt, estimate]);
  const now = useExpiryClock(expiries);
  const stale = !Number.isFinite(observed) || observed > now || now >= staleAt;
  const timing = immediate && inboundArrival(immediate.leg, now, stale);
  const zone = flight.originTimeZone || immediate?.leg.destinationTimeZone;
  return (
    <View style={layout.section}>
      <Heading>Where’s my plane?</Heading>
      {immediate ? (
        <>
          <View style={styles.rotationSummary}>
            <Text accessibilityRole="header" style={[styles.strong, { color: colors.text }]}>
              Inbound to {flight.origin || flight.originICAO}
            </Text>
            {timing ? <Body>{timing.label} · {airportDateTime(timing.time, zone)}</Body> : <Body>Arrival time unavailable.</Body>}
            <Body>{immediate.source}</Body>
            {Number.isFinite(observed) ? (
              <Body>{stale ? 'Last reported' : 'Reported'} {airportDateTime(observed, zone)}</Body>
            ) : <Body>Report time unavailable.</Body>}
          </View>
          <FlightRow flight={immediate.leg} onPress={open && (() => open(immediate.leg))} />
        </>
      ) : <Body>Immediate inbound details are unavailable.</Body>}
      {!!earlier.length && (
        <>
          <Text accessibilityRole="header" style={[styles.strong, { color: colors.text }]}>
            {immediate ? 'Earlier aircraft legs' : 'Available aircraft history'}
          </Text>
          {earlier.map((leg) => <FlightRow key={leg.id} flight={leg} onPress={open && (() => open(leg))} />)}
        </>
      )}
    </View>
  );
}

const noticeSourceLabels: Record<string, string> = {
  faa_nas: 'FAA NAS', faa_swim: 'FAA SWIM', eurocontrol_nm: 'EUROCONTROL Network Manager',
};

function useExpiryClock(expiries: number[]) {
  const [checkedAt, setCheckedAt] = useState(Date.now);
  useFocusEffect(useCallback(() => {
    if (!expiries.length) return;
    let timer: ReturnType<typeof setTimeout>;
    function checkExpiry() {
      const now = Date.now();
      setCheckedAt(now);
      const next = Math.min(...expiries.filter((expiry) => expiry > now));
      if (Number.isFinite(next)) timer = setTimeout(checkExpiry, Math.min(next - now, 6 * 60 * 60_000));
    }
    checkExpiry();
    return () => clearTimeout(timer);
  }, [expiries]));
  return Math.max(checkedAt, Date.now());
}

function AirportNotices({ flight, notices }: { flight: Flight; notices: FlightNotice[] }) {
  const { colors } = usePalette();
  return (
    <View style={layout.section}>
      <Heading>Airport notices</Heading>
      <Body>These notices describe airport operations. Check your airline for flight-specific updates.</Body>
      {notices.map((notice) => {
        const departure = notice.operation === 'departure';
        const zone = departure ? flight.originTimeZone : flight.destinationTimeZone;
        const time = (value: string) => {
          const timeZone = zone || 'UTC';
          return `${formatDay(value, timeZone)} at ${formatTime(value, timeZone)}${zone ? '' : ' UTC'}`;
        };
        const window = [notice.startsAt && `Published start ${time(notice.startsAt)}`,
          notice.endsAt && `Published end ${time(notice.endsAt)}`].filter(Boolean).join(' · ');
        return (
          <View key={`${notice.source}:${notice.id}:${notice.operation}`} style={[styles.event, { borderColor: colors.divider }]}>
            <Text style={[styles.strong, { color: colors.text }]}>{notice.airport} · {departure ? 'Departure' : 'Arrival'} airport</Text>
            <Body>{notice.summary}</Body>
            <Body>{noticeSourceLabels[notice.source] ?? 'Airport notice feed'} · Observed {time(notice.observedAt)}</Body>
            {!!window && <Body>{window}</Body>}
          </View>
        );
      })}
    </View>
  );
}

const radarInterval = 8_000;
const radarPageInterval = 1_000;
const radarCompletedRetries = 2;
const radarMaxPoints = 512;
const radarPageSize = 256;

const radarSources: Record<string, string> = {
  eurocontrol_nm: 'EUROCONTROL',
  faa_swim: 'FAA SWIM',
  flightaware_aeroapi: 'FlightAware',
  operator_readsb: 'Local ADS-B receiver',
  plano_demo: 'Demo data',
};

function radarInstant(value: string) {
  const fraction = /\.(\d{1,9})(?=Z$|[+-]\d{2}:\d{2}$)/.exec(value);
  if (!fraction) return { millis: Date.parse(value), nanos: 0 };
  const digits = (fraction[1] ?? '').padEnd(9, '0');
  const isoMillis = `${value.slice(0, fraction.index)}.${digits.slice(0, 3)}${value.slice(fraction.index + fraction[0].length)}`;
  return { millis: Date.parse(isoMillis), nanos: Number(digits.slice(3)) };
}

function radarOrder(next: string, previous: string) {
  const a = radarInstant(next), b = radarInstant(previous);
  return a.millis === b.millis ? a.nanos - b.nanos : a.millis - b.millis;
}

export function mergeRadarPositions(...groups: (readonly Position[] | undefined)[]) {
  const byTime = new Map<string, { millis: number; nanos: number; point: Position }>();
  const latestAllowed = Date.now() + 30_000;
  for (const group of groups) {
    for (const point of group ?? []) {
      const { millis, nanos } = radarInstant(point.at);
      if (!Number.isFinite(millis) || millis > latestAllowed || !Number.isFinite(point.latitude) || !Number.isFinite(point.longitude)
        || Math.abs(point.latitude) > 90 || Math.abs(point.longitude) > 180) continue;
      const key = `${millis}:${nanos}`;
      const previous = byTime.get(key)?.point;
      const sameFix = previous && previous.source === point.source && previous.latitude === point.latitude && previous.longitude === point.longitude;
      byTime.set(key, { millis, nanos, point: sameFix ? {
        ...point,
        altitudeFeet: point.altitudeFeet ?? previous.altitudeFeet,
        groundSpeedKnots: point.groundSpeedKnots ?? previous.groundSpeedKnots,
        heading: point.heading ?? previous.heading,
        verticalRateFpm: point.verticalRateFpm ?? previous.verticalRateFpm,
        verticalTrend: point.verticalTrend ?? previous.verticalTrend,
      } : point });
    }
  }
  const ordered = [...byTime.values()].sort((a, b) => a.millis - b.millis || a.nanos - b.nanos);
  if (ordered.length <= radarMaxPoints) return ordered.map(({ point }) => point);

  // Retain the trajectory endpoints and spread the rest across observation
  // time so a dense stream of new fixes does not crowd out the early flight.
  const first = ordered[0]!;
  const last = ordered[ordered.length - 1]!;
  const elapsedNanos = ({ millis, nanos }: typeof first) => (millis - first.millis) * 1_000_000 + nanos - first.nanos;
  const span = elapsedNanos(last);
  const bucketCount = radarMaxPoints - 2;
  // Double the bucket width only when needed. As the flight grows, existing
  // buckets stay aligned so repeated polling does not thin older segments.
  const bucketWidth = 2 ** Math.ceil(Math.log2(span / bucketCount));
  const sampled = [first.point];
  let previousBucket = -1;
  for (let index = 1; index < ordered.length - 1; index += 1) {
    const sample = ordered[index]!;
    const bucket = Math.min(bucketCount - 1, Math.floor(elapsedNanos(sample) / bucketWidth));
    if (bucket === previousBucket) sampled[sampled.length - 1] = sample.point;
    else sampled.push(sample.point);
    previousBucket = bucket;
  }
  sampled.push(last.point);
  return sampled;
}

function privateRadarUpdate(known: RadarCursor, revision?: string, cursor?: string, positions?: Position[]) {
  const replace = revision !== known.revision;
  const page = revision === undefined ? [] : positions ?? [];
  if (!replace && page.length === 0) return;
  const after = page.length ? cursor ?? mergeRadarPositions(page).at(-1)?.at : undefined;
  return {
    cursor: { after: replace ? after : after ?? known.after, revision },
    page,
    replace,
    hasMore: !replace && page.length === radarPageSize && !!after && after !== known.after,
  };
}

function radarValue(value: number | undefined, unit: string) {
  return typeof value === 'number' && Number.isFinite(value)
    ? `${Math.round(value).toLocaleString()} ${unit}` : undefined;
}

function RadarReadout({ position }: { position: Position }) {
  const { colors } = usePalette();
  const rate = position.verticalRateFpm;
  const hasRate = typeof rate === 'number' && Number.isFinite(rate);
  const trend = hasRate ? (rate > 0 ? 'climbing' : rate < 0 ? 'descending' : 'level') : position.verticalTrend;
  const motion = trend === 'climbing' ? 'Climbing' : trend === 'descending' ? 'Descending' : trend === 'level' ? 'Level' : undefined;
  const metrics = [
    { label: 'Altitude', value: radarValue(position.altitudeFeet, 'ft') },
    { label: 'Ground speed', value: radarValue(position.groundSpeedKnots, 'kt') },
    { label: 'Vertical motion', value: hasRate ? `${motion} · ${radarValue(Math.abs(rate), 'ft/min')}` : motion },
  ];
  const latitude = `${Math.abs(position.latitude).toFixed(2)}° ${position.latitude < 0 ? 'S' : 'N'}`;
  const longitude = `${Math.abs(position.longitude).toFixed(2)}° ${position.longitude < 0 ? 'W' : 'E'}`;
  const source = (position.source && radarSources[position.source]) || 'Position report';
  return (
    <View style={[styles.radarReadout, { borderColor: colors.divider }]}>
      <View style={styles.radarMetrics}>
        {metrics.map(({ label, value }) => value && (
          <View key={label} style={styles.radarMetric}>
            <Text style={{ color: colors.muted }}>{label}</Text>
            <Text style={[styles.radarValue, { color: colors.text }]}>{value}</Text>
          </View>
        ))}
      </View>
      <Body>{latitude} · {longitude}</Body>
      <Body>{source} · Observed {formatTime(position.at, 'UTC')} UTC</Body>
    </View>
  );
}

function FlightRadar({ api, flight, tracked, history, detailRevision, refreshFlight }: {
  api: PlanoAPI;
  flight: Flight;
  tracked: boolean;
  history?: Position[];
  detailRevision?: string;
  refreshFlight: () => void;
}) {
  const id = flight.id;
  const inFlight = tracked && (flight.status === 'airborne' || flight.status === 'diverted') && !flight.actualLanding && !flight.actualArrival;
  const canReadRadar = inFlight || tracked && !!flight.actualTakeoff;
  const receiverCursor = useRef<RadarCursor>({});
  const swimCursor = useRef<RadarCursor>({});
  const eurocontrolCursor = useRef<RadarCursor>({});
  const providerRevision = useRef<string>(undefined);
  const [receiver, setReceiver] = useState<{ revision?: string; positions: Position[] }>({ positions: [] });
  const [swimPositions, setSWIMPositions] = useState<Position[]>([]);
  const [eurocontrolPositions, setEurocontrolPositions] = useState<Position[]>([]);
  const [provider, setProvider] = useState<{ revision: string; positions: Position[] }>();
  const [privateScopeObservedAt, setPrivateScopeObservedAt] = useState<string>();
  const [stream, setStream] = useState<'ready' | 'unavailable' | 'interrupted'>();
  const [checkedAt, setCheckedAt] = useState(0);
  const replacingAircraft = receiver.revision !== undefined && detailRevision !== receiver.revision;
  const waitingForPrivateScope = privateScopeObservedAt !== undefined && radarOrder(privateScopeObservedAt, flight.observedAt) > 0;
  const points = useMemo(() => {
    if (replacingAircraft) return [];
    const eurocontrol = waitingForPrivateScope ? undefined : eurocontrolPositions;
    const swim = waitingForPrivateScope ? undefined : swimPositions;
    const providerOrder = provider ? radarOrder(provider.revision, flight.observedAt) : NaN;
    if (provider && providerOrder > 0) {
      // A newer provider snapshot can correct or remove an earlier fix.
      const localHistory = history?.filter((point) => point.source === 'operator_readsb');
      return mergeRadarPositions(eurocontrol, swim, provider.positions, localHistory, receiver.positions);
    }
    // Flight detail can trim early provider fixes when its receiver track is
    // dense. Supplement it with the separately fetched same-revision track.
    const providerHistory = providerOrder === 0 ? provider?.positions : undefined;
    return mergeRadarPositions(eurocontrol, swim, providerHistory, history, receiver.positions);
  }, [provider, flight.observedAt, history, receiver.positions, swimPositions, eurocontrolPositions, replacingAircraft, waitingForPrivateScope]);

  useFocusEffect(useCallback(() => {
    if (!canReadRadar) return;
    let focused = true;
    let foreground = AppState.currentState == null || AppState.currentState === 'active';
    let inProgress = false;
    let supported = true;
    let consecutiveFailures = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function load() {
      if (!focused || !foreground || inProgress || !supported) return;
      inProgress = true;
      let delay = radarInterval;
      try {
        const knownReceiver = receiverCursor.current;
        const knownSWIM = swimCursor.current;
        const knownEurocontrol = eurocontrolCursor.current;
        const result = await api.radar(id, knownReceiver.after, providerRevision.current, knownReceiver.revision, knownSWIM, knownEurocontrol);
        if (!focused || !foreground) return;
        setCheckedAt(Date.now());
        if (!result) {
          supported = false;
          setStream('unavailable');
          swimCursor.current = {};
          setSWIMPositions([]);
          eurocontrolCursor.current = {};
          setEurocontrolPositions([]);
          return;
        }
        setStream('ready');
        const nextRevision = result.providerRevision;
        if (nextRevision && nextRevision !== providerRevision.current) {
          providerRevision.current = nextRevision;
          setProvider({ revision: nextRevision, positions: result.providerPositions ?? [] });
        }
        const nextReceiverRevision = result.receiverRevision;
        const replaceReceiver = nextReceiverRevision !== undefined && nextReceiverRevision !== knownReceiver.revision;
        if (replaceReceiver || result.positions.length) {
          const nextCursor = result.positions.length ? result.cursor ?? mergeRadarPositions(result.positions).at(-1)?.at : undefined;
          if (!replaceReceiver && result.positions.length === radarPageSize && nextCursor && nextCursor !== knownReceiver.after) delay = radarPageInterval;
          const receiverRevision = nextReceiverRevision ?? knownReceiver.revision;
          receiverCursor.current = { after: replaceReceiver ? nextCursor : nextCursor ?? knownReceiver.after, revision: receiverRevision };
          setReceiver((previous) => ({
            revision: receiverRevision,
            positions: mergeRadarPositions(replaceReceiver ? undefined : previous.positions, result.positions),
          }));
        }
        const swim = privateRadarUpdate(knownSWIM, result.swimRevision, result.swimCursor, result.swimPositions);
        if (swim) {
          swimCursor.current = swim.cursor;
          setSWIMPositions((previous) => mergeRadarPositions(swim.replace ? undefined : previous, swim.page));
        }
        const eurocontrol = privateRadarUpdate(knownEurocontrol, result.eurocontrolRevision, result.eurocontrolCursor, result.eurocontrolPositions);
        if (eurocontrol) {
          eurocontrolCursor.current = eurocontrol.cursor;
          setEurocontrolPositions((previous) => mergeRadarPositions(eurocontrol.replace ? undefined : previous, eurocontrol.page));
        }
        if (swim?.hasMore || eurocontrol?.hasMore) delay = radarPageInterval;
        const changedPrivateScope = (swim?.replace && knownSWIM.revision !== undefined && result.swimRevision !== undefined) ||
          (eurocontrol?.replace && knownEurocontrol.revision !== undefined && result.eurocontrolRevision !== undefined);
        const refreshPrivateScope = changedPrivateScope && result.providerRevision && radarOrder(result.providerRevision, flight.observedAt) > 0;
        if (refreshPrivateScope) setPrivateScopeObservedAt(result.providerRevision);
        if ((replaceReceiver && detailRevision !== nextReceiverRevision) || refreshPrivateScope) refreshFlight();
        consecutiveFailures = 0;
      } catch {
        delay *= 3;
        if (focused && foreground) {
          consecutiveFailures += 1;
          setCheckedAt(Date.now());
          setStream('interrupted');
        }
      } finally {
        inProgress = false;
        const retry = consecutiveFailures > 0 && consecutiveFailures <= radarCompletedRetries;
        if (focused && foreground && supported && (inFlight || delay === radarPageInterval || retry)) {
          timer = setTimeout(() => { void load(); }, delay);
        }
      }
    }
    const subscription = AppState.addEventListener('change', (state) => {
      foreground = state === 'active';
      clearTimeout(timer);
      if (foreground) void load();
    });
    void load();
    return () => {
      focused = false;
      clearTimeout(timer);
      subscription.remove();
    };
  }, [api, id, inFlight, canReadRadar, flight.observedAt, detailRevision, refreshFlight]));

  const latest = points.at(-1);
  if (!inFlight && !latest) return null;
  const age = latest ? checkedAt - radarInstant(latest.at).millis : Number.POSITIVE_INFINITY;
  const fresh = inFlight && stream === 'ready' && age >= -30_000 && age <= 120_000;
  const note = replacingAircraft ? 'Refreshing flight details before showing radar.'
    : stream === 'interrupted' ? 'Live updates are temporarily unavailable.'
    : stream === 'unavailable' ? 'Live positions are unavailable on this server.'
    : latest ? 'Waiting for a fresh position.' : 'Waiting for the first position.';
  return (
    <View style={layout.section}>
      <Heading>{fresh ? 'Live radar' : latest ? 'Recorded flight path' : 'Flight radar'}</Heading>
      {latest && (
        <>
          <RadarRouteMap label={`Flight path from ${flight.origin} to ${flight.destination}`} positions={points} />
          <RadarReadout position={latest} />
        </>
      )}
      {inFlight && !fresh && <Body>{note}</Body>}
      <RadarMetricChart label="Altitude (ft)" positions={points} metric="altitudeFeet" />
      <RadarMetricChart label="Ground speed (kt)" positions={points} metric="groundSpeedKnots" />
    </View>
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
      {canCustomizeAndroidWidget && <WidgetSettings />}
    </Page>
  );
}

function WidgetSettings() {
  const setting = useRemote(androidWidgetDetailsEnabled);
  const [saving, setSaving] = useState(false);
  const [issue, setIssue] = useState<string>();
  const editing = useRef(false);

  async function toggle() {
    if (editing.current || setting.pending || setting.value === undefined) return;
    editing.current = true;
    setSaving(true);
    setIssue(undefined);
    try {
      const next = !setting.value;
      await setAndroidWidgetDetailsEnabled(next);
      setting.setValue(next);
    } catch {
      setIssue('Couldn’t update the widget setting. Try again.');
      setting.refresh();
    } finally {
      editing.current = false;
      setSaving(false);
    }
  }

  return (
    <View style={layout.section}>
      <Heading>Android flight widget</Heading>
      <Body>Flight updates can show the gate, terminal, and seat on this widget. Anyone who can see your lock screen may see these details.</Body>
      {setting.value !== undefined && (
        <Action secondary label={setting.value ? 'Hide flight details on widget' : 'Show flight details on widget'}
          onPress={toggle} busy={saving} disabled={setting.pending} />
      )}
      {setting.pending && setting.value === undefined && <Loading />}
      {setting.error && <Action secondary label="Reload widget setting" onPress={setting.refresh} disabled={setting.pending || saving} />}
      <ErrorMessage message={issue ?? setting.error} />
    </View>
  );
}

function AlertsSettings({ api }: { api: PlanoAPI }) {
  const [pending, setPending] = useState(false);
  const [issue, setIssue] = useState<string>();
  const [enabled, setEnabled] = useState(false);
  useEffect(() => {
    flightAlertsEnabled(api).then(setEnabled, (cause: unknown) => setIssue(errorText(cause)));
  }, [api]);

  async function toggle() {
    setPending(true);
    setIssue(undefined);
    try {
      await changeFlightAlerts(api, !enabled);
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      setEnabled(await flightAlertsEnabled(api).catch(() => enabled));
      setPending(false);
    }
  }
  return (
    <View style={layout.section}>
      <Heading>Flight alerts</Heading>
      <Body>Get gate, delay, and arrival updates from this server.</Body>
      <Action icon="bell" secondary label={enabled ? 'Turn off flight alerts' : 'Enable flight alerts'}
        onPress={toggle} busy={pending} />
      {enabled && <Body>Flight alerts are enabled for this server.</Body>}
      <ErrorMessage message={issue} />
    </View>
  );
}

const styles = StyleSheet.create({
  event: { borderBottomWidth: StyleSheet.hairlineWidth, paddingBottom: 12, gap: 5 },
  eventTitle: { flexDirection: 'row', justifyContent: 'space-between', flexWrap: 'wrap', gap: 8 },
  radarReadout: { borderBottomWidth: StyleSheet.hairlineWidth, paddingBottom: 12, gap: 6 },
  radarMetrics: { flexDirection: 'row', flexWrap: 'wrap', columnGap: 20, rowGap: 8 },
  radarMetric: { gap: 2 },
  radarValue: { fontSize: 18, fontWeight: '600', fontVariant: ['tabular-nums'] },
  rotationSummary: { gap: 4 },
  strong: { fontSize: 16, fontWeight: '600' },
  weatherHeading: { fontSize: 18, fontWeight: '700' },
  historicalRisk: { gap: 4 },
  registryNotice: { gap: 4 },
});
