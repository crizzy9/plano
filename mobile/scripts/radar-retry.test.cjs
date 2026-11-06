const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const source = readFileSync(join(__dirname, '../src/screens.tsx'), 'utf8');
const { outputText } = ts.transpileModule(source + '\nexport { FlightRadar };', {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
});

function completedRadar(radar, history) {
  const timers = new Map();
  const hooks = [];
  let hookIndex = 0;
  let cleanup;
  let nextTimer = 0;
  let onAppStateChange;
  const appState = {
    currentState: 'active',
    addEventListener(event, listener) {
      assert.equal(event, 'change');
      onAppStateChange = listener;
      return { remove() { onAppStateChange = undefined; } };
    },
  };
  const jsx = (type, props) => ({ type, props });
  const modules = {
    react: {
      useCallback: (callback) => callback,
      useMemo: (compute) => compute(),
      useRef: (current) => hooks[hookIndex++] ??= { current },
      useState: (initial) => {
        const index = hookIndex++;
        if (!(index in hooks)) hooks[index] = initial;
        return [hooks[index], (value) => {
          hooks[index] = typeof value === 'function' ? value(hooks[index]) : value;
        }];
      },
    },
    'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: 'Fragment' },
    '@react-navigation/native': { useFocusEffect: (callback) => { cleanup ??= callback(); } },
    'react-native': {
      AppState: appState,
      StyleSheet: { create: (styles) => styles },
      View: 'View',
    },
    './ui': { Heading: 'Heading', layout: {} },
    './radar-visuals': { RadarRouteMap: 'RadarRouteMap' },
  };
  const exports = {};
  runInNewContext(outputText, {
    exports,
    require: (id) => modules[id] ?? {},
    AbortController,
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
  });
  const flight = {
    id: 'UA123', origin: 'SFO', destination: 'EWR', status: 'arrived',
    actualTakeoff: '2025-06-01T15:00:00Z', actualArrival: '2025-06-01T20:00:00Z',
    observedAt: '2025-06-01T20:01:00Z',
  };
  const settle = () => new Promise(setImmediate);
  return {
    timers,
    settle,
    setAppState(state) {
      appState.currentState = state;
      onAppStateChange?.(state);
    },
    render() {
      hookIndex = 0;
      return exports.FlightRadar({ api: { radar }, flight, history, tracked: true, detailRevision: '0', refreshFlight() {} });
    },
    async retry() {
      assert.equal(timers.size, 1);
      const [[id, { callback, delay }]] = timers;
      assert.equal(delay, 24_000);
      timers.delete(id);
      callback();
      await settle();
    },
  };
}

test('completed flight retries radar and shows private telemetry after a transient failure', async () => {
  let calls = 0;
  const position = { at: '2025-06-01T19:55:00Z', source: 'faa_swim', latitude: 40, longitude: -74, altitudeFeet: 6500 };
  const radar = completedRadar(async () => {
    if (++calls === 1) throw new Error('unavailable');
    return { positions: [], receiverRevision: '0', swimRevision: '0', swimPositions: [position] };
  });
  assert.equal(radar.render(), null);
  await radar.settle();
  await radar.retry();
  const [heading, track] = radar.render().props.children;
  assert.equal(heading.props.children, 'Recorded flight path');
  const [map, readout] = track.props.children;
  assert.equal(map.props.positions[0], position);
  assert.equal(readout.props.position, position);
  assert.equal(radar.timers.size, 0);
});

test('radar restarts on foreground and discards a response from before backgrounding', async () => {
  const fresh = { at: '2025-06-01T19:55:00Z', source: 'operator_readsb', latitude: 40, longitude: -74 };
  const start = Date.parse('2025-06-01T19:45:00Z');
  const stalePage = Array.from({ length: 256 }, (_, index) => ({
    ...fresh, at: new Date(start + index * 1_000).toISOString(),
  }));
  const signals = [];
  let resolveStale;
  const radar = completedRadar(async (_id, _after, _provider, _receiver, _swim, _eurocontrol, signal) => {
    signals.push(signal);
    if (signals.length === 1) return new Promise((resolve) => { resolveStale = resolve; });
    return { positions: [fresh], receiverRevision: '0' };
  });
  radar.render();
  assert.ok(signals[0] instanceof AbortSignal && !signals[0].aborted);
  radar.setAppState('background');
  assert.equal(signals[0].aborted, true);
  assert.equal(signals.length, 1);
  radar.setAppState('active');
  assert.equal(signals.length, 2);
  assert.ok(signals[1] instanceof AbortSignal && !signals[1].aborted);
  await radar.settle();

  // An accepted full page would append old positions and trigger pagination.
  resolveStale({ positions: stalePage });
  await radar.settle();
  const [, track] = radar.render().props.children;
  const [map] = track.props.children;
  assert.equal(map.props.positions.length, 1);
  assert.equal(map.props.positions[0], fresh);
  assert.equal(radar.timers.size, 0);
});

test('a dense recorded track spans its first and latest fixes within the display cap', async () => {
  const start = Date.parse('2025-06-01T15:00:00Z');
  const positions = Array.from({ length: 700 }, (_, index) => ({
    at: new Date(start + index * 5_000).toISOString(),
    source: index % 2 ? 'faa_swim' : 'eurocontrol_nm',
    latitude: 40, longitude: -74 - index / 1_000,
  }));
  const radar = completedRadar(async () => ({
    positions: [], receiverRevision: '0',
    swimRevision: '0', swimPositions: positions.filter(({ source }) => source === 'faa_swim'),
    eurocontrolRevision: '0', eurocontrolPositions: positions.filter(({ source }) => source === 'eurocontrol_nm'),
  }));
  radar.render();
  await radar.settle();
  const [, track] = radar.render().props.children;
  const [map, readout] = track.props.children;
  assert.ok(map.props.positions.length > 256 && map.props.positions.length <= 512);
  assert.equal(map.props.positions[0], positions[0]);
  assert.equal(map.props.positions.at(-1), positions.at(-1));
  assert.equal(readout.props.position, positions.at(-1));
  assert.equal(radar.timers.size, 0);
});

test('radar fetches the provider baseline even when detail has the same revision', async () => {
  const early = { at: '2025-06-01T15:00:00Z', source: 'flightaware_aeroapi', latitude: 38, longitude: -121 };
  const late = { at: '2025-06-01T19:55:00Z', source: 'operator_readsb', latitude: 40, longitude: -74 };
  let knownRevision;
  const radar = completedRadar(async (_id, _after, providerRevision) => {
    knownRevision = providerRevision;
    return {
      positions: [], receiverRevision: '0',
      providerRevision: '2025-06-01T16:01:00-04:00', providerPositions: [early],
    };
  }, [late]);
  radar.render();
  await radar.settle();
  assert.equal(knownRevision, undefined);
  const [, track] = radar.render().props.children;
  const [map] = track.props.children;
  assert.equal(map.props.positions.length, 2);
  assert.equal(map.props.positions[0], early);
  assert.equal(map.props.positions[1], late);
});

const unavailable = async () => { throw new Error('unavailable'); };
for (const [failure, respond] of [['request failure', unavailable], ['malformed response', async () => ({})]]) {
  test(`completed flight stops retrying after two attempts to recover from a ${failure}`, async () => {
    let calls = 0;
    const radar = completedRadar(async () => {
      calls += 1;
      return respond();
    });
    radar.render();
    await radar.settle();
    await radar.retry();
    await radar.retry();
    assert.equal(calls, 3);
    assert.equal(radar.timers.size, 0);
  });
}
