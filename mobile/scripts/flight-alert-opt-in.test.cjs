const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

function compile(file, extra = '') {
  return ts.transpileModule(readFileSync(join(__dirname, '../src', file), 'utf8') + extra, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;
}

const code = {
  api: compile('api.ts'), alerts: compile('alerts.ts'), flight: compile('flight.ts'),
  screens: compile('screens.tsx', '\nexport { AlertsSettings };'),
};
const server = 'https://active.plano.test';
const timeoutMessage = 'Couldn’t enable flight alerts. Check your connection and try again.';
const settle = () => new Promise(setImmediate);

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes);
  return tree && typeof tree === 'object' ? [tree, ...nodes(tree.props.children)] : [];
}

function fixture(platform = 'ios', permission = Promise.resolve({ granted: true })) {
  const token = Promise.withResolvers();
  const storage = new Map([['plano.server.v1', server]]);
  const timers = new Map();
  const delays = [];
  const cleared = [];
  const registrations = [];
  const configured = [];
  const hooks = [];
  let hookIndex = 0;
  let mounted = false;
  let now = 0;
  let nextTimer = 0;
  let tokenReads = 0;
  let revision = 0;
  let androidEnabled = false;
  const jsx = (type, props) => ({ type, props });
  const modules = {
    react: {
      useEffect(effect) { if (!mounted) { mounted = true; effect(); } },
      useState(initial) {
        const slot = hookIndex++;
        if (!(slot in hooks)) hooks[slot] = initial;
        return [hooks[slot], (value) => { hooks[slot] = typeof value === 'function' ? value(hooks[slot]) : value; }];
      },
    },
    'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: 'Fragment' },
    'react-native': { Platform: { OS: platform }, StyleSheet: { create: (styles) => styles }, View: 'View' },
    'expo-secure-store': {
      getItemAsync: async (key) => storage.get(key) ?? null,
      setItemAsync: async (key, value) => { storage.set(key, value); },
      deleteItemAsync: async (key) => { storage.delete(key); },
    },
    'expo-notifications': {
      requestPermissionsAsync: () => permission,
      // Match Expo's shared native request: retries call the API again but can
      // observe the same genuine pending token rather than inventing a token.
      getDevicePushTokenAsync: () => { tokenReads++; return token.promise; },
      AndroidImportance: { HIGH: 4 },
      setNotificationChannelAsync: async () => {},
    },
    './live': {
      canFenceIOSPush: platform === 'ios',
      nextIOSPushRegistration: async () => ({ liveRevision: ++revision, pushClock: '00000000-0000-4000-8000-000000000001' }),
      configureLiveActivities: async (api) => { configured.push(api.server); },
      selectAndroidPushOrigin: async () => null,
      androidPushStatus: async () => ({ generation: 'fixture-generation', enabled: androidEnabled, suppressed: false }),
      enableAndroidPush: async () => { androidEnabled = true; return true; },
    },
    './offline-flights': {},
    './ui': { Action: 'Action', Body: 'Body', ErrorMessage: 'ErrorMessage', Heading: 'Heading', layout: {} },
  };
  function load(source) {
    const exports = {};
    runInNewContext(source, {
      exports, require: (name) => modules[name] ?? {}, Error, AbortController, URL, URLSearchParams,
      __DEV__: false, process: { env: {} },
      setTimeout(callback, delay) {
        const id = ++nextTimer;
        timers.set(id, { callback, at: now + delay });
        delays.push(delay);
        return id;
      },
      clearTimeout(id) { cleared.push(id); timers.delete(id); },
    });
    return exports;
  }
  const fences = modules['./api'] = load(code.api);
  const alerts = modules['./alerts'] = load(code.alerts);
  modules['./flight'] = load(code.flight);
  const { AlertsSettings } = load(code.screens);
  const api = {
    server,
    registerDevice: async (device) => { registrations.push(JSON.parse(JSON.stringify(device))); },
    installationToken: async () => 'fixture-installation',
  };
  const preference = fences.serverStorageKey('flight-alerts', server);
  return {
    token, storage, timers, delays, cleared, registrations, configured, preference, fences,
    tokenReads: () => tokenReads,
    enable: () => alerts.changeFlightAlerts(api, true),
    render() { hookIndex = 0; return nodes(AlertsSettings({ api })); },
    advance(ms) {
      now += ms;
      for (const [id, timer] of timers) {
        if (timer.at <= now) { timers.delete(id); timer.callback(); }
      }
    },
  };
}

const action = (f) => f.render().find(({ type }) => type === 'Action').props;

test('Settings waits for permission, then bounds an unresolved iOS token and clears busy with retry guidance', async () => {
  const permission = Promise.withResolvers();
  const f = fixture('ios', permission.promise);
  f.render();
  await settle();
  const pending = action(f).onPress();
  await settle();
  assert.equal(action(f).busy, true);
  assert.equal(f.timers.size, 0);
  f.advance(60_000);
  assert.equal(f.tokenReads(), 0);
  permission.resolve({ granted: true });
  await settle();
  assert.deepEqual(f.delays, [60_000]);
  f.advance(59_999);
  await settle();
  assert.equal(action(f).busy, true);
  f.advance(1);
  await pending;
  assert.equal(action(f).busy, false);
  assert.equal(action(f).label, 'Enable flight alerts');
  assert.equal(f.render().find(({ type }) => type === 'ErrorMessage').props.message, timeoutMessage);
  assert.equal(f.storage.get(f.preference), undefined);
  assert.deepEqual(f.registrations, []);
  assert.deepEqual(f.configured, []);
  assert.equal(f.timers.size, 0);
});

