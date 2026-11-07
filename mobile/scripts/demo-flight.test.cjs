const assert = require('node:assert/strict');
const { existsSync, mkdtempSync, readFileSync, renameSync, rmSync, statSync, unlinkSync, writeFileSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

function compile(file) {
  return ts.transpileModule(readFileSync(join(__dirname, '../src', file), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;
}

const code = Object.fromEntries(['flight.ts', 'theme.ts', 'ui.tsx', 'offline-flights.ts'].map((file) => [file, compile(file)]));
function load(file, modules, globals = {}) {
  const exports = {};
  runInNewContext(code[file], { exports, require: (id) => {
    assert.ok(Object.hasOwn(modules, id), `Unexpected import: ${id}`);
    return modules[id];
  }, ...globals });
  return exports;
}

const observedAt = new Date().toISOString();
const flight = {
  id: 'demo-2027-05-18-UA123', number: 'UA123', origin: 'SFO', destination: 'EWR',
  originTimeZone: 'America/Los_Angeles', destinationTimeZone: 'America/New_York',
  scheduledDeparture: '2027-05-18T15:00:00Z', scheduledArrival: '2027-05-18T20:00:00Z',
  status: 'scheduled', observedAt,
};
const disclosure = 'Demo flight · Simulated data';
const label = 'UA123, SFO to EWR, Scheduled';

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes);
  return tree && typeof tree === 'object' ? [tree, ...nodes(tree.props.children)] : [];
}

function renderer(scheme) {
  const jsx = (type, props) => typeof type === 'function' ? type(props) : { type, props };
  const modules = {
    react: { useEffect() {}, useState: (initial) => [typeof initial === 'function' ? initial() : initial, () => {}] },
    'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: 'Fragment' },
    'react-native': {
      ActivityIndicator: 'ActivityIndicator', Pressable: 'Pressable', Text: 'Text', TextInput: 'TextInput', View: 'View',
      StyleSheet: { create: (styles) => styles, hairlineWidth: 1 }, useColorScheme: () => scheme,
    },
    'react-native-svg': { default: 'Svg', Path: 'Path' },
    './flight': load('flight.ts', {}),
  };
  modules['./theme'] = load('theme.ts', modules);
  return load('ui.tsx', modules);
}

for (const scheme of ['light', 'dark']) {
  test(`${scheme} shared flight UI discloses demo data visibly and in the row action`, () => {
    const ui = renderer(scheme);
    const demo = { ...flight, source: 'plano_demo' };
    let opened = 0;
    const row = ui.FlightRow({ flight: demo, onPress: () => opened++ });
    assert.equal(row.type, 'Pressable');
    assert.equal(row.props.accessibilityLabel, `${label}, ${disclosure}`);
    row.props.onPress();
    assert.equal(opened, 1);
    for (const tree of [row, ui.FlightRow({ flight: demo }), ui.FlightStrip({ flight: demo })]) {
      const visibleDisclosure = nodes(tree).filter(({ type, props }) => type === 'Text' && props.children === disclosure);
      assert.equal(visibleDisclosure.length, 1);
      assert.notEqual(visibleDisclosure[0].props.accessible, false);
    }
    const stripText = nodes(ui.FlightStrip({ flight: demo })).filter(({ type }) => type === 'Text').map(({ props }) => props.children);
    for (const expected of ['UA123', 'SFO', 'EWR', 'Scheduled']) assert.ok(stripText.includes(expected));
  });
}

test('ordinary and absent flight sources keep the existing row label and have no demo disclosure', () => {
  const ui = renderer('light');
  for (const source of [undefined, 'flightaware', 'manual', 'PLANO_DEMO', 'plano_demo_extra']) {
    const item = { ...flight, source };
    const row = ui.FlightRow({ flight: item, onPress() {} });
    assert.equal(row.props.accessibilityLabel, label);
    for (const tree of [row, ui.FlightStrip({ flight: item })]) {
      assert.ok(!nodes(tree).some(({ type, props }) => type === 'Text' && props.children === disclosure));
    }
  }
});

function cacheFixture(t) {
  const directory = mkdtempSync(join(tmpdir(), 'plano-demo-cache-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  class File {
    constructor(base, name) { this.path = join(base, name); }
    get size() { return statSync(this.path).size; }
    textSync() { return readFileSync(this.path, 'utf8'); }
    create({ overwrite }) {
      if (!overwrite && existsSync(this.path)) throw new Error('File exists');
      writeFileSync(this.path, '');
    }
    write(bytes) { writeFileSync(this.path, bytes); }
    moveSync(destination) { renameSync(this.path, destination.path); }
    delete() { unlinkSync(this.path); }
  }
  const modules = {
    'expo-crypto': {}, 'expo-file-system': { File, Paths: { cache: directory } },
    'react-native': { Platform: { OS: 'android' } }, './flight': load('flight.ts', {}),
  };
  return { load: () => load('offline-flights.ts', modules, { TextEncoder }), directory };
}

test('demo provenance survives public overview/detail cache writes and fresh reads, including inbound flights', (t) => {
  const fixture = cacheFixture(t);
  const demo = { ...flight, source: 'plano_demo' };
  const cache = fixture.load();
  cache.saveOverview('fixture', [demo]);
  cache.saveFlightDetail('fixture', { flight: demo, inbound: [{ ...demo, id: 'demo-inbound', number: 'UA456' }] });
  const persisted = JSON.parse(readFileSync(join(fixture.directory, 'plano-offline-flights-v1.json'), 'utf8'));
  assert.equal(persisted.overview.flights[0].source, 'plano_demo');
  assert.equal(persisted.details[0].detail.flight.source, 'plano_demo');
  const restored = fixture.load();
  const overview = restored.readSavedOverview('fixture').flights[0];
  const detail = restored.readSavedFlight('fixture', demo.id).detail;
  assert.equal(overview.source, 'plano_demo');
  assert.equal(detail.flight.source, 'plano_demo');
  assert.equal(detail.inbound[0].source, 'plano_demo');
  const ui = renderer('light');
  assert.equal(ui.FlightRow({ flight: overview, onPress() {} }).props.accessibilityLabel, `${label}, ${disclosure}`);
  assert.ok(nodes(ui.FlightStrip({ flight: detail.flight })).some(({ type, props }) => type === 'Text' && props.children === disclosure));
});

test('cache source text stays bounded and rejects nonstring input on write and read', (t) => {
  const fixture = cacheFixture(t);
  const sources = ['plano_demo', 'flightaware', 'x'.repeat(64), 'x'.repeat(65), {}, [], 42, true, null, undefined];
  const inputs = sources.map((source, index) => ({ ...flight, id: `source-${index}`, source }));
  const expected = ['plano_demo', 'flightaware', 'x'.repeat(64), ...sources.slice(3).map(() => undefined)];
  const cache = fixture.load();
  cache.saveOverview('fixture', inputs);
  for (const item of inputs) cache.saveFlightDetail('fixture', { flight: item });
  let restored = fixture.load();
  restored.readSavedOverview('fixture').flights.forEach((item, index) => assert.equal(item.source, expected[index]));
  // The detail cap still applies; the newest eight entries include all malformed sources.
  for (let index = 2; index < inputs.length; index++) {
    assert.equal(restored.readSavedFlight('fixture', inputs[index].id).detail.flight.source, expected[index]);
  }
  // Untrusted JSON can also arrive from an old or externally altered cache file.
  writeFileSync(join(fixture.directory, 'plano-offline-flights-v1.json'), JSON.stringify({
    scope: 'fixture', overview: { flights: inputs, savedAt: observedAt },
    details: inputs.slice(-8).map((item) => ({ detail: { flight: item }, savedAt: observedAt })),
  }));
  restored = fixture.load();
  restored.readSavedOverview('fixture').flights.forEach((item, index) => assert.equal(item.source, expected[index]));
  for (let index = 2; index < inputs.length; index++) {
    assert.equal(restored.readSavedFlight('fixture', inputs[index].id).detail.flight.source, expected[index]);
  }
});
