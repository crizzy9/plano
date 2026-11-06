import { useId, useMemo, useState } from 'react';
import { Pressable, StyleSheet, Text, View } from 'react-native';
import Svg, { Circle, ClipPath, Defs, G, Path, Rect, Use } from 'react-native-svg';
import type { PassportSegment } from './api';
import { landRings } from './passport-land';
import { usePalette } from './theme';
import { Action, Body, layout } from './ui';

type Coordinate = readonly [number, number];
type Projection = 'globe' | 'flat';

const rad = Math.PI / 180;
const mapWidth = 360;
const mapHeight = 180;
const globeCenter = 108;
const globeRadius = 104;
const globeGrid = [
  ...[-60, -30, 0, 30, 60].map((lat) =>
    Array.from({ length: 91 }, (_, index): Coordinate => [lat, -180 + 4 * index])),
  ...[-180, -120, -60, 0, 60, 120].map((lon) =>
    Array.from({ length: 46 }, (_, index): Coordinate => [-90 + 4 * index, lon])),
];

const flatGrid = [
  ...[30, 60, 90, 120, 150].map((y) => `M0 ${y}H${mapWidth}`),
  ...[0, 60, 120, 180, 240, 300].map((x) => `M${x} 0V${mapHeight}`),
].join(' ');
const flatLand = landRings.map((ring) => {
  return ring.map(([lat, lon], index) => `${index === 0 ? 'M' : 'L'}${(lon + 180).toFixed(1)} ${(90 - lat).toFixed(1)}`).join(' ') + 'Z';
}).join(' ');

function chartPoint(command: 'M' | 'L', x: number, y: number) {
  return `${command}${x.toFixed(2)} ${y.toFixed(2)}`;
}

function wrapLongitude(value: number) {
  return ((value + 180) % 360 + 360) % 360 - 180;
}

export function flatLine(points: readonly Coordinate[]) {
  let path = '';
  let previous: Coordinate | undefined;
  for (const [lat, lon] of points) {
    if (!previous) {
      path = chartPoint('M', lon + 180, 90 - lat);
    } else {
      const [oldLat, oldLon] = previous;
      const delta = lon - oldLon;
      if (Math.abs(delta) > 180) {
        // Interpolate the crossing and draw both halves to the edges.
        const seam = delta > 0 ? -180 : 180;
        const unwrapped = lon + (delta > 0 ? -360 : 360);
        const fraction = (seam - oldLon) / (unwrapped - oldLon || 1);
        const seamLat = oldLat + (lat - oldLat) * fraction;
        path += chartPoint('L', seam + 180, 90 - seamLat) + chartPoint('M', 180 - seam, 90 - seamLat);
      }
      path += chartPoint('L', lon + 180, 90 - lat);
    }
    previous = [lat, lon];
  }
  return path;
}

function globeLine(points: readonly Coordinate[], viewLongitude: number) {
  let path = '';
  let previous: [number, number, number] | undefined;
  for (const [lat, lon] of points) {
    const latitude = lat * rad;
    const longitude = (lon - viewLongitude) * rad;
    const x = Math.cos(latitude) * Math.sin(longitude);
    const y = -Math.sin(latitude);
    const z = Math.cos(latitude) * Math.cos(longitude);
    if (previous && (previous[2] >= 0) !== (z >= 0)) {
      const [oldX, oldY, oldZ] = previous;
      const fraction = oldZ / (oldZ - z);
      const edgeX = oldX + (x - oldX) * fraction;
      const edgeY = oldY + (y - oldY) * fraction;
      const length = Math.hypot(edgeX, edgeY) || 1;
      path += chartPoint(z >= 0 ? 'M' : 'L',
        globeCenter + globeRadius * edgeX / length, globeCenter + globeRadius * edgeY / length);
    }
    if (z >= 0) {
      path += chartPoint(previous ? 'L' : 'M', globeCenter + globeRadius * x, globeCenter + globeRadius * y);
    }
    previous = [x, y, z];
  }
  return path;
}

export function greatCircle(segment: PassportSegment): Coordinate[] {
  const { originLatitude, originLongitude, destinationLatitude, destinationLongitude } = segment;
  const vector = (lat: number, lon: number) => [
    Math.cos(lat * rad) * Math.cos(lon * rad), Math.cos(lat * rad) * Math.sin(lon * rad), Math.sin(lat * rad),
  ] as const;
  const [ax, ay, az] = vector(originLatitude, originLongitude);
  const [bx, by, bz] = vector(destinationLatitude, destinationLongitude);
  const angle = Math.acos(Math.max(-1, Math.min(1, ax * bx + ay * by + az * bz)));
  const divisor = Math.sin(angle);
  if (Math.abs(divisor) < 0.000001) return [];
  return Array.from({ length: 33 }, (_, index) => {
    const portion = index / 32;
    const a = Math.sin((1 - portion) * angle) / divisor;
    const b = Math.sin(portion * angle) / divisor;
    const x = a * ax + b * bx;
    const y = a * ay + b * by;
    const z = a * az + b * bz;
    return [Math.atan2(z, Math.hypot(x, y)) / rad, Math.atan2(y, x) / rad];
  });
}

function isCoordinate(latitude: number, longitude: number) {
  return Number.isFinite(latitude) && Math.abs(latitude) <= 90
    && Number.isFinite(longitude) && Math.abs(longitude) <= 180;
}

