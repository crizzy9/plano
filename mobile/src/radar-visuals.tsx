import { useId, useMemo } from 'react';
import { StyleSheet, Text, View } from 'react-native';
import Svg, { Circle, ClipPath, Defs, G, Path, Rect, Use } from 'react-native-svg';
import type { Position } from './api';
import { landRings } from './passport-land';
import { usePalette } from './theme';

const mapWidth = 320;
const mapHeight = 184;
const mapInset = 16;
const chartWidth = 320;
const chartHeight = 80;
const chartInset = 5;
const metricGap = 5 * 60_000;

type XY = readonly [number, number];
type Metric = 'altitudeFeet' | 'groundSpeedKnots';

function pathPoint(command: 'M' | 'L', [x, y]: XY) {
  return `${command}${x.toFixed(2)} ${y.toFixed(2)}`;
}

const land = landRings.map((ring) => ring.map(([latitude, longitude], index) =>
  pathPoint(index === 0 ? 'M' : 'L', [longitude, -latitude])).join('') + 'Z').join('');

function routeGeometry(positions: readonly Position[]) {
  const coordinates: XY[] = [];
  let previousLongitude: number | undefined;
  for (const { latitude, longitude } of positions) {
    if (!Number.isFinite(latitude) || Math.abs(latitude) > 90 ||
      !Number.isFinite(longitude) || Math.abs(longitude) > 180) continue;
    // Keep each point in the world copy nearest the previous one.
    const unwrapped = previousLongitude === undefined ? longitude
      : longitude + 360 * Math.round((previousLongitude - longitude) / 360);
    coordinates.push([unwrapped, -latitude]);
    previousLongitude = unwrapped;
  }
  if (!coordinates.length) return undefined;

  const longitudes = coordinates.map(([longitude]) => longitude);
  const northings = coordinates.map(([, northing]) => northing);
  const west = Math.min(...longitudes), east = Math.max(...longitudes);
  const north = Math.min(...northings), south = Math.max(...northings);
  const centerX = (west + east) / 2, centerY = (north + south) / 2;
  // One scale for both axes, with longitude corrected for the viewport's latitude.
  const longitudeScale = Math.max(0.15, Math.cos(centerY * Math.PI / 180));
  const units = Math.min((mapWidth - 2 * mapInset) / ((east - west) * longitudeScale || 1),
    (mapHeight - 2 * mapInset) / Math.max(south - north, 8));
  const scaleX = units * longitudeScale;
  const offsetX = mapWidth / 2 - centerX * scaleX;
  const offsetY = mapHeight / 2 - centerY * units;
  const projected = coordinates.map(([x, y]): XY => [x * scaleX + offsetX, y * units + offsetY]);
  const firstWorld = Math.ceil((-offsetX / scaleX - 180) / 360);
  const lastWorld = Math.floor(((mapWidth - offsetX) / scaleX + 180) / 360);
  return {
    track: projected.map((point, index) => pathPoint(index === 0 ? 'M' : 'L', point)).join(''),
    latest: projected[projected.length - 1]!,
    transform: `matrix(${scaleX} 0 0 ${units} ${offsetX} ${offsetY})`,
    worlds: Array.from({ length: lastWorld - firstWorld + 1 }, (_, index) => 360 * (firstWorld + index)),
  };
}

export function RadarRouteMap({ positions, label }: { positions: readonly Position[]; label: string }) {
  const { colors } = usePalette();
  const instance = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const clipID = `radar-viewport-${instance}`;
  const landID = `radar-land-${instance}`;
  const geometry = useMemo(() => routeGeometry(positions), [positions]);
  if (!geometry) return null;
  const [x, y] = geometry.latest;

  return (
    <View style={styles.figure}>
      <View accessible accessibilityRole="image" accessibilityLabel={label}
        style={[styles.map, { backgroundColor: colors.route }]}>
        <Svg width="100%" height="100%" viewBox={`0 0 ${mapWidth} ${mapHeight}`} accessible={false}>
          <Defs>
            <ClipPath id={clipID}><Rect width={mapWidth} height={mapHeight} /></ClipPath>
            <Path id={landID} d={land} fill={colors.routeMuted} fillOpacity={0.25} />
          </Defs>
          <G clipPath={`url(#${clipID})`}>
            <G transform={geometry.transform}>
              {geometry.worlds.map((offset) => <Use key={offset} href={`#${landID}`} x={offset} />)}
            </G>
            <Path d={geometry.track} fill="none" stroke={colors.routeLine} strokeWidth={2.5}
              strokeLinecap="round" strokeLinejoin="round" />
            <Circle cx={x} cy={y} r={5} fill={colors.routeText} stroke={colors.route} strokeWidth={1.5} />
          </G>
        </Svg>
      </View>
      <Text style={[styles.caption, { color: colors.muted }]}>Map data from Natural Earth</Text>
    </View>
  );
}

