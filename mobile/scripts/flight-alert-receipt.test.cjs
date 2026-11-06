const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

function compile(file) {
  return ts.transpileModule(readFileSync(join(__dirname, '../src', file), 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText;
}

const alerts = compile('alerts.ts');
const live = compile('live.ts');
const server = 'https://active.plano.test';
const other = 'https://other.plano.test';
const settle = () => new Promise(setImmediate);

function fixture(platform) {
  const hints = [];
  const removed = [];
  const listeners = new Map();
  const state = { server, consent: 'on', installed: true };
  const appState = { currentState: 'active', addEventListener: (_, listener) => subscribe('state', listener) };
  let cleanup;
  let handler;
  function subscribe(name, listener) {
    listeners.set(name, listener);
    return { remove() { listeners.delete(name); removed.push(name); } };
  }
  const modules = {
    react: { useEffect(effect) { cleanup = effect(); } },
    'react-native': { AppState: appState, Platform: { OS: platform } },
    'expo-secure-store': { async getItemAsync() { return state.consent; } },
    expo: { requireOptionalNativeModule: () => ({ addListener: subscribe }) },
    'expo-notifications': {
      addPushTokenListener: (listener) => subscribe('token', listener),
      addNotificationReceivedListener: (listener) => subscribe('ios', listener),
      setNotificationHandler(value) { handler = value; },
      async getPermissionsAsync() { return { granted: false }; },
    },
    './api': {
      async loadServer() { return state.server; },
      normalizeServer: (origin) => new URL(origin).origin,
      serverStorageKey: (kind, origin) => `${kind}:${origin}`,
      async initializeAndroidPushOrigin(origin) { assert.equal(origin, server); },
    },
    './flight': {},
    './flight-refresh': { publishFlightRefresh: (...hint) => hints.push(hint) },
  };
  function load(code) {
    const exports = {};
    runInNewContext(code, { exports, require: (name) => modules[name] });
    return exports;
  }
  modules['./live'] = load(live);
  load(alerts).useFlightAlerts({ server, async hasSavedInstallationToken() { return state.installed; } });
  return {
    appState, hints, listeners, removed, state,
    stop() { cleanup(); assert.equal(handler, null); },
  };
}

test('iOS receives only foreground push hints for a current opted-in installation', async () => {
  const f = fixture('ios');
  await settle();
  const listener = f.listeners.get('ios');
  const payload = { flightId: 'AA123', refreshOrigin: 'HTTPS://ACTIVE.PLANO.TEST:443/' };
  const fromAPNs = (data) => ({ request: { trigger: { type: 'push', payload: data } } });
  const push = fromAPNs(payload);
  listener(null);
  listener({ request: { trigger: 'push' } });
  listener({ request: { trigger: { type: 'calendar', payload } } });
  listener(fromAPNs({ flightId: 'AA123' }));
  listener(fromAPNs({ ...payload, refreshOrigin: other }));
  listener(fromAPNs({ ...payload, refreshOrigin: 'x'.repeat(321) }));
  listener(fromAPNs({ ...payload, refreshOrigin: 'invalid origin' }));
  f.appState.currentState = 'background';
  listener(push);
  f.appState.currentState = 'active';
  f.state.consent = 'retiring';
  listener(push);
  await settle();
  f.state.consent = 'on';
  f.state.installed = false;
  listener(push);
  await settle();
  f.state.installed = true;
  f.state.server = other;
  listener(push);
  await settle();
  assert.deepEqual(f.hints, []);

  f.state.server = server;
  listener(push);
  await settle();
  assert.deepEqual(f.hints, [[server, payload]]);
  listener(push);
  f.stop();
  await settle();
  assert.deepEqual(f.hints, [[server, payload]]);
  assert.deepEqual(f.removed.sort(), ['ios', 'state', 'token']);
});

test('Android receives hints only from the selected native alert origin', async () => {
  const f = fixture('android');
  await settle();
  const listener = f.listeners.get('onFlightAlert');
  const payload = { origin: server, flightId: 'AA123', connectionInboundId: 'AA234' };
  listener(null);
  listener({ ...payload, origin: other });
  f.appState.currentState = 'background';
  listener(payload);
  f.appState.currentState = 'active';
  f.state.server = other;
  listener(payload);
  await settle();
  assert.deepEqual(f.hints, []);

  f.state.server = server;
  listener(payload);
  await settle();
  assert.deepEqual(f.hints, [[server, payload]]);
  f.stop();
  listener(payload);
  await settle();
  assert.deepEqual(f.hints, [[server, payload]]);
  assert.deepEqual(f.removed.sort(), ['onFlightAlert', 'state', 'token']);
});
