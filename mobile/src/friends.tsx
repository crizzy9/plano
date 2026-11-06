import { useCallback, useRef, useState, type ReactNode } from 'react';
import { useFocusEffect } from '@react-navigation/native';
import { AppState, Platform, Share, StyleSheet, Switch, Text, View } from 'react-native';
import type { Flight, Friend, FriendInvite, PlanoAPI } from './api';
import { arrival, departure, errorText, flightStatus, formatDay, formatTime } from './flight';
import { Page, useRemote } from './screens';
import { usePalette } from './theme';
import { Action, Body, ErrorMessage, Heading, Input, Loading, layout } from './ui';

type Panel = 'invite' | 'accept';

export function FriendsScreen({ api }: { api: PlanoAPI }) {
  const { colors } = usePalette();
  const loadFriends = useCallback(() => api.friends(), [api]);
  const loadTracked = useCallback(() => api.tracked(), [api]);
  const remote = useRemote(loadFriends, true, 45_000);
  const refreshFriends = remote.refresh;
  useFocusEffect(useCallback(() => {
    const subscription = AppState.addEventListener('change', (state) => {
      if (state === 'active') refreshFriends();
    });
    return () => subscription.remove();
  }, [refreshFriends]));
  const [panel, setPanel] = useState<Panel>();
  const [alias, setAlias] = useState('');
  const [code, setCode] = useState('');
  const [invite, setInvite] = useState<FriendInvite>();
  const [editing, setEditing] = useState<string>();
  const [removing, setRemoving] = useState<string>();
  const [issue, setIssue] = useState<string>();
  const [notice, setNotice] = useState<string>();
  const [working, setWorking] = useState<string>();
  const busy = useRef(false);

  const outgoing = remote.value?.friends.filter((friend) => friend.direction === 'outgoing') ?? [];
  const incoming = remote.value?.friends.filter((friend) => friend.direction === 'incoming') ?? [];
  const editingFriend = outgoing.find(({ id }) => id === editing);
  const tracked = useRemote(loadTracked, !!editingFriend);
  const listedFlightIds = new Set(tracked.value?.map(({ id }) => id));
  const unlistedGrants = tracked.value || tracked.error
    ? editingFriend?.flightIds.filter((id) => !listedFlightIds.has(id)) ?? [] : [];

  function openPanel(next?: Panel) {
    setPanel(next);
    setInvite(undefined);
    setCode('');
    setIssue(undefined);
    setNotice(undefined);
  }

  async function perform(key: string, operation: () => Promise<void>) {
    if (busy.current) return;
    busy.current = true;
    setWorking(key);
    setIssue(undefined);
    setNotice(undefined);
    try {
      await operation();
    } catch (cause) {
      setIssue(errorText(cause));
    } finally {
      busy.current = false;
      setWorking(undefined);
    }
  }

  async function createInvite() {
    const name = alias.trim();
    if (!name) return;
    await perform('invite', async () => {
      const result = await api.createFriendInvite(name);
      setInvite(result);
    });
  }

  async function acceptInvite() {
    const name = alias.trim();
    const entered = code.trim();
    if (!name || !entered) return;
    await perform('accept', async () => {
      const { friend } = await api.acceptFriendInvite(entered, name);
      openPanel();
      remote.setValue((old) => {
        const friends = old?.friends.filter(({ id }) => id !== friend.id) ?? [];
        return { friends: [...friends, friend], flights: old?.flights ?? [] };
      });
      setNotice(`Connected with ${friend.alias}.`);
      remote.refresh();
    });
  }

  async function shareInvite() {
    if (!invite) return;
    await perform('share', async () => {
      if (Date.parse(invite.expiresAt) <= Date.now()) {
        throw new Error('This invite expired. Create a new one.');
      }
      const message = `Enter this single-use code in Plano’s Friends screen while connected to the same Plano server.\n\n${invite.code}`;
      if (Platform.OS === 'web') {
        if (typeof navigator.share === 'function') {
          try {
            await navigator.share({ title: 'Plano friend invite', text: message });
            return;
          } catch (cause) {
            if (cause instanceof Error && cause.name === 'AbortError') return;
          }
        }
        if (navigator.clipboard) {
          await navigator.clipboard.writeText(message);
          setNotice('Invite copied.');
          return;
        }
        throw new Error('Copy the invite code from the screen to share it.');
      }
      await Share.share({ title: 'Plano friend invite', message });
    });
  }

  async function setAccess(friend: Friend, flightId: string, display: string, shared: boolean) {
    await perform(`grant-${flightId}`, async () => {
      await api.setFriendFlight(friend.id, flightId, shared);
      remote.setValue((old) => old && ({
        ...old,
        friends: old.friends.map((entry) => {
          if (entry.id !== friend.id) return entry;
          const flightIds = entry.flightIds.filter((id) => id !== flightId);
          return { ...entry, flightIds: shared ? [...flightIds, flightId] : flightIds };
        }),
      }));
      setNotice(shared ? `${display} is shared with ${friend.alias}.` : `Access to ${display} ended.`);
      remote.refresh();
    });
  }

  async function removeFriend(friend: Friend) {
    await perform(`remove-${friend.id}`, async () => {
      await api.removeFriend(friend.id);
      remote.setValue((old) => old && ({
        friends: old.friends.filter(({ id }) => id !== friend.id),
        flights: old.flights.filter(({ friendId }) => friendId !== friend.id),
      }));
      if (editing === friend.id) setEditing(undefined);
      setRemoving(undefined);
      setNotice(`Removed ${friend.alias}.`);
      remote.refresh();
    });
  }

  function removal(friend: Friend) {
    if (removing !== friend.id) {
      return <Action secondary label={`Remove ${friend.alias}`} disabled={!!working}
        onPress={() => setRemoving(friend.id)} />;
    }
    const consequence = friend.direction === 'outgoing'
      ? `${friend.alias} will lose access to the flights you shared through this connection.`
      : `The flights ${friend.alias} shared through this connection will disappear from Plano.`;
    return (
      <View style={styles.confirm}>
        <Body>{consequence}</Body>
        <View style={layout.actions}>
          <Action label="Remove friend" busy={working === `remove-${friend.id}`}
            disabled={!!working && working !== `remove-${friend.id}`} onPress={() => void removeFriend(friend)} />
          <Action label="Keep friend" secondary disabled={!!working} onPress={() => setRemoving(undefined)} />
        </View>
      </View>
    );
  }

  return (
    <Page refresh={remote.refresh} refreshing={remote.pending && !!remote.value}>
      <Body>Choose which flights friends can see. You and your friend need to connect to the same Plano server.</Body>
      <View style={layout.actions}>
        <Action icon="share" label="Invite a friend" disabled={!!working} onPress={() => openPanel('invite')} />
        <Action label="Enter invite code" secondary disabled={!!working} onPress={() => openPanel('accept')} />
      </View>
      {panel && (
        <View style={styles.form}>
          <Heading>{panel === 'invite' ? 'Create an invite' : 'Accept an invite'}</Heading>
          <Input label="Your display name" value={alias} onChangeText={setAlias} editable={!working}
            autoComplete="off" autoCapitalize="words" maxLength={64} />
          <Body>Your friend will see this name.</Body>
          {panel === 'accept' && (
            <Input label="Invite code" value={code} onChangeText={setCode} editable={!working}
              autoCorrect={false} autoCapitalize="none" autoComplete="off" maxLength={128} />
          )}
          {invite && (
            <View style={[styles.codePanel, { backgroundColor: colors.tintSoft }]}>
              <Text selectable style={[styles.code, { color: colors.text }]}>{invite.code}</Text>
              <Body>Single use · Expires {formatDay(invite.expiresAt)} at {formatTime(invite.expiresAt)}</Body>
              <Action label="Share invite code" icon="share" busy={working === 'share'}
                disabled={!!working && working !== 'share'} onPress={() => void shareInvite()} />
            </View>
          )}
          <View style={layout.actions}>
            <Action label={panel === 'invite' ? invite ? 'Create another code' : 'Create invite code' : 'Accept invite'}
              disabled={!alias.trim() || panel === 'accept' && !code.trim() || !!working && working !== panel}
              busy={working === panel} onPress={() => void (panel === 'invite' ? createInvite() : acceptInvite())} />
            <Action label="Close" secondary disabled={!!working} onPress={() => openPanel()} />
          </View>
        </View>
      )}
      <ErrorMessage message={issue || remote.error} />
      {notice && <Text accessibilityLiveRegion="polite" style={[styles.notice, { color: colors.tint }]}>{notice}</Text>}
      {remote.error && <Action label="Reload friends" secondary onPress={remote.refresh} />}
      {remote.pending && !remote.value && <Loading />}
      {remote.value && (
        <>
          <View style={layout.section}>
            <Heading>You share with</Heading>
            {!outgoing.length && <Body>Friends who accept your invite will appear here.</Body>}
            {outgoing.map((friend) => (
              <FriendGroup key={friend.id} alias={friend.alias}>
                <Body>{friend.flightIds.length === 1 ? '1 flight shared' : `${friend.flightIds.length} flights shared`}</Body>
                <View style={layout.actions}>
                  <Action label={editing === friend.id ? 'Close flight access' : `Choose flights for ${friend.alias}`}
                    secondary disabled={!!working} onPress={() => setEditing(editing === friend.id ? undefined : friend.id)} />
                  {removing !== friend.id && removal(friend)}
                </View>
                {removing === friend.id && removal(friend)}
                {editingFriend?.id === friend.id && (
                  <View style={styles.grants}>
                    <Body>Turn on access for the tracked flights you want to share.</Body>
                    <ErrorMessage message={tracked.error} />
                    {tracked.error && <Action label="Reload tracked flights" secondary onPress={tracked.refresh} />}
                    {tracked.pending && !tracked.value && <Loading />}
                    {tracked.value?.length === 0 && <Body>Track a flight in My flights before sharing it.</Body>}
                    {tracked.value?.map((flight) => (
                      <FlightGrant key={flight.id} flight={flight} shared={friend.flightIds.includes(flight.id)}
                        disabled={!!working} onChange={(shared) => void setAccess(friend, flight.id, flight.number, shared)} />
                    ))}
                    {unlistedGrants.map((id, index) => (
                      <View key={id} style={styles.confirm}>
                        <Body>Previously shared flight {index + 1} · Details unavailable</Body>
                        <Action label={`Revoke previously shared flight ${index + 1}`} secondary
                          disabled={!!working && working !== `grant-${id}`} busy={working === `grant-${id}`}
                          onPress={() => void setAccess(friend, id, 'this flight', false)} />
                      </View>
                    ))}
                  </View>
                )}
              </FriendGroup>
            ))}
          </View>
          <View style={layout.section}>
            <Heading>Shared with you</Heading>
            {!incoming.length && <Body>Accept an invite to see the flights a friend shares with you.</Body>}
            {incoming.map((friend) => {
              const flights = remote.error || remote.pending
                ? [] : remote.value?.flights.filter(({ friendId }) => friendId === friend.id) ?? [];
              return (
                <FriendGroup key={friend.id} alias={friend.alias}>
                  {remote.error ? <Body>Shared flights will appear when Plano reconnects.</Body>
                    : remote.pending ? <Body>Checking shared flight access…</Body>
                      : flights.length === 0 && <Body>No flights shared yet.</Body>}
                  {flights.map(({ flight }) => <SharedFlight key={flight.id} flight={flight} />)}
                  <View style={removing === friend.id ? undefined : layout.actions}>{removal(friend)}</View>
                </FriendGroup>
              );
            })}
          </View>
        </>
      )}
    </Page>
  );
}