function routeArcs(segments: PassportSegment[]) {
  const arcs = new Map<string, Coordinate[]>();
  for (const segment of segments) {
    if (!isCoordinate(segment.originLatitude, segment.originLongitude)
      || !isCoordinate(segment.destinationLatitude, segment.destinationLongitude)) continue;
    const route = [
      [segment.originLatitude, segment.originLongitude].join(','),
      [segment.destinationLatitude, segment.destinationLongitude].join(','),
    ].sort().join('>');
    if (!arcs.has(route)) arcs.set(route, greatCircle(segment));
  }
  return [...arcs.values()].filter((arc) => arc.length > 0);
}

export function PassportMaps({ segments }: { segments: PassportSegment[] }) {
  const [mode, setMode] = useState<Projection>('globe');
  const [globeLongitude, setGlobeLongitude] = useState(wrapLongitude(segments[0]?.originLongitude ?? 0));
  const [flatLongitude, setFlatLongitude] = useState(0);
  const { colors } = usePalette();
  const instance = useId().replace(/[^a-zA-Z0-9_-]/g, '');
  const worldID = `passport-world-${instance}`;
  const viewportID = `passport-viewport-${instance}`;
  const arcs = useMemo(() => routeArcs(segments), [segments]);
  const flatRoutes = useMemo(() => arcs.map(flatLine).join(' '), [arcs]);
  const globe = useMemo(() => ({
    grid: globeGrid.map((line) => globeLine(line, globeLongitude)).join(' '),
    land: landRings.map((ring) => globeLine(ring, globeLongitude)).join(' '),
    routes: arcs.map((arc) => globeLine(arc, globeLongitude)).join(' '),
  }), [arcs, globeLongitude]);

  if (!arcs.length) return <Body>Route maps appear when airport coordinates are available.</Body>;
  const pan = (direction: number) => {
    const set = mode === 'globe' ? setGlobeLongitude : setFlatLongitude;
    set((longitude) => wrapLongitude(longitude + direction * 30));
  };

  return (
    <View style={styles.plot}>
      <View style={styles.segmented}>
        {(['globe', 'flat'] as const).map((projection) => {
          const selected = mode === projection;
          return (
            <Pressable key={projection} accessibilityRole="button" accessibilityState={{ selected }}
              onPress={() => setMode(projection)}
              style={({ pressed }) => [styles.segment, {
                backgroundColor: selected || pressed ? colors.tintSoft : colors.surface, borderColor: colors.divider,
              }]}>
              <Text style={[styles.segmentLabel, { color: selected ? colors.tint : colors.text }]}>
                {projection === 'globe' ? 'Globe' : 'Flat map'}
              </Text>
            </Pressable>
          );
        })}
      </View>
      {mode === 'globe' ? (
        <Svg width="100%" height={236} viewBox="0 0 216 216"
          accessibilityRole="image" accessibilityLabel="Globe showing your completed flight routes">
          <Circle cx={globeCenter} cy={globeCenter} r={globeRadius} fill={colors.route} />
          <Path d={globe.grid} stroke="#547A6E" strokeWidth={0.65} fill="none" />
          <Path d={globe.land} stroke={colors.routeMuted} strokeWidth={0.65} strokeOpacity={0.7} fill="none" />
          <Path d={globe.routes} stroke={colors.routeLine} strokeWidth={1.65} strokeLinecap="round" fill="none" />
        </Svg>
      ) : (
        <View style={styles.flatFrame}>
          <Svg width="100%" height="100%" viewBox="0 0 360 180"
            accessibilityRole="image" accessibilityLabel="Flat world map showing your completed flight routes">
            <Defs>
              <ClipPath id={viewportID}><Rect width={mapWidth} height={mapHeight} /></ClipPath>
              <G id={worldID}>
                <Path d={flatLand} fill="#346357" />
                <Path d={flatGrid} stroke="#547A6E" strokeWidth={0.45} fill="none" />
                <Path d={flatRoutes} stroke={colors.routeLine} strokeWidth={1.25} strokeLinecap="round" fill="none" />
              </G>
            </Defs>
            <Rect width={mapWidth} height={mapHeight} fill={colors.route} />
            <G clipPath={`url(#${viewportID})`}>
              <Use href={`#${worldID}`} x={-flatLongitude} />
              {flatLongitude !== 0 && (
                <Use href={`#${worldID}`} x={(flatLongitude < 0 ? -mapWidth : mapWidth) - flatLongitude} />
              )}
            </G>
          </Svg>
        </View>
      )}
      <View style={layout.actions}>
        <Action secondary label={mode === 'globe' ? 'Rotate west' : 'Pan west'} onPress={() => pan(-1)} />
        <Action secondary label={mode === 'globe' ? 'Rotate east' : 'Pan east'} onPress={() => pan(1)} />
      </View>
      <Body>Routes connect departure and arrival airports. Map data from Natural Earth.</Body>
    </View>
  );
}

const styles = StyleSheet.create({
  plot: { gap: 10 },
  segmented: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  segment: { borderRadius: 10, borderWidth: 1, minHeight: 48, paddingHorizontal: 16, justifyContent: 'center' },
  segmentLabel: { fontSize: 15, fontWeight: '600' },
  flatFrame: { width: '100%', aspectRatio: 2, borderRadius: 12, overflow: 'hidden' },
});
