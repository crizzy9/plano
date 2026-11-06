import { useCallback, useRef, useState } from 'react';
import { useFocusEffect } from '@react-navigation/native';
import { View } from 'react-native';
import type { PlanoAPI } from './api';
import { errorText, formatDay, formatTime } from './flight';
import { useRemote } from './screens';
import { Action, Body, ErrorMessage, Heading, Input, Loading, layout } from './ui';

const validFeedURL = /^(?:https|webcal):\/\/(?:www\.)?tripit\.com\/feed\/ical\/(?:[A-Za-z0-9_-]+\/){0,4}[A-Za-z0-9_.-]+\.ics$/;

function syncTime(instant: string) {
  return `${formatDay(instant)} at ${formatTime(instant)}`;
}

export function TripItSync({ api, disabled }: { api: PlanoAPI; disabled?: boolean }) {
  const load = useCallback((signal: AbortSignal) => api.tripItSubscription(signal), [api]);
  const remote = useRemote(load, !!api.server, 60_000);
  const [editing, setEditing] = useState(false);
  const [url, setURL] = useState('');
  const [issue, setIssue] = useState<string>();
  const [working, setWorking] = useState<'save' | 'remove'>();
  const busy = useRef(false);
  const subscription = remote.value;

  useFocusEffect(useCallback(() => () => {
    setURL('');
    setEditing(false);
  }, []));

  async function save() {
    if (busy.current || disabled || !subscription?.available) return;
    const feed = url.trim();
    if (feed.length > 1024 || !validFeedURL.test(feed) || feed.includes('..')) {
      setIssue('Enter the private calendar feed URL from your TripIt account.');
      return;
    }
    busy.current = true;
    setWorking('save');
    setIssue(undefined);
    try {
      await api.saveTripItSubscription(feed);
      remote.setValue({ available: true, enabled: true, status: 'pending' });
      setURL('');
      setEditing(false);
      remote.refresh();
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      busy.current = false;
      setWorking(undefined);
    }
  }

  async function stop() {
    if (busy.current || disabled || !subscription?.enabled) return;
    busy.current = true;
    setWorking('remove');
    setIssue(undefined);
    try {
      await api.removeTripItSubscription();
      remote.setValue({ available: subscription.available, enabled: false });
      setURL('');
      setEditing(false);
      remote.refresh();
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      busy.current = false;
      setWorking(undefined);
    }
  }

  const stateLabel = subscription?.status === 'unavailable' ? 'The last sync could not finish.'
    : subscription?.status === 'synced' ? 'Last sync completed.'
      : 'Waiting for a sync.';
  return (
    <View style={layout.section}>
      <Heading>Automatic TripIt sync</Heading>
      <Body>Your Plano server stores the private calendar feed URL and checks it on a schedule for flights to import.</Body>
      {(issue || remote.error) && <ErrorMessage message={issue || remote.error} />}
      {remote.error && <Action label="Reload TripIt sync status" secondary onPress={remote.refresh} />}
      {remote.pending && !subscription && <Loading />}
      {subscription && !subscription.available && <Body>Automatic TripIt sync is unavailable on this server.</Body>}
      {subscription?.enabled && (
        <>
          <Body>TripIt sync is enabled. {subscription.available ? stateLabel : 'The server has paused scheduled sync.'}</Body>
          {subscription.lastSyncAt && <Body>Last sync {syncTime(subscription.lastSyncAt)}</Body>}
          {subscription.available && subscription.nextSyncAt && <Body>Next check {syncTime(subscription.nextSyncAt)}</Body>}
          {(subscription.matched !== undefined || subscription.unmatched !== undefined) && (
            <Body>Last sync found {subscription.matched ?? 0} matched · {subscription.unmatched ?? 0} unmatched</Body>
          )}
          <View style={layout.actions}>
            {subscription.available && !editing && (
              <Action label="Change TripIt sync feed" secondary disabled={disabled || !!working} onPress={() => setEditing(true)} />
            )}
            <Action label="Stop TripIt sync" secondary busy={working === 'remove'}
              disabled={disabled || working === 'save'} onPress={() => void stop()} />
          </View>
        </>
      )}
      {subscription?.available && (!subscription.enabled || editing) && (
        <>
          <Input label="Private TripIt calendar URL" value={url} onChangeText={setURL} editable={!disabled && !working}
            autoComplete="off" autoCapitalize="none" autoCorrect={false} secureTextEntry maxLength={1024} />
          <View style={layout.actions}>
            <Action label={subscription.enabled ? 'Replace sync feed' : 'Enable TripIt sync'}
              disabled={disabled || !url.trim() || working === 'remove'} busy={working === 'save'}
              onPress={() => void save()} />
            {editing && <Action label="Cancel feed change" secondary disabled={!!working} onPress={() => {
              setURL('');
              setEditing(false);
            }} />}
          </View>
        </>
      )}
    </View>
  );
}
