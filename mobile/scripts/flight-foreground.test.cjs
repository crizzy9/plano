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
const { outputText: refreshModule } = ts.transpileModule(
  readFileSync(join(__dirname, '../src/flight-refresh.ts'), 'utf8'),
  { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } },
);
const settle = () => new Promise(setImmediate);

function remoteFixture(initialState = 'active', refreshScope) {
  const hooks = [];
  const listeners = new Set();
  const timers = new Map();
  const requests = [];
  let hookIndex = 0;
  let nextTimer = 0;
  let blur;
  const appState = {
    currentState: initialState,
    addEventListener(event, listener) {
      assert.equal(event, 'change');
      listeners.add(listener);
      return { remove: () => listeners.delete(listener) };
    },
  };
  const modules = {
    react: {
      useCallback: (callback) => callback,
      useRef: (current) => hooks[hookIndex++] ??= { current },
      useState(initial) {
        const slot = hookIndex++;
        if (!(slot in hooks)) hooks[slot] = initial;
        return [hooks[slot], (update) => {
          hooks[slot] = typeof update === 'function' ? update(hooks[slot]) : update;
        }];
      },
    },
    '@react-navigation/native': { useFocusEffect: (effect) => { blur ??= effect(); } },
    'react-native': { AppState: appState, StyleSheet: { create: (styles) => styles } },
    './flight': { errorText: (error) => error.message },
  };
  const exports = {};
  const refreshEvents = {};
  runInNewContext(refreshModule, { exports: refreshEvents });
  modules['./flight-refresh'] = refreshEvents;
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
  function fetch(signal) {
    return new Promise((resolve, reject) => requests.push({ signal, resolve, reject }));
  }
  return {
    requests, timers, listeners,
    render() {
      hookIndex = 0;
      return exports.useRemote(fetch, true, 30_000, refreshScope);
    },
    appState(state) {
      appState.currentState = state;
      for (const listener of listeners) listener(state);
    },
    blur() { blur(); },
    hint: refreshEvents.publishFlightRefresh,
    tick(delay) {
      assert.equal(timers.size, 1);
      const [[id, timer]] = timers;
      assert.equal(timer.delay, delay);
      timers.delete(id);
      timer.callback();
    },
  };
}

test('foreground reload replaces an interrupted request and ignores its late response', async () => {
  const fixture = remoteFixture();
  fixture.render();
  const [interrupted] = fixture.requests;
  assert.equal(fixture.render().pending, true);

  fixture.appState('background');
  assert.equal(interrupted.signal.aborted, true);
  assert.equal(fixture.render().pending, false);
  assert.equal(fixture.timers.size, 0);

  fixture.appState('active');
  fixture.appState('active');
  assert.equal(fixture.requests.length, 2);
  const resumed = fixture.requests[1];
  resumed.resolve('updated gate');
  await settle();
  assert.equal(fixture.render().value, 'updated gate');
  assert.deepEqual([...fixture.timers.values()].map(({ delay }) => delay), [30_000]);

  interrupted.resolve('old gate');
  await settle();
  assert.equal(fixture.render().value, 'updated gate');
  assert.equal(fixture.timers.size, 1);

  fixture.appState('inactive');
  assert.equal(fixture.timers.size, 0);
  fixture.appState('active');
  assert.equal(fixture.requests.length, 3);
  fixture.blur();
  assert.equal(fixture.requests[2].signal.aborted, true);
  assert.equal(fixture.listeners.size, 0);
  fixture.appState('active');
  assert.equal(fixture.requests.length, 3);
});

test('a focused screen waits until the app becomes active before its first poll', async () => {
  const fixture = remoteFixture('inactive');
  fixture.render();
  assert.equal(fixture.requests.length, 0);
  fixture.appState('background');
  assert.equal(fixture.requests.length, 0);
  fixture.appState('active');
  assert.equal(fixture.requests.length, 1);
  fixture.requests[0].reject(new Error('Couldn’t reach Plano.'));
  await settle();
  assert.equal(fixture.render().error, 'Couldn’t reach Plano.');
  assert.equal(fixture.render().pending, false);
  assert.equal(fixture.timers.size, 1);
  fixture.blur();
  assert.equal(fixture.timers.size, 0);
});

test('only matching alert hints refresh a focused screen and bursts leave one trailing read', async () => {
  const origin = 'https://plano.example.test';
  const fixture = remoteFixture('active', { origin, flightId: 'outbound', relatedFlightId: 'inbound' });
  fixture.render();
  fixture.requests[0].resolve('initial');
  await settle();
  fixture.hint('https://other.example.test', { flightId: 'outbound' });
  fixture.hint(origin, { flightId: 'different' });
  fixture.hint(origin, { flightId: 'outbound', connectionInboundId: 'outbound' });
  fixture.hint(origin, { flightId: 'invalid\n', connectionInboundId: 'inbound' });
  assert.deepEqual([...fixture.timers.values()].map(({ delay }) => delay), [30_000]);

  fixture.hint(origin, { flightId: 'inbound' });
  fixture.hint(origin, { flightId: 'outbound', connectionInboundId: 'inbound' });
  fixture.tick(250);
  assert.equal(fixture.requests.length, 2);

  fixture.hint(origin, { flightId: 'outbound' });
  fixture.hint(origin, { flightId: 'inbound' });
  assert.equal(fixture.requests[1].signal.aborted, false);
  assert.equal(fixture.timers.size, 0);
  fixture.requests[1].resolve('first alert update');
  await settle();
  fixture.tick(250);
  assert.equal(fixture.requests.length, 3);
  fixture.requests[2].resolve('latest gate');
  await settle();
  assert.equal(fixture.render().value, 'latest gate');
  assert.deepEqual([...fixture.timers.values()].map(({ delay }) => delay), [30_000]);

  fixture.appState('background');
  fixture.hint(origin, { flightId: 'outbound' });
  assert.equal(fixture.timers.size, 0);
  fixture.appState('active');
  assert.equal(fixture.requests.length, 4);
  fixture.blur();
  fixture.hint(origin, { flightId: 'outbound' });
  assert.equal(fixture.requests.length, 4);
  assert.equal(fixture.timers.size, 0);
});