function FriendGroup({ alias, children }: { alias: string; children: ReactNode }) {
  const { colors } = usePalette();
  return (
    <View style={[styles.friend, { borderColor: colors.divider }]}>
      <Text accessibilityRole="header" style={[styles.name, { color: colors.text }]}>{alias}</Text>
      {children}
    </View>
  );
}

function FlightGrant({ flight, shared, disabled, onChange }: {
  flight: Flight;
  shared: boolean;
  disabled: boolean;
  onChange: (shared: boolean) => void;
}) {
  const { colors } = usePalette();
  const date = formatDay(flight.scheduledDeparture, flight.originTimeZone);
  return (
    <View style={[styles.flightGrant, { backgroundColor: colors.surface, borderColor: colors.divider }]}>
      <View style={styles.flightInfo}>
        <Text style={[styles.flightTitle, { color: colors.text }]}>{flight.number} · {flight.origin} to {flight.destination}</Text>
        <Body>{date}</Body>
      </View>
      <View style={styles.switch}>
        <Switch accessibilityLabel={`Share ${flight.number} from ${flight.origin} to ${flight.destination} on ${date}`}
          value={shared} disabled={disabled} onValueChange={onChange}
          trackColor={{ false: colors.divider, true: colors.tint }} />
      </View>
    </View>
  );
}