test('a token arriving after an expired attempt cannot register a device or adopt preference', async () => {
  const f = fixture();
  const rejected = assert.rejects(f.enable(), { message: timeoutMessage });
  await settle();
  f.advance(60_000);
  await rejected;
  f.token.resolve({ data: 'genuine-late-native-token' });
  await settle();
  assert.deepEqual(f.registrations, []);
  assert.deepEqual(f.configured, []);
  assert.equal(f.storage.get(f.preference), undefined);
  assert.equal(f.timers.size, 0);
});

test('a current retry can accept the shared genuine native request without reviving its expired caller', async () => {
  const f = fixture();
  const expired = assert.rejects(f.enable(), { message: timeoutMessage });
  await settle();
  f.advance(60_000);
  await expired;
  const retry = f.enable();
  await settle();
  assert.equal(f.tokenReads(), 2);
  assert.equal(f.timers.size, 1);
  f.token.resolve({ data: 'genuine-shared-native-token' });
  await retry;
  await settle();
  assert.deepEqual(f.registrations, [{
    platform: 'ios', token: 'genuine-shared-native-token', kind: 'alert',
    liveRevision: 1, pushClock: '00000000-0000-4000-8000-000000000001',
  }]);
  assert.equal(f.storage.get(f.preference), 'on');
  assert.deepEqual(f.configured, [server]);
  assert.equal(f.timers.size, 0);
  assert.equal(f.cleared.length, 2);
});

test('late native rejection after timeout is consumed without registration or an unhandled rejection', async (t) => {
  const unhandled = [];
  const listen = (reason) => unhandled.push(reason);
  process.on('unhandledRejection', listen);
  t.after(() => process.removeListener('unhandledRejection', listen));
  const f = fixture();
  const expired = assert.rejects(f.enable(), { message: timeoutMessage });
  await settle();
  f.advance(60_000);
  await expired;
  f.token.reject(new Error('Native registration failed after the deadline.'));
  await settle();
  await settle();
  assert.deepEqual(unhandled, []);
  assert.deepEqual(f.registrations, []);
  assert.equal(f.storage.get(f.preference), undefined);
  assert.equal(f.timers.size, 0);
});

test('fast iOS success and native failure both clear their deadline while retaining the real result', async () => {
  const success = fixture();
  const enabled = success.enable();
  await settle();
  success.token.resolve({ data: 'genuine-native-token' });
  await enabled;
  assert.equal(success.registrations.length, 1);
  assert.equal(success.storage.get(success.preference), 'on');
  assert.equal(success.timers.size, 0);
  assert.equal(success.cleared.length, 1);
  const failure = fixture();
  const nativeError = new Error('Native registration failed.');
  const rejected = assert.rejects(failure.enable(), (cause) => cause === nativeError);
  await settle();
  failure.token.reject(nativeError);
  await rejected;
  assert.equal(failure.timers.size, 0);
  assert.equal(failure.cleared.length, 1);
  assert.deepEqual(failure.registrations, []);
  assert.equal(failure.storage.get(failure.preference), undefined);
});

test('denied permission never starts a token request or token deadline', async () => {
  const f = fixture('ios', Promise.resolve({ granted: false }));
  await assert.rejects(f.enable(), { message: 'Allow notifications for Plano in your device settings.' });
  assert.equal(f.tokenReads(), 0);
  assert.deepEqual(f.delays, []);
  assert.deepEqual(f.registrations, []);
});

test('origin and intent fences still reject a genuine token from a retired enable attempt', async () => {
  for (const retire of [
    (f) => f.storage.set('plano.server.v1', 'https://other.plano.test'),
    (f) => f.fences.beginIOSPushIntent(),
  ]) {
    const f = fixture();
    const rejected = assert.rejects(f.enable(), { message: 'The active server or alert settings changed. Try enabling alerts again.' });
    await settle();
    retire(f);
    f.token.resolve({ data: 'genuine-retired-native-token' });
    await rejected;
    assert.deepEqual(f.registrations, []);
    assert.equal(f.storage.get(f.preference), undefined);
    assert.equal(f.timers.size, 0);
  }
});

test('Android keeps its existing native token and binding path without an iOS deadline', async () => {
  const f = fixture('android');
  const enabled = f.enable();
  await settle();
  assert.deepEqual(f.delays, []);
  f.advance(120_000);
  f.token.resolve({ data: 'genuine-android-native-token' });
  await enabled;
  assert.deepEqual(f.registrations, [{ platform: 'android', token: 'genuine-android-native-token', kind: 'alert', binding: 'fixture-generation' }]);
  assert.equal(f.storage.get(f.preference), 'on');
  assert.deepEqual(f.configured, []);
});
