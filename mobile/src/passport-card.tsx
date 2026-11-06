import { useRef, useState } from 'react';
import { PixelRatio, Platform, Share, StyleSheet, Text, View } from 'react-native';
import type { Passport } from './api';
import { errorText } from './flight';
import { usePalette } from './theme';
import { Action, Body, ErrorMessage, Icon, layout } from './ui';

const maxImagePixels = 1080;
const shareRetentionMs = 5 * 60 * 1000;

type CardSize = { width: number; height: number };

export function cardCaptureSize({ width, height }: CardSize, density: number) {
  const side = Math.max(width, height);
  if (width <= 0 || height <= 0 || !Number.isFinite(side) || !Number.isFinite(density) || density <= 0) {
    throw new Error('The Passport card is still loading. Try again in a moment.');
  }
  const scale = maxImagePixels / (density * side);
  return { width: Math.max(1, Math.floor(width * scale)), height: Math.max(1, Math.floor(height * scale)) };
}

export function cardShareURL(uri: string) {
  // View Shot returns a raw temporary path on iOS and a file URI on Android.
  return uri.startsWith('/') ? 'file://' + uri.split('/').map(encodeURIComponent).join('/') : uri;
}

export function PassportCard({ stats }: { stats: Passport['stats'] }) {
  const { colors } = usePalette();
  const card = useRef<View>(null);
  const size = useRef<CardSize>({ width: 0, height: 0 });
  const inProgress = useRef(false);
  const [busy, setBusy] = useState<'image' | 'text'>();
  const [notice, setNotice] = useState<string>();
  const [issue, setIssue] = useState<string>();

  const formatter = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
  const metrics = [
    ['Completed flights', formatter.format(stats.flights)],
    ['Miles recorded', formatter.format(stats.miles)],
    ['Flight hours', formatter.format(stats.hours)],
    ['Aircraft', formatter.format(stats.aircraft)],
    ['Delay minutes', formatter.format(stats.delayMinutes)],
  ] as const;
  const message = ['My Plano Passport', ...metrics.map(([label, value]) => `${label}  ${value}`)].join('\n');

  async function shareText() {
    if (Platform.OS === 'web') {
      if (typeof navigator.share === 'function') {
        try {
          await navigator.share({ title: 'Plano Passport', text: message });
          return;
        } catch (error) {
          if (error instanceof Error && error.name === 'AbortError') return;
        }
      }
      if (navigator.clipboard) {
        await navigator.clipboard.writeText(message);
        setNotice('Flight stats copied.');
        return;
      }
      throw new Error('Your browser couldn’t open sharing. Select and copy the stats from the card.');
    }
    await Share.share({ title: 'Plano Passport', message });
  }

  async function share(kind: 'image' | 'text') {
    if (inProgress.current) return;
    inProgress.current = true;
    setBusy(kind);
    setNotice(undefined);
    setIssue(undefined);
    try {
      if (kind === 'image') {
        try {
          const sharing = await import('expo-sharing');
          if (!await sharing.isAvailableAsync()) throw new Error('File sharing is unavailable.');
          const { captureRef, releaseCapture } = await import('react-native-view-shot');
          const target = card.current;
          if (!target) throw new Error('The Passport card is still loading.');
          const uri = await captureRef(target, {
            ...cardCaptureSize(size.current, Platform.OS === 'ios' ? PixelRatio.get() : 1),
            format: 'png',
            result: 'tmpfile',
          });
          try {
            await sharing.shareAsync(cardShareURL(uri), { mimeType: 'image/png', UTI: 'public.png', dialogTitle: 'Share Plano Passport' });
          } catch (cause) {
            releaseCapture(uri);
            throw cause;
          }
          // The Android target can read the URI after the chooser returns.
          // Keep it available briefly. View Shot also uses the OS's temp cache.
          setTimeout(() => releaseCapture(uri), shareRetentionMs);
          return;
        } catch {
          setNotice('Image sharing was unavailable. Opened text sharing.');
        }
      }
      await shareText();
    } catch (cause) {
      setNotice(undefined);
      setIssue(errorText(cause));
    } finally {
      inProgress.current = false;
      setBusy(undefined);
    }
  }

  return (
    <View style={styles.section}>
      <View ref={card} collapsable={false} onLayout={({ nativeEvent }) => { size.current = nativeEvent.layout; }}
        style={[styles.card, { backgroundColor: colors.route }]}>
        <View style={[styles.titleRow, { borderColor: colors.routeMuted }]}>
          <Icon name="plane" size={22} color={colors.routeLine} />
          <Text accessibilityRole="header" style={[styles.title, { color: colors.routeText }]}>Plano Passport</Text>
        </View>
        {metrics.map(([label, value]) => (
          <View key={label} style={styles.metric}>
            <Text selectable style={[styles.label, { color: colors.routeMuted }]}>{label}</Text>
            <Text selectable style={[styles.number, { color: colors.routeText }]}>{value}</Text>
          </View>
        ))}
      </View>
      <View style={layout.actions}>
        {Platform.OS !== 'web' && (
          <Action icon="share" label="Share stats image" onPress={() => void share('image')}
            busy={busy === 'image'} disabled={!!busy} />
        )}
        <Action secondary label="Share as text" onPress={() => void share('text')}
          busy={busy === 'text'} disabled={!!busy} />
      </View>
      {notice && <Body>{notice}</Body>}
      <ErrorMessage message={issue} />
    </View>
  );
}

const styles = StyleSheet.create({
  section: { gap: 12 },
  card: { gap: 10, padding: 22, borderRadius: 14 },
  titleRow: {
    flexDirection: 'row', alignItems: 'center', gap: 8, borderBottomWidth: StyleSheet.hairlineWidth,
    paddingBottom: 14, marginBottom: 4,
  },
  title: { fontSize: 22, fontWeight: '700', flexShrink: 1 },
  metric: { flexDirection: 'row', justifyContent: 'space-between', flexWrap: 'wrap', alignItems: 'baseline', gap: 5 },
  label: { fontSize: 15 },
  number: { fontSize: 17, fontWeight: '700', fontVariant: ['tabular-nums'] },
});
