import { useCallback, useRef, useState } from 'react';
import { useFocusEffect } from '@react-navigation/native';
import { View } from 'react-native';
import type { PartnerBooking } from './api';
import { errorText } from './flight';
import { Action, Body, ErrorMessage, Heading, Input, layout } from './ui';

export function ReservationLookup({ disabled, resolve }: {
  disabled?: boolean;
  resolve: (details: PartnerBooking) => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [carrier, setCarrier] = useState('');
  const [locator, setLocator] = useState('');
  const [lastName, setLastName] = useState('');
  const [issue, setIssue] = useState<string>();
  const [working, setWorking] = useState(false);
  const [done, setDone] = useState(false);
  const active = useRef(false);
  const busy = useRef(false);

  const clear = useCallback(() => {
    setCarrier('');
    setLocator('');
    setLastName('');
  }, []);
  useFocusEffect(useCallback(() => {
    active.current = true;
    return () => {
      active.current = false;
      clear();
      setOpen(false);
      setIssue(undefined);
      setDone(false);
    };
  }, [clear]));

  const valid = /^(?:[A-Z0-9]{2}|[A-Z]{3})$/.test(carrier.trim().toUpperCase())
    && /^[A-Z0-9]{6}$/.test(locator.trim().toUpperCase()) && !!lastName.trim();

  async function submit() {
    if (disabled || busy.current || !valid || !active.current) return;
    const details = {
      carrier: carrier.trim().toUpperCase(),
      locator: locator.trim().toUpperCase(),
      lastName: lastName.trim(),
    };
    clear();
    busy.current = true;
    setWorking(true);
    setIssue(undefined);
    setDone(false);
    try {
      await resolve(details);
      if (active.current) {
        setOpen(false);
        setDone(true);
      }
    } catch (cause) {
      if (active.current) setIssue(errorText(cause));
    } finally {
      busy.current = false;
      setWorking(false);
    }
  }

  return (
    <View style={layout.section}>
      <Heading>Partner booking lookup</Heading>
      <Body>Look up an airline reservation through a licensed partner configured by your Plano server operator.</Body>
      {!open && <Action label="Open partner lookup" secondary disabled={disabled || working} onPress={() => {
        setOpen(true);
        setDone(false);
      }} />}
      {done && <Body>Lookup finished. Choose a matched flight in the results below.</Body>}
      {open && (
        <>
          <Body>When a partner is available, submitting sends your carrier code, confirmation code, and last name to that partner.</Body>
          <Input label="Carrier code" value={carrier} onChangeText={setCarrier} placeholder="UA or UAL"
            autoComplete="off" autoCapitalize="characters" autoCorrect={false} maxLength={3} editable={!working && !disabled} />
          <Input label="Airline confirmation code" value={locator} onChangeText={setLocator}
            autoComplete="off" autoCapitalize="characters" autoCorrect={false} secureTextEntry
            maxLength={6} editable={!working && !disabled} />
          <Input label="Traveler last name" value={lastName} onChangeText={setLastName}
            autoComplete="off" autoCapitalize="none" autoCorrect={false} secureTextEntry
            maxLength={64} editable={!working && !disabled} />
          <View style={layout.actions}>
            <Action label="Send details and look up booking" disabled={!valid || disabled} busy={working}
              onPress={() => void submit()} />
            <Action label="Cancel partner lookup" secondary disabled={working} onPress={() => {
              clear();
              setIssue(undefined);
              setOpen(false);
            }} />
          </View>
        </>
      )}
      <ErrorMessage message={issue} />
    </View>
  );
}
