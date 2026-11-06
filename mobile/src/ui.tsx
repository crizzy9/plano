import { useEffect, useState, type ReactNode } from 'react';
import {
  ActivityIndicator, Pressable, StyleSheet, Text, TextInput, View, type TextInputProps,
} from 'react-native';
import Svg, { Path } from 'react-native-svg';
import type { Flight } from './api';
import {
  arrival, arrivalNote, departure, departureNote, flightStatus, formatDay, formatTime, progress,
} from './flight';
import { usePalette } from './theme';

export type IconName = 'search' | 'plane' | 'arrow' | 'share' | 'bell' | 'globe' | 'download';

const icons: Record<IconName, string> = {
  search: 'M15.5 15.5 21 21 M17 10.5a6.5 6.5 0 1 1-13 0 6.5 6.5 0 0 1 13 0',
  plane: 'M12 2v18 M12 7 3 13v2l9-3 9 3v-2l-9-6 M8 22l4-2 4 2',
  arrow: 'm9 6 6 6-6 6',
  share: 'M12 16V3 m-4 4 4-4 4 4 M7 10H4v11h16V10h-3',
  bell: 'M5 16h14l-2-3V9a5 5 0 0 0-10 0v4l-2 3 M10 20h4',
  globe: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0 M3 12h18 M12 3c-4 4-4 14 0 18 4-4 4-14 0-18',
  download: 'M12 3v13 m-4-4 4 4 4-4 M4 17v4h16v-4',
};

export function Icon({ name, color, size = 22 }: { name: IconName; color: string; size?: number }) {
  return (
    <Svg width={size} height={size} viewBox="0 0 24 24" accessible={false}>
      <Path d={icons[name]} stroke={color} strokeWidth={1.8} fill="none" strokeLinecap="round" strokeLinejoin="round" />
    </Svg>
  );
}

export function Action({ label, onPress, icon, secondary, busy, disabled }: {
  label: string;
  onPress: () => void;
  icon?: IconName;
  secondary?: boolean;
  busy?: boolean;
  disabled?: boolean;
}) {
  const { colors } = usePalette();
  const textColor = secondary ? colors.tint : colors.onTint;
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={label}
      accessibilityState={{ disabled: disabled || busy, busy }}
      onPress={onPress}
      disabled={disabled || busy}
      style={({ pressed }) => [
        styles.action,
        { backgroundColor: secondary ? colors.tintSoft : colors.tint, opacity: pressed || disabled ? 0.72 : 1 },
      ]}
    >
      {busy ? <ActivityIndicator color={textColor} /> : icon && <Icon name={icon} color={textColor} size={19} />}
      <Text style={[styles.actionText, { color: textColor }]}>{label}</Text>
    </Pressable>
  );
}

export function Heading({ children }: { children: ReactNode }) {
  const { colors } = usePalette();
  return <Text accessibilityRole="header" style={[styles.heading, { color: colors.text }]}>{children}</Text>;
}

export function Body({ children }: { children: ReactNode }) {
  const { colors } = usePalette();
  return <Text style={[styles.body, { color: colors.muted }]}>{children}</Text>;
}

export function ErrorMessage({ message }: { message: string | undefined }) {
  const { colors } = usePalette();
  return message ? (
    <Text accessibilityRole="alert" accessibilityLiveRegion="polite"
      style={[styles.error, { color: colors.alert, backgroundColor: colors.alertSoft }]}>{message}</Text>
  ) : null;
}

export function Input({ label, ...props }: TextInputProps & { label: string }) {
  const { colors } = usePalette();
  return (
    <View style={styles.inputGroup}>
      <Text style={[styles.label, { color: colors.text }]}>{label}</Text>
      <TextInput
        {...props}
        accessibilityLabel={label}
        placeholderTextColor={colors.muted}
        selectionColor={colors.tint}
        style={[styles.input, { color: colors.text, borderColor: colors.divider, backgroundColor: colors.surface }, props.style]}
      />
    </View>
  );
}

export function Loading() {
  const { colors } = usePalette();
  return <ActivityIndicator accessibilityLabel="Loading" size="large" color={colors.tint} style={styles.loading} />;
}

export function FlightRow({ flight, onPress }: { flight: Flight; onPress?: () => void }) {
  const { colors } = usePalette();
  const rowStyle = (pressed: boolean) => [styles.row, {
    backgroundColor: pressed ? colors.tintSoft : colors.surface,
    borderColor: colors.divider,
  }];
  const content = (
    <>
      <View style={styles.rowLabel}>
        <Text style={[styles.rowRoute, { color: colors.text }]}>{flight.origin} to {flight.destination}</Text>
        <Body>{flight.number} · {formatDay(flight.scheduledDeparture, flight.originTimeZone)}</Body>
      </View>
      <View style={styles.rowStatus}>
        <Text style={[styles.label, { color: colors.tint }]}>{flightStatus(flight.status)}</Text>
        <Body>{formatTime(departure(flight), flight.originTimeZone)}</Body>
      </View>
      {onPress && <Icon name="arrow" color={colors.muted} size={18} />}
    </>
  );
  return onPress ? (
    <Pressable onPress={onPress} accessibilityRole="button"
      accessibilityLabel={`${flight.number}, ${flight.origin} to ${flight.destination}, ${flightStatus(flight.status)}`}
      style={({ pressed }) => rowStyle(pressed)}>
      {content}
    </Pressable>
  ) : <View style={rowStyle(false)}>{content}</View>;
}

