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

function completedRadar(radar) {
  const timers = new Map();
  const hooks = [];
  let hookIndex = 0;
  let cleanup;
  let nextTimer = 0;
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
      AppState: { currentState: 'active', addEventListener: () => ({ remove() {} }) },
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
    render() {
      hookIndex = 0;
      return exports.FlightRadar({ api: { radar }, flight, tracked: true, detailRevision: '0', refreshFlight() {} });
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
