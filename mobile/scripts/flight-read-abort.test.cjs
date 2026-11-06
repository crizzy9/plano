const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const { outputText } = ts.transpileModule(readFileSync(join(__dirname, '../src/api.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
});
const server = 'https://reads.plano.test';
const token = 'fixture-flight-reader';
const flightId = 'UA123.2027-05-18';
const savedOverview = { flights: [{ id: flightId }], savedAt: '2027-05-18T10:00:00Z' };
const settle = () => new Promise(setImmediate);
const aborted = { name: 'AbortError' };

function caller() {
  const controller = new AbortController();
  const { signal } = controller;
  const listeners = new Set();
  const add = signal.addEventListener.bind(signal);
  const remove = signal.removeEventListener.bind(signal);
  signal.addEventListener = (type, listener) => {
    listeners.add(listener);
    add(type, listener);
  };
  signal.removeEventListener = (type, listener) => {
    listeners.delete(listener);
    remove(type, listener);
  };
  return { controller, signal, listeners };
}

function fixture({ readToken = () => token } = {}) {
  const events = [];
  const requests = [];
  const timers = new Map();
  let nextTimer = 0;
  const modules = {
    'react-native': { Platform: { OS: 'android' } },
    'expo-secure-store': {
      async getItemAsync(key) {
        if (key === 'plano.server.v1') return server;
        assert.ok(key.startsWith('plano.installation.'));
        events.push('token');
        return readToken();
      },
    },
    './live': {},
    './offline-flights': {
      offlineFlightScope: async () => {
        events.push('scope');
        return server;
      },
      readSavedOverview: () => {
        events.push('read');
        return savedOverview;
      },
      readSavedFlight: () => events.push('read flight'),
      saveOverview: () => events.push('save'),
      saveFlightDetail: () => events.push('save flight'),
      forgetSavedFlight: () => events.push('forget'),
      clearSavedFlights: () => events.push('clear'),
    },
  };
  const exports = {};
  runInNewContext(outputText, {
    exports, __DEV__: false, process: { env: {} }, AbortController, URL, URLSearchParams,
    require: (id) => modules[id],
    setTimeout(callback, delay) {
      const id = ++nextTimer;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
    fetch: (target, init) => {
      const { origin, pathname } = new URL(target);
      assert.equal(origin, server);
      assert.equal(init.headers.Authorization, `Bearer ${token}`);
      const response = Promise.withResolvers();
      const abort = () => response.reject(new Error('transport aborted'));
      init.signal.addEventListener('abort', abort);
      requests.push({ pathname, signal: init.signal, respond: response.resolve });
      return response.promise.finally(() => init.signal.removeEventListener('abort', abort));
    },
  });
  return { api: exports.createAPI(server), exports, events, requests, timers };
}

test('a pre-aborted read does not load the installation token', async () => {
  const { api, events, requests, timers } = fixture();
  const { controller, signal, listeners } = caller();
  controller.abort();
  for (const read of [
    () => api.flightOverview(signal),
    () => api.flightScreen(flightId, undefined, signal),
    () => api.friends(signal),
    () => api.connection('in', 'out', signal),
    () => api.connectionMonitors(signal),
    () => api.tripItSubscription(signal),
    () => api.radar(flightId, undefined, undefined, undefined, undefined, undefined, signal),
  ]) await assert.rejects(read, aborted);
  assert.deepEqual(events, []);
  assert.deepEqual(requests, []);
  assert.equal(timers.size, 0);
  assert.equal(listeners.size, 0);
});

test('canceling one token waiter leaves the shared lookup available', async () => {
  const pendingToken = Promise.withResolvers();
  const { api, events, requests } = fixture({ readToken: () => pendingToken.promise });
  const { controller, signal, listeners } = caller();
  const pending = api.flightOverview(signal);
  const rejected = assert.rejects(pending, aborted);
  const next = api.friends();
  assert.deepEqual(events, ['token']);
  controller.abort();
  await rejected;
  assert.equal(listeners.size, 0);
  assert.deepEqual(requests, []);
  pendingToken.resolve(token);
  await settle();
  assert.deepEqual(requests.map(({ pathname }) => pathname), ['/v1/friends']);
  requests[0].respond(Response.json({ friends: [] }));
  await next;
});

test('explicit cancellation aborts both flight detail requests without cache fallback', async () => {
  const { api, events, requests, timers } = fixture();
  const { controller, signal, listeners } = caller();
  const pending = api.flightScreen(flightId, undefined, signal);
  const rejected = assert.rejects(pending, aborted);
  await settle();
  assert.deepEqual(requests.map(({ pathname }) => pathname), [`/v1/flights/${flightId}`, '/v1/tracked']);
  assert.equal(timers.size, 2);
  controller.abort();
  await rejected;
  assert.ok(requests.every((request) => request.signal.aborted));
  assert.deepEqual(events, ['token']);
  assert.equal(timers.size, 0);
  assert.equal(listeners.size, 0);
});

test('the transport deadline still returns the saved overview', async () => {
  const { api, events, requests, timers } = fixture();
  const { signal, listeners } = caller();
  const pending = api.flightOverview(signal);
  await settle();
  assert.equal(requests.length, 1);
  assert.equal(timers.size, 1);
  const [{ callback, delay }] = timers.values();
  assert.equal(delay, 12_000);
  callback();
  assert.equal(await pending, savedOverview);
  assert.equal(signal.aborted, false);
  assert.equal(requests[0].signal.aborted, true);
  assert.deepEqual(events, ['token', 'scope', 'token', 'read']);
  assert.equal(timers.size, 0);
  assert.equal(listeners.size, 0);
});

for (const status of [200, 401]) {
  test(`canceling a queued ${status} cache operation leaves the newer read intact`, async () => {
    const { api, exports, events, requests, timers } = fixture();
    const lockHeld = Promise.withResolvers();
    const releaseLock = Promise.withResolvers();
    const locked = exports.withActiveAndroidPushOrigin(server, () => {
      lockHeld.resolve();
      return releaseLock.promise;
    });
    await lockHeld.promise;
    const { controller, signal, listeners } = caller();
    const pending = api.flightOverview(signal);
    const rejected = assert.rejects(pending, aborted);
    await settle();
    const next = api.flightOverview();
    await settle();
    assert.equal(requests.length, 2);
    requests[0].respond(Response.json({ flights: [] }, { status }));
    await settle();
    assert.deepEqual(events, ['token', 'scope']);
    controller.abort();
    await rejected;
    assert.equal(listeners.size, 0);
    requests[1].respond(Response.json({ flights: [{ id: flightId }] }));
    releaseLock.resolve();
    await locked;
    const result = await next;
    assert.equal(result.flights[0].id, flightId);
    assert.deepEqual(events, ['token', 'scope', 'scope', 'token', 'save']);
    assert.equal(timers.size, 0);
  });
}
