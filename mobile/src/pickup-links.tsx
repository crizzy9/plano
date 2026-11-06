import { useEffect, useState } from 'react';
import { View } from 'react-native';
import type { PickupLink, PlanoAPI } from './api';
import { errorText, formatDay, formatTime } from './flight';
import { Action, Body, ErrorMessage, Heading, Loading, layout } from './ui';

export function PickupLinks({ api, flightId }: { api: PlanoAPI; flightId: string }) {
  const [open, setOpen] = useState(false);
  const [links, setLinks] = useState<PickupLink[]>();
  const [error, setError] = useState<string>();
  const [revokeId, setRevokeId] = useState<string>();
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    if (!open) return;
    let current = true;
    setError(undefined);
    setLinks(undefined);
    void api.pickupLinks(flightId).then((result) => {
      if (current) setLinks(result.links);
    }).catch((cause: unknown) => {
      if (current) setError(errorText(cause));
    });
    return () => { current = false; };
  }, [api, flightId, open, revision]);

  async function revoke(id: string) {
    setRevokeId(id);
    setError(undefined);
    try {
      await api.revokePickupLink(id);
      setLinks((current) => current?.filter((link) => link.id !== id));
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setRevokeId(undefined);
    }
  }

  return (
    <View style={layout.section}>
      <Action label={open ? 'Hide pickup links' : 'Manage pickup links'} onPress={() => setOpen(!open)} secondary />
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
          <Action label="Refresh links" secondary disabled={revokeId !== undefined} onPress={() => setRevision((value) => value + 1)} />
        </>
      )}
    </View>
  );
}