function SharedFlight({ flight }: { flight: Flight }) {
  const { colors } = usePalette();
  return (
    <View style={[styles.sharedFlight, { backgroundColor: colors.surface, borderColor: colors.divider }]}>
      <Text style={[styles.flightTitle, { color: colors.text }]}>{flight.origin} to {flight.destination}</Text>
      <Body>{flight.number} · {formatDay(flight.scheduledDeparture, flight.originTimeZone)} · {flightStatus(flight.status)}</Body>
      <Body>Departure {formatTime(departure(flight), flight.originTimeZone)}
        {flight.departureGate && ` · Gate ${flight.departureGate}`}</Body>
      <Body>Arrival {formatTime(arrival(flight), flight.destinationTimeZone)}
        {flight.arrivalGate && ` · Gate ${flight.arrivalGate}`}</Body>
    </View>
  );
}

const styles = StyleSheet.create({
  form: { gap: 12, marginTop: 8 },
  codePanel: { gap: 10, padding: 14, borderRadius: 12, alignItems: 'flex-start' },
  code: { fontSize: 18, fontWeight: '700', fontVariant: ['tabular-nums'], flexShrink: 1 },
  notice: { fontSize: 15, lineHeight: 21 },
  friend: { gap: 9, paddingBottom: 14, borderBottomWidth: StyleSheet.hairlineWidth },
  name: { fontSize: 18, fontWeight: '700' },
  confirm: { gap: 10, paddingVertical: 6 },
  grants: { gap: 8, paddingTop: 6 },
  flightGrant: { minHeight: 62, flexDirection: 'row', alignItems: 'center', gap: 8, paddingHorizontal: 10,
    borderBottomWidth: StyleSheet.hairlineWidth },
  flightInfo: { flex: 1, gap: 3 },
  flightTitle: { fontSize: 16, fontWeight: '600' },
  switch: { minWidth: 48, minHeight: 48, justifyContent: 'center', alignItems: 'center' },
  sharedFlight: { gap: 5, padding: 12, borderBottomWidth: StyleSheet.hairlineWidth },
});
