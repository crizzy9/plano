import { useCallback, useRef, useState } from 'react';
import { useFocusEffect } from '@react-navigation/native';
import { randomUUID } from 'expo-crypto';
import { StyleSheet, View } from 'react-native';
import type { ManualPassportFlight, PlanoAPI } from './api';
import { errorText, validDate } from './flight';
import { Page, type Screen } from './screens';
import { Action, Body, ErrorMessage, Heading, Input, layout } from './ui';

const airportCode = /^(?:[A-Z0-9]{3}|[A-Z]{4})$/;
const flightNumber = /^[A-Z0-9]{2,3}\d{1,5}[A-Z]?$/;
const registration = /^[A-Z0-9][A-Z0-9-]{0,15}$/;
const maxMinutes = 2880;

function optionalMinutes(input: string, minimum: number) {
  const text = input.trim();
  if (!text) return undefined;
  const minutes = Number(text);
  return /^\d+$/.test(text) && minutes >= minimum && minutes <= maxMinutes ? minutes : NaN;
}

export function PassportEditorScreen({ api, navigation, route }: Screen<'PassportEditor'> & { api: PlanoAPI }) {
  const { date, number, flight, fromSearch } = route.params ?? {};
  const [draft, setDraft] = useState(() => ({
    date: flight?.historyDate ?? date ?? '',
    origin: flight?.origin ?? '',
    destination: flight?.destination ?? '',
    number: flight?.number ?? number ?? '',
    tailNumber: flight?.tailNumber ?? '',
    aircraftModel: flight?.aircraftModel ?? '',
    durationMinutes: flight?.historyDurationMinutes?.toString() ?? '',
    arrivalDelayMinutes: flight?.historyArrivalDelayMinutes?.toString() ?? '',
  }));
  const [detailsOpen, setDetailsOpen] = useState(() => !!(draft.number || draft.tailNumber || draft.aircraftModel
    || draft.durationMinutes || draft.arrivalDelayMinutes));
  const [saving, setSaving] = useState(false);
  const [issue, setIssue] = useState<string>();
  const [savedEarlier, setSavedEarlier] = useState(false);
  const active = useRef<symbol | undefined>(undefined);
  const firstSubmission = useRef<{ id: string; details: string } | undefined>(undefined);

  useFocusEffect(useCallback(() => {
    setSaving(false);
    return () => { active.current = undefined; };
  }, [api]));

  function openPassport() {
    if (fromSearch || !navigation.canGoBack()) navigation.replace('Passport');
    else navigation.goBack();
  }

  function change(field: keyof typeof draft, value: string) {
    setDraft((previous) => ({ ...previous, [field]: value }));
    setIssue(undefined);
  }

  async function save() {
    if (active.current || savedEarlier) return;
    setIssue(undefined);
    const date = draft.date.trim();
    const origin = draft.origin.trim().toUpperCase();
    const destination = draft.destination.trim().toUpperCase();
    if (!validDate(date) || date < '1900-01-01' || date >= new Date().toISOString().slice(0, 10)) {
      setIssue('Enter a past departure date from 1900 onward in YYYY-MM-DD format.');
      return;
    }
    if (!airportCode.test(origin) || !airportCode.test(destination) || origin === destination) {
      setIssue('Enter two different airport codes, such as JFK and EGLL.');
      return;
    }
    const number = draft.number.trim().toUpperCase().replace(/\s+/g, '');
    const tailNumber = draft.tailNumber.trim().toUpperCase();
    const durationMinutes = optionalMinutes(draft.durationMinutes, 1);
    const arrivalDelayMinutes = optionalMinutes(draft.arrivalDelayMinutes, 0);
    let detailIssue: string | undefined;
    if (number && !flightNumber.test(number)) detailIssue = 'Enter a flight number like UA123, or leave it blank.';
    else if (tailNumber && !registration.test(tailNumber)) detailIssue = 'Use letters, numbers, and hyphens in the aircraft registration.';
    else if (Number.isNaN(durationMinutes)) detailIssue = 'Enter a flight time between 1 and 2,880 minutes.';
    else if (Number.isNaN(arrivalDelayMinutes)) detailIssue = 'Enter an arrival delay between 0 and 2,880 minutes.';
    if (detailIssue) {
      setDetailsOpen(true);
      setIssue(detailIssue);
      return;
    }

    const details: ManualPassportFlight = {
      date, origin, destination, number: number || undefined, tailNumber: tailNumber || undefined,
      aircraftModel: draft.aircraftModel.trim() || undefined, durationMinutes, arrivalDelayMinutes,
    };
    const attempt = Symbol();
    active.current = attempt;
    setSaving(true);
    try {
      if (flight) {
        await api.updateManualPassportFlight(flight.id, details, flight.observedAt);
      } else {
        const serialized = JSON.stringify(details);
        const submission = firstSubmission.current ??= { id: randomUUID(), details: serialized };
        const result = await api.addManualPassportFlight(details, submission.id);
        if (active.current === attempt && !result.created && submission.details !== serialized) {
          setSavedEarlier(true);
          return;
        }
      }
      if (active.current === attempt) openPassport();
    } catch (cause) {
      if (active.current === attempt) setIssue(errorText(cause));
    } finally {
      if (active.current === attempt) {
        active.current = undefined;
        setSaving(false);
      }
    }
  }

  if (savedEarlier) {
    return <Page>
      <Heading>Flight saved earlier</Heading>
      <Body>Passport received an earlier save. Reopen the flight there to make further changes.</Body>
      <Action label="Open Passport" onPress={openPassport} />
    </Page>;
  }

  return (
    <Page>
      <Heading>Flight details</Heading>
      <Body>Save a completed flight in your private Passport history. Use the date at the departure airport.</Body>
      <Input label="Departure date" value={draft.date} onChangeText={(value) => change('date', value)}
        placeholder="YYYY-MM-DD" autoCorrect={false} maxLength={10} editable={!saving} />
      <View style={layout.fieldRow}>
        <View style={styles.airportField}>
          <Input label="Departure airport" value={draft.origin} onChangeText={(value) => change('origin', value)}
            placeholder="JFK" autoCapitalize="characters" autoCorrect={false} maxLength={4} editable={!saving} />
        </View>
        <View style={styles.airportField}>
          <Input label="Arrival airport" value={draft.destination} onChangeText={(value) => change('destination', value)}
            placeholder="EGLL" autoCapitalize="characters" autoCorrect={false} maxLength={4} editable={!saving} />
        </View>
      </View>
      <Action secondary label={detailsOpen ? 'Hide optional details' : 'Add optional details'}
        disabled={saving} onPress={() => setDetailsOpen(!detailsOpen)} />
      {detailsOpen && <View style={layout.section}>
        <Input label="Flight number" value={draft.number} onChangeText={(value) => change('number', value)}
          placeholder="UA123" autoCapitalize="characters" autoCorrect={false} maxLength={12} editable={!saving} />
        <Input label="Aircraft registration" value={draft.tailNumber} onChangeText={(value) => change('tailNumber', value)}
          autoCapitalize="characters" autoCorrect={false} maxLength={16} editable={!saving} />
        <Input label="Aircraft model" value={draft.aircraftModel} onChangeText={(value) => change('aircraftModel', value)}
          autoCorrect={false} maxLength={80} editable={!saving} />
        <Input label="Flight time in minutes" value={draft.durationMinutes} onChangeText={(value) => change('durationMinutes', value)}
          keyboardType="number-pad" maxLength={4} editable={!saving} />
        <Input label="Arrival delay in minutes" value={draft.arrivalDelayMinutes} onChangeText={(value) => change('arrivalDelayMinutes', value)}
          keyboardType="number-pad" maxLength={4} editable={!saving} />
        <Body>Leave unknown times blank. Enter 0 for an on-time or early arrival.</Body>
      </View>}
      <ErrorMessage message={issue} />
      <Action label={flight ? 'Save changes' : 'Save to Passport'} busy={saving} onPress={() => void save()} />
    </Page>
  );
}

const styles = StyleSheet.create({
  airportField: { flex: 1, minWidth: 124 },
});