export function FlightStrip({ flight }: { flight: Flight }) {
  const { colors } = usePalette();
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (!['airborne', 'diverted'].includes(flight.status)) return;
    const interval = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(interval);
  }, [flight.status]);

  const fraction = progress(flight, now);
  const departureAirport = flight.departureTerminal && `Terminal ${flight.departureTerminal}`;
  const arrivalAirport = flight.arrivalTerminal && `Terminal ${flight.arrivalTerminal}`;

  return (
    <View style={[styles.strip, { backgroundColor: colors.route }]}>
      <View style={styles.stripTop}>
        <Text style={[styles.label, { color: colors.routeText }]}>{flight.number}</Text>
        <Text style={[styles.label, { color: colors.routeLine }]}>{flightStatus(flight.status)}</Text>
      </View>
      <View style={styles.airports}>
        <View style={styles.endpoint}>
          <Text style={[styles.code, { color: colors.routeText }]}>{flight.origin}</Text>
          <Text style={[styles.time, { color: colors.routeText }]}>{formatTime(departure(flight), flight.originTimeZone)}</Text>
          <Text style={[styles.meta, { color: colors.routeMuted }]}>{departureNote(flight)}</Text>
        </View>
        <Icon name="plane" color={colors.routeLine} size={21} />
        <View style={[styles.endpoint, styles.right]}>
          <Text style={[styles.code, { color: colors.routeText }]}>{flight.destination}</Text>
          <Text style={[styles.time, { color: colors.routeText }]}>{formatTime(arrival(flight), flight.destinationTimeZone)}</Text>
          <Text style={[styles.meta, { color: colors.routeMuted, textAlign: 'right' }]}>{arrivalNote(flight)}</Text>
        </View>
      </View>
      {fraction !== undefined && (
        <View accessibilityRole="progressbar"
          accessibilityLabel={['airborne', 'diverted'].includes(flight.status) ? 'Estimated flight progress' : 'Flight progress'}
          accessibilityValue={{ min: 0, max: 100, now: Math.round(fraction * 100) }}
          style={[styles.progress, { backgroundColor: '#547A6E' }]}>
          <View style={[styles.progressFill, { backgroundColor: colors.routeLine, width: `${fraction * 100}%` }]} />
        </View>
      )}
      <View style={styles.stripTop}>
        <Text style={[styles.meta, { color: colors.routeMuted }]}>
          {[departureAirport, flight.departureGate && `Gate ${flight.departureGate}`].filter(Boolean).join(' · ') || 'Departure gate pending'}
        </Text>
        <Text style={[styles.meta, { color: colors.routeMuted, textAlign: 'right' }]}>
          {[arrivalAirport, flight.arrivalGate && `Gate ${flight.arrivalGate}`].filter(Boolean).join(' · ') || 'Arrival gate pending'}
        </Text>
      </View>
    </View>
  );
}

export const layout = StyleSheet.create({
  screen: { flex: 1 },
  content: { alignSelf: 'center', width: '100%', maxWidth: 720, padding: 20, paddingBottom: 32, gap: 16 },
  section: { gap: 12, marginTop: 12 },
  actions: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  fieldRow: { flexDirection: 'row', flexWrap: 'wrap', gap: 12 },
});

const styles = StyleSheet.create({
  action: { minHeight: 48, paddingHorizontal: 16, paddingVertical: 10, borderRadius: 12, flexDirection: 'row',
    alignItems: 'center', justifyContent: 'center', gap: 8 },
  actionText: { fontSize: 15, fontWeight: '600', flexShrink: 1 },
  heading: { fontSize: 23, fontWeight: '700', letterSpacing: -0.35 },
  body: { fontSize: 15, lineHeight: 21 },
  error: { padding: 12, borderRadius: 8, fontSize: 15, lineHeight: 21 },
  inputGroup: { gap: 6 },
  label: { fontSize: 14, fontWeight: '600', fontVariant: ['tabular-nums'] },
  input: { minHeight: 50, borderWidth: 1, borderRadius: 10, paddingHorizontal: 14, paddingVertical: 10, fontSize: 17 },
  loading: { padding: 20 },
  row: { padding: 14, borderBottomWidth: StyleSheet.hairlineWidth, minHeight: 68, flexDirection: 'row',
    alignItems: 'center', justifyContent: 'space-between', gap: 10 },
  rowLabel: { flex: 1, gap: 4 },
  rowStatus: { alignItems: 'flex-end', gap: 4, flexShrink: 1 },
  rowRoute: { fontSize: 17, fontWeight: '700' },
  strip: { borderRadius: 18, padding: 18, gap: 16 },
  stripTop: { flexDirection: 'row', justifyContent: 'space-between', gap: 12 },
  airports: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', gap: 6 },
  endpoint: { flex: 1 },
  right: { alignItems: 'flex-end' },
  code: { fontSize: 37, fontWeight: '700', letterSpacing: -1.4 },
  time: { fontSize: 16, fontWeight: '600', fontVariant: ['tabular-nums'] },
  meta: { fontSize: 12, lineHeight: 17, flexShrink: 1 },
  progress: { height: 4, borderRadius: 2, overflow: 'hidden' },
  progressFill: { height: 4 },
});
