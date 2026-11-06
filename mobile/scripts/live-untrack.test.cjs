const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const { outputText } = ts.transpileModule(readFileSync(join(__dirname, '../src/api.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
});
const A = 'https://a.plano.test';
const B = 'https://b.plano.test';
const flightId = 'UA123.2027-05-18';
const trackedPath = `/v1/tracked/${flightId}`;
const token = (origin) => origin === A ? 'fixture-installation-a' : 'fixture-installation-b';

function fixture(platform) {
  let revision = 0;
  let session;
  const storage = new Map();
  const events = [];
  const stopped = [];
  const storageKey = (name, options) => options?.keychainService ? `${options.keychainService}:${name}` : name;
  const modules = {
    'react-native': { Platform: { OS: platform } },
    'expo-secure-store': {
      getItemAsync: async (name, options) => storage.get(storageKey(name, options)) ?? null,
      setItemAsync: async (name, value, options) => { storage.set(storageKey(name, options), value); },
      deleteItemAsync: async (name, options) => { storage.delete(storageKey(name, options)); },
    },
    './live': {
      canFenceIOSPush: platform === 'ios',
      selectAndroidPushOrigin: async (origin) => {
        if (!session || session.origin === origin) return null;
        const old = session;
        session = undefined;
        return old;
      },
      disableAndroidPush: async (origin) => {
        if (session?.origin !== origin) return null;
        const old = session;
        session = undefined;
        return old;
      },
      retireIOSPush: async (origin, installationToken) => {
        assert.equal(installationToken, token(origin));
        if (session?.origin === origin) session = undefined;
        return ++revision;
      },
      clearLiveActivities: async () => { session = undefined; },
      captureLiveProgressSession: async (origin) => {
        const binding = session?.origin === origin ? session.binding : null;
        events.push(['capture', origin, binding]);
        return binding;
      },
      stopLiveActivityInSession: async (origin, binding, id) => {
        events.push(['stop', origin, binding, id]);
        if (session?.origin === origin && session.binding === binding) stopped.push([origin, binding, id]);
      },
    },
    './offline-flights': {
      offlineFlightScope: async (origin) => origin,
      clearSavedFlights: () => {},
      forgetSavedFlight: (origin, id) => events.push(['forget', origin, id]),
    },
  };
  const { promise: requested, resolve: requestEntered } = Promise.withResolvers();
  const { promise: response, resolve: respond } = Promise.withResolvers();
  const exports = {};
  runInNewContext(outputText, {
    exports, __DEV__: false, process: { env: {} },
    AbortController, URL, URLSearchParams, setTimeout, clearTimeout,
    require: (name) => {
      assert.ok(Object.hasOwn(modules, name), `Unexpected import ${name}`);
      return modules[name];
    },
    fetch: async (target, init) => {
      const { origin, pathname } = new URL(target);
      assert.ok([A, B].includes(origin));
      if (pathname === '/healthz') return Response.json({ status: 'ok', mode: 'demo' });
      if (pathname === '/v1/installations') return Response.json({ token: token(origin) });
      assert.equal(init.method, 'DELETE');
      assert.equal(init.headers.Authorization, `Bearer ${token(origin)}`);
      if (pathname === '/v1/devices') return new Response(null, { status: 204 });
      assert.equal(pathname, trackedPath);
      events.push(['delete', origin]);
      requestEntered(origin);
      const status = await response;
      return status === 204 ? new Response(null, { status }) : Response.json({ error: 'Fixture untrack failed' }, { status });
    },
  });
  return {
    api: exports, events, stopped, requested, respond,
    enable(origin) {
      const binding = `00000000-0000-4000-8000-${String(++revision).padStart(12, '0')}`;
      session = { origin, binding };
      return binding;
    },
  };
}

async function begin(t, platform) {
  const scenario = fixture(platform);
  await scenario.api.saveServer(A, '', '');
  const binding = scenario.enable(A);
  const finished = scenario.api.createAPI(A).untrack(flightId);
  void finished.catch(() => {});
  t.after(async () => {
    scenario.respond(204);
    await finished.catch(() => {});
  });
  assert.equal(await scenario.requested, A);
  assert.deepEqual(scenario.events, [['capture', A, binding], ['delete', A]]);
  return { ...scenario, binding, finished };
}

for (const platform of ['android', 'ios']) {
  test(`${platform} untrack stops progress for its original session after success`, { timeout: 2000 }, async (t) => {
    const { binding, events, finished, respond, stopped } = await begin(t, platform);
    respond(204);
    await finished;
    assert.deepEqual(events.slice(2), [['forget', A, flightId], ['stop', A, binding, flightId]]);
    assert.deepEqual(stopped, [[A, binding, flightId]]);
  });

  test(`${platform} failed untrack leaves progress alone`, { timeout: 2000 }, async (t) => {
    const { events, finished, respond, stopped } = await begin(t, platform);
    respond(503);
    await assert.rejects(finished, (error) => error.status === 503);
    assert.deepEqual(events.slice(2), []);
    assert.deepEqual(stopped, []);
  });

  for (const transition of ['server switch', 'server round trip', 'alerts re-enable']) {
    test(`${platform} late untrack cannot stop a new session after ${transition}`, { timeout: 2000 }, async (t) => {
      const { api, binding, enable, events, finished, respond, stopped } = await begin(t, platform);
      let active = A;
      if (transition === 'alerts re-enable') {
        await (platform === 'android' ? api.disableAndroidPushForOrigin(A) : api.disableIOSPushForOrigin(A));
      } else {
        await api.saveServer(B, '', A);
        active = B;
        if (transition === 'server round trip') {
          enable(B);
          await api.saveServer(A, '', B);
          active = A;
        }
      }
      const newBinding = enable(active);
      assert.notEqual(newBinding, binding);
      respond(204);
      await finished;
      const attempts = events.filter(([name]) => name === 'stop');
      if (active === B) assert.deepEqual(attempts, []);
      else assert.ok(attempts.every((entry) => entry[1] === A && entry[2] === binding && entry[3] === flightId));
      assert.deepEqual(stopped, []);
    });
  }
}