function metricGeometry(positions: readonly Position[], metric: Metric) {
  const samples: XY[] = [];
  for (const position of positions) {
    // Charts use milliseconds even when the server sends a nanosecond cursor.
    const time = Date.parse(position.at.replace(/(\.\d{3})\d+/, '$1'));
    const value = position[metric];
    if (Number.isFinite(time) && typeof value === 'number' && Number.isFinite(value)) samples.push([time, value]);
  }
  if (samples.length < 2) return undefined;

  const first = samples[0]![0], last = samples[samples.length - 1]![0];
  if (last <= first) return undefined;
  const values = samples.map(([, value]) => value);
  const low = Math.min(...values), high = Math.max(...values);
  const dots: XY[] = [];
  const path = samples.map(([time, value], index) => {
    const point: XY = [chartInset + (time - first) / (last - first) * (chartWidth - 2 * chartInset),
      high === low ? chartHeight / 2 : chartHeight - chartInset - (value - low) / (high - low) * (chartHeight - 2 * chartInset)];
    const previous = samples[index - 1], next = samples[index + 1];
    const startsSegment = !previous || time - previous[0] > metricGap;
    if (startsSegment && (!next || next[0] - time > metricGap)) dots.push(point);
    return pathPoint(startsSegment ? 'M' : 'L', point);
  }).join('');
  return { path, dots, low, high, elapsed: last - first };
}

function elapsedLabel(millis: number) {
  if (millis < 60_000) return `${Math.max(1, Math.round(millis / 1000))} sec`;
  const minutes = Math.round(millis / 60_000);
  const hours = Math.floor(minutes / 60);
  return hours ? `${hours} hr ${minutes % 60} min` : `${minutes} min`;
}

export function RadarMetricChart({ positions, metric, label }: {
  positions: readonly Position[];
  metric: Metric;
  label: string;
}) {
  const { colors } = usePalette();
  const geometry = useMemo(() => metricGeometry(positions, metric), [positions, metric]);
  if (!geometry) return null;
  const low = geometry.low.toLocaleString(), high = geometry.high.toLocaleString();
  const range = geometry.high === geometry.low ? low : `${low} to ${high}`;
  const elapsed = elapsedLabel(geometry.elapsed);

  return (
    <View accessible accessibilityRole="image" accessibilityLabel={`${label}, ${range} over ${elapsed}`}
      style={styles.figure}>
      <Text style={[styles.label, { color: colors.text }]}>{label}</Text>
      <Svg width="100%" height={chartHeight} viewBox={`0 0 ${chartWidth} ${chartHeight}`}
        preserveAspectRatio="none" accessible={false}>
        <Path d="M5 5H315M5 40H315M5 75H315" stroke={colors.divider} />
        <Path d={geometry.path} stroke={colors.tint} strokeWidth={2} fill="none" strokeLinejoin="round" />
        {geometry.dots.map(([x, y], index) => <Circle key={index} cx={x} cy={y} r={2.5} fill={colors.tint} />)}
      </Svg>
      <View style={styles.axis}>
        <Text style={[styles.caption, { color: colors.muted }]}>{range}</Text>
        <Text style={[styles.caption, { color: colors.muted }]}>{elapsed} of observations</Text>
      </View>
    </View>
  );
}

const styles = StyleSheet.create({
  figure: { gap: 4 },
  map: { width: '100%', aspectRatio: mapWidth / mapHeight, borderRadius: 12, overflow: 'hidden' },
  label: { fontSize: 14, fontWeight: '600' },
  caption: { fontSize: 13, fontVariant: ['tabular-nums'], flexShrink: 1 },
  axis: { flexDirection: 'row', flexWrap: 'wrap', justifyContent: 'space-between', columnGap: 12, rowGap: 4 },
});
