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
const probePath = '/v1/tracked?flightId=plano-installation-check';
const token = (origin) => origin === A ? 'fixture-installation-a' : 'fixture-installation-b';
const replacementToken = 'fixture-replacement-installation';
const enrollmentCode = 'fixture-enrollment-code';
const backgroundStore = { keychainService: 'plano.background.v1' };

function fixture(platform) {
  let revision = 0;
  let session;
  let probeStatus;
  let enrollmentGate;
  let retirementGate;
  const retirementStarted = Promise.withResolvers();
  const storage = new Map();
  const accepted = new Map();
  const replacements = new Set();
  const events = [];
  const stopped = [];
  const enrollment = [];
  const probes = [];
  const retirements = [];
  const deletes = [];
  const calendarScopes = ['calendar-sync', 'calendar-sync-imports', 'calendar-imports'];
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
      disableAndroidPush: async (origin, expected) => {
        if (session?.origin !== origin || expected && session.binding !== expected) return null;
        retirementStarted.resolve();
        if (retirementGate) await retirementGate;
        const old = session;
        session = undefined;
        retirements.push([origin, old.binding]);
        return old;
      },
      retireIOSPush: async (origin, installationToken) => {
        assert.equal(installationToken, storage.get(exports.serverStorageKey('installation', origin)));
        retirementStarted.resolve();
        if (retirementGate) await retirementGate;
        retirements.push([origin, installationToken]);
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
      const { origin, pathname, search } = new URL(target);
      assert.ok([A, B].includes(origin));
      if (pathname === '/healthz') return Response.json({ status: 'ok', mode: 'demo' });
      if (pathname === '/v1/installations') {
        assert.equal(init.method, 'POST');
        const code = JSON.parse(init.body).enrollmentCode;
        enrollment.push([origin, code]);
        if (replacements.has(origin) && code !== enrollmentCode) {
          return Response.json({ error: 'Fixture enrollment rejected' }, { status: 403 });
        }
        if (enrollmentGate) await enrollmentGate;
        const bearer = replacements.has(origin) ? replacementToken : token(origin);
        accepted.set(origin, `Bearer ${bearer}`);
        return Response.json({ token: bearer });
      }
      const bearer = init.headers.Authorization;
      const authorized = accepted.has(origin) && bearer === accepted.get(origin);
      if (pathname + search === probePath) {
        assert.equal(init.method ?? 'GET', 'GET');
        probes.push([origin, bearer]);
        if (probeStatus) return Response.json({ error: 'Fixture probe failed' }, { status: probeStatus });
        return authorized ? Response.json({ flights: [] }) : new Response(null, { status: 401 });
      }
      if (pathname === '/v1/friends') {
        assert.equal(init.method ?? 'GET', 'GET');
        return authorized ? Response.json({ friends: [] }) : new Response(null, { status: 401 });
      }
      assert.equal(init.method, 'DELETE');
      if (pathname === '/v1/devices') return new Response(null, { status: authorized ? 204 : 401 });
      assert.equal(pathname, trackedPath);
      deletes.push(bearer);
      if (!authorized) return new Response(null, { status: 401 });
      events.push(['delete', origin]);
      requestEntered(origin);
      const status = await response;
      return status === 204 ? new Response(null, { status }) : Response.json({ error: 'Fixture untrack failed' }, { status });
    },
  });
  return {
    api: exports, events, stopped, requested, respond, enrollment, probes, retirements, deletes,
    storedInstallation: (origin) => storage.get(exports.serverStorageKey('installation', origin)),
    async allowBackgroundAndAlerts(origin) {
      await exports.prepareCalendarBackground(origin, token(origin));
      storage.set(exports.serverStorageKey('flight-alerts', origin), 'on');
      for (const scope of calendarScopes) storage.set(exports.serverStorageKey(scope, origin), 'fixture');
    },
    localConsent(origin) {
      return {
        alertPreference: storage.get(exports.serverStorageKey('flight-alerts', origin)),
        backgroundServer: storage.get(storageKey('plano.server.v1', backgroundStore)),
        backgroundToken: storage.get(storageKey(exports.serverStorageKey('installation', origin), backgroundStore)),
        binding: session?.origin === origin ? session.binding : undefined,
        calendarState: calendarScopes.filter((scope) => storage.has(exports.serverStorageKey(scope, origin))),
      };
    },
    replaceDatabase(origin, pendingEnrollment) {
      replacements.add(origin);
      accepted.delete(origin);
      enrollmentGate = pendingEnrollment;
    },
    failProbe(status) { probeStatus = status; },
    holdRetirement(gate) {
      retirementGate = gate;
      return retirementStarted.promise;
    },
    enable(origin) {
      const binding = `00000000-0000-4000-8000-${String(++revision).padStart(12, '0')}`;
      session = { origin, binding };
      return binding;
    },
  };
}

