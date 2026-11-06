const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const source = readFileSync(join(__dirname, '../src/screens.tsx'), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
});
const start = Date.parse('2026-11-06T19:00:00Z');
const iso = (elapsed = 0) => new Date(start + elapsed).toISOString();
const flight = { id: 'UA123', number: 'UA123', origin: 'SFO', destination: 'EWR', status: 'scheduled', observedAt: iso() };
const settle = () => new Promise(setImmediate);

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes);
  return tree && typeof tree === 'object' ? [tree, ...nodes(tree.props.children)] : [];
}

function flightFixture(flightScreen) {
  const hooks = [];
  const effects = [];
  const timers = new Map();
  let index = 0;
  let now = start;
  let nextTimer = 0;
  let dirty = true;
  let tree;
  const jsx = (type, props) => ({ type, props });
  const sameDeps = (prior, next) => prior?.length === next.length && next.every((value, i) => Object.is(value, prior[i]));
  function useMemo(compute, deps) {
    const slot = index++;
    if (!sameDeps(hooks[slot]?.deps, deps)) hooks[slot] = { deps, value: compute() };
    return hooks[slot].value;
  }
  function useEffect(callback, deps) {
    const slot = index++;
    const previous = hooks[slot];
    if (sameDeps(previous?.deps, deps)) return;
    effects.push(() => {
      previous?.cleanup?.();
      hooks[slot] = { deps, cleanup: callback() };
    });
  }
  const modules = {
    react: {
      useMemo, useEffect, useCallback: (callback, deps) => useMemo(() => callback, deps),
      useRef: (current) => hooks[index++] ??= { current },
      useState(initial) {
        const slot = index++;
        if (!(slot in hooks)) hooks[slot] = typeof initial === 'function' ? initial() : initial;
        return [hooks[slot], (update) => {
          const previous = hooks[slot];
          const value = typeof update === 'function' ? update(previous) : update;
          if (!Object.is(value, previous)) {
            hooks[slot] = value;
            dirty = true;
          }
        }];
      },
    },
    'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: 'Fragment' },
    '@react-navigation/native': { useFocusEffect: (callback) => useEffect(callback, [callback]) },
    'react-native': { Text: 'Text', View: 'View', StyleSheet: { create: (styles) => styles } },
    './flight': { formatDay: () => 'Nov 6', formatTime: () => '19:00', isFinished: () => false, errorText: (cause) => cause.message },
    './theme': { usePalette: () => ({ colors: {} }) },
    './pickup-links': { PickupLinks: 'PickupLinks' },
    './ui': { Action: 'Action', Body: 'Body', ErrorMessage: 'ErrorMessage', FlightStrip: 'FlightStrip', Heading: 'Heading', Input: 'Input', Loading: 'Loading', layout: {} },
  };
  const exports = {};
  runInNewContext(outputText, {
    exports,
    require: (id) => modules[id] ?? {},
    Date: class extends Date { static now() { return now; } },
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      timers.set(id, { callback, due: now + delay });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
  });
  const props = { api: { flightScreen }, route: { params: { id: flight.id, number: flight.number } } };
  function render() {
    if (dirty) {
      index = 0;
      dirty = false;
      tree = exports.FlightDetailScreen(props);
      for (const effect of effects.splice(0)) effect();
    }
    return nodes(tree);
  }
  return {
    render,
    async advance(ms) {
      now += ms;
      for (const [id, { callback, due }] of timers) {
        if (due <= now) {
          timers.delete(id);
          callback();
        }
      }
      await settle();
      return render();
    },
  };
}

const contains = (tree, type, text) => tree.some((node) => node.type === type && node.props.children === text);
const hasNotices = (tree) => tree.some(({ type }) => type.name === 'AirportNotices');
const weather = (conditions, expiry) => ({ airport: 'KSFO', operation: 'departure', conditions, observedAt: iso(), validUntil: iso(expiry) });

test('Flight Detail suppresses stale delay evidence after a failed poll and restores fresh evidence', async () => {
  let reads = 0;
  const fixture = flightFixture(async () => {
    const read = ++reads;
    if (read === 2) throw new Error('Flight detail unavailable');
    return { tracked: read < 4, detail: {
      flight, prediction: { reasons: [read === 1 ? 'Initial delay estimate.' : 'Fresh delay estimate.'] },
      weather: [weather('Low ceilings', 180_000)],
      notices: [{ id: 'notice', airport: 'KSFO', summary: 'Runway notice', validUntil: iso(180_000) }],
    } };
  });
  fixture.render();
  await settle();
  const ready = fixture.render();
  assert.ok(contains(ready, 'Body', 'Initial delay estimate.'));
  assert.ok(contains(ready, 'Body', 'Low ceilings'));
  assert.ok(hasNotices(ready));

  const failed = await fixture.advance(30_000);
  assert.ok(failed.some(({ type, props }) => type === 'ErrorMessage' && props.message === 'Flight detail unavailable'));
  assert.ok(failed.some(({ type, props }) => type === 'FlightStrip' && props.flight === flight));
  assert.ok(!contains(failed, 'Heading', 'Delay outlook'));
  assert.ok(!hasNotices(failed));

  const recovered = await fixture.advance(30_000);
  assert.ok(contains(recovered, 'Body', 'Fresh delay estimate.'));
  assert.ok(contains(recovered, 'Body', 'Low ceilings'));
  assert.ok(hasNotices(recovered));

  const untracked = await fixture.advance(30_000);
  assert.ok(contains(untracked, 'Body', 'Fresh delay estimate.'));
  assert.ok(!contains(untracked, 'Body', 'Low ceilings'));
  assert.ok(!hasNotices(untracked));
});

test('an expiring METAR disappears before the next Flight Detail poll', async () => {
  let reads = 0;
  const fixture = flightFixture(async () => {
    reads++;
    return { tracked: true, detail: { flight, weather: [weather('Low ceilings', 5_000)] } };
  });
  fixture.render();
  await settle();
  assert.ok(contains(fixture.render(), 'Body', 'Low ceilings'));
  assert.ok(contains(await fixture.advance(4_999), 'Body', 'Low ceilings'));
  assert.ok(!contains(await fixture.advance(1), 'Heading', 'Delay outlook'));
  assert.equal(reads, 1);
});
