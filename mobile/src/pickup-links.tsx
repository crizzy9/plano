import { useEffect, useRef, useState } from 'react';
import { View } from 'react-native';
import type { PickupLink, PlanoAPI } from './api';
import { errorText, formatDay, formatTime } from './flight';
import { Action, Body, ErrorMessage, Heading, Loading, layout } from './ui';

export function PickupLinks({ api, flightId, refreshRevision }: { api: PlanoAPI; flightId: string; refreshRevision: number }) {
  const [open, setOpen] = useState(false);
  const [links, setLinks] = useState<PickupLink[]>();
  const [listError, setListError] = useState<string>();
  const [revokeError, setRevokeError] = useState<string>();
  const [revokeId, setRevokeId] = useState<string>();
  const [revision, setRevision] = useState(0);
  const mutation = useRef({ pending: false, version: 0 });
  const error = revokeError ?? listError;

  useEffect(() => {
    if (!open || mutation.current.pending) return;
    let active = true;
    const version = mutation.current.version;
    const current = () => active && version === mutation.current.version;
    setListError(undefined);
    setLinks(undefined);
    void api.pickupLinks(flightId).then((result) => {
      if (current()) setLinks(result.links);
    }).catch((cause: unknown) => {
      if (current()) setListError(errorText(cause));
    });
    return () => { active = false; };
  }, [api, flightId, open, refreshRevision, revision]);

  async function revoke(id: string) {
    if (mutation.current.pending) return;
    // Invalidate any read that began before the DELETE and defer new reads
    // until it settles, even if the panel closes or the parent refreshes.
    mutation.current.pending = true;
    mutation.current.version += 1;
    setRevokeId(id);
    setRevokeError(undefined);
    try {
      await api.revokePickupLink(id);
      setLinks((current) => current?.filter((link) => link.id !== id));
    } catch (cause) {
      setRevokeError(errorText(cause));
    } finally {
      mutation.current.pending = false;
      setRevokeId(undefined);
      setRevision((value) => value + 1);
    }
  }

  return (
    <View style={layout.section}>
      <Action label={open ? 'Hide pickup links' : 'Manage pickup links'} onPress={() => {
        setRevokeError(undefined);
        setOpen((value) => !value);
      }} secondary />
      {open && (
        <>
          <Heading>Public pickup links</Heading>
          <Body>Anyone with a link can see this flight’s arrival status until the link expires or is revoked.</Body>
          {!links && !error && <Loading />}
          {links?.length === 0 && <Body>No active pickup links.</Body>}
          {links?.map((link, index) => (
            <View key={link.id} style={layout.section}>
              <Body>{link.createdAt ? `Created ${formatDay(link.createdAt)} at ${formatTime(link.createdAt)}` : `Pickup link ${index + 1}`}</Body>
              <Body>Expires {formatDay(link.expiresAt)} at {formatTime(link.expiresAt)}</Body>
              <Action label="Revoke link" secondary busy={revokeId === link.id} disabled={revokeId !== undefined}
                onPress={() => void revoke(link.id)} />
            </View>
          ))}
          <ErrorMessage message={error} />
          <Action label="Refresh links" secondary disabled={revokeId !== undefined} onPress={() => {
            setRevokeError(undefined);
            setRevision((value) => value + 1);
          }} />
        </>
      )}
    </View>
  );
}