for (const platform of ['android', 'ios']) {
  test(`${platform} same-origin reconnect replaces a revoked installation and refreshes an open client`, async () => {
    const { api, enrollment, probes, retirements, replaceDatabase, storedInstallation, allowBackgroundAndAlerts,
      localConsent, enable } = fixture(platform);
    await api.saveServer(A, '', '');
    const binding = enable(A);
    await allowBackgroundAndAlerts(A);
    const originalConsent = { alertPreference: 'on', backgroundServer: A, backgroundToken: token(A), binding,
      calendarState: ['calendar-sync', 'calendar-sync-imports', 'calendar-imports'] };
    assert.deepEqual(localConsent(A), originalConsent);
    const existingClient = api.createAPI(A);
    await existingClient.friends();

    const reused = await api.saveServer(A, enrollmentCode, A);
    assert.equal(reused.url, A);
    assert.equal(reused.replacedInstallation, false);
    assert.deepEqual(probes, [[A, `Bearer ${token(A)}`]]);
    assert.deepEqual(enrollment, [[A, undefined]]);
    assert.equal(storedInstallation(A), token(A));
    assert.deepEqual(localConsent(A), originalConsent);
    assert.deepEqual(retirements, []);

    replaceDatabase(A);
    await assert.rejects(existingClient.friends(), { status: 401 });
    const replaced = await api.saveServer(A, enrollmentCode, A);
    assert.equal(replaced.url, A);
    assert.equal(replaced.replacedInstallation, true);
    assert.deepEqual(probes, [[A, `Bearer ${token(A)}`], [A, `Bearer ${token(A)}`]]);
    assert.deepEqual(enrollment, [[A, undefined], [A, enrollmentCode]]);
    assert.equal(storedInstallation(A), replacementToken);
    assert.deepEqual(localConsent(A), {
      alertPreference: undefined, backgroundServer: undefined, backgroundToken: undefined, binding: undefined,
      calendarState: [],
    });
    assert.deepEqual(retirements, [[A, platform === 'ios' ? token(A) : binding]]);
    await existingClient.friends();
  });

  test(`${platform} an unavailable probe or rejected enrollment preserves the saved installation and consent`, async () => {
    const { api, enrollment, probes, retirements, failProbe, replaceDatabase, storedInstallation, allowBackgroundAndAlerts,
      localConsent, enable } = fixture(platform);
    await api.saveServer(A, '', '');
    enable(A);
    await allowBackgroundAndAlerts(A);
    const originalConsent = localConsent(A);
    const existingClient = api.createAPI(A);
    await existingClient.friends();

    failProbe(503);
    await assert.rejects(api.saveServer(A, enrollmentCode, A), { status: 503 });
    assert.deepEqual(probes, [[A, `Bearer ${token(A)}`]]);
    assert.deepEqual(enrollment, [[A, undefined]]);
    assert.equal(storedInstallation(A), token(A));
    assert.deepEqual(localConsent(A), originalConsent);
    await existingClient.friends();

    failProbe(undefined);
    replaceDatabase(A);
    await assert.rejects(api.saveServer(A, 'invalid fixture code', A), { status: 403 });
    assert.deepEqual(enrollment, [[A, undefined], [A, 'invalid fixture code']]);
    assert.equal(storedInstallation(A), token(A));
    assert.deepEqual(localConsent(A), originalConsent);
    assert.deepEqual(retirements, []);
    assert.equal(await api.loadServer(), A);
    await assert.rejects(existingClient.friends(), { status: 401 });
  });

  test(`${platform} an old untrack intent cannot delete from the replacement installation`, { timeout: 2000 }, async (t) => {
    const scenario = fixture(platform);
    await scenario.api.saveServer(A, '', '');
    scenario.enable(A);
    const client = scenario.api.createAPI(A);
    await client.friends();
    scenario.replaceDatabase(A);
    const { promise, resolve } = Promise.withResolvers();
    t.after(() => { resolve(); scenario.respond(204); });
    const retirement = scenario.holdRetirement(promise);
    const reconnect = scenario.api.saveServer(A, enrollmentCode, A);
    void reconnect.catch(() => {});
    await retirement;
    const untrack = client.untrack(flightId);
    const rejected = assert.rejects(untrack, { status: 401 });
    resolve();
    await reconnect;
    scenario.respond(204);
    await rejected;
    assert.deepEqual(scenario.deletes, [`Bearer ${token(A)}`]);
    assert.deepEqual(scenario.stopped, []);
  });
}

test('concurrent reconnects converge on one installation', { timeout: 2000 }, async (t) => {
  const { api, enrollment, replaceDatabase, storedInstallation } = fixture('android');
  await api.saveServer(A, '', '');
  const { promise, resolve } = Promise.withResolvers();
  t.after(() => resolve());
  replaceDatabase(A, promise);

  const reconnects = Promise.all([api.saveServer(A, enrollmentCode, A), api.saveServer(A, enrollmentCode, A)]);
  void reconnects.catch(() => {});
  await new Promise(setImmediate);
  assert.equal(storedInstallation(A), token(A));
  resolve();
  const results = await reconnects;
  assert.deepEqual(results.map(({ url }) => url), [A, A]);
  assert.deepEqual(results.map(({ replacedInstallation }) => replacedInstallation).sort(), [false, true]);
  assert.deepEqual(enrollment, [[A, undefined], [A, enrollmentCode]]);
  assert.equal(storedInstallation(A), replacementToken);
});

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
