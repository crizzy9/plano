const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const { outputText } = ts.transpileModule(readFileSync(join(__dirname, '../src/api.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
});
const origin = 'https://fixture.plano.test';
const installation = 'fixture-installation-token';
const pushClock = '11111111-1111-4111-8111-111111111111';
const device = { platform: 'ios', kind: 'alert', token: 'fixture-apns-token', liveRevision: 12, pushClock };

function fixture(capability, register = () => new Response(null, { status: 204 })) {
  const calls = [];
  const exports = {};
  runInNewContext(outputText, {
    exports, __DEV__: false, process: { env: {} }, AbortController, URL, URLSearchParams, setTimeout, clearTimeout,
    require: (name) => name === 'react-native' ? { Platform: { OS: 'ios' } } : {},
    fetch: async (url, init) => {
      const endpoint = new URL(url);
      assert.equal(endpoint.origin, origin);
      calls.push({ path: endpoint.pathname, body: init.body ? JSON.parse(init.body) : null });
      if (endpoint.pathname === '/v1/devices/capabilities') return capability();
      assert.equal(endpoint.pathname, '/v1/devices');
      assert.equal(init.method, 'POST');
      assert.equal(init.headers.Authorization, `Bearer ${installation}`);
      return register(calls.at(-1).body);
    },
  });
  return { api: exports.createAPI(origin), calls };
}

test('a compatible server receives and caches support for iOS push clocks', async () => {
  const { api, calls } = fixture(() => Response.json({ iosPushClock: true }));
  await api.registerDevice(device, installation);
  await api.registerDevice({ ...device, liveRevision: 13 }, installation);
  assert.deepEqual(calls, [
    { path: '/v1/devices/capabilities', body: null },
    { path: '/v1/devices', body: device },
    { path: '/v1/devices', body: { ...device, liveRevision: 13 } },
  ]);
});

test('a route-missing response uses the legacy device wire until the server upgrades', async () => {
  let upgraded = false;
  const { api, calls } = fixture(() => upgraded ? Response.json({ iosPushClock: true }) :
    new Response('404 page not found\n', { status: 404 }));
  await api.registerDevice(device, installation);
  assert.equal(Object.hasOwn(calls[1].body, 'pushClock'), false);
  assert.equal(calls[1].body.liveRevision, 12);
  upgraded = true;
  await api.registerDevice({ ...device, liveRevision: 13 }, installation);
  assert.deepEqual(calls[3].body, { ...device, liveRevision: 13 });
});

test('a transient error or unrelated 404 cannot downgrade iOS push registration', async () => {
  for (const response of [
    new Response('unavailable', { status: 503 }),
    Response.json({ error: 'record not found' }, { status: 404 }),
    new Response('proxy route missing', { status: 404 }),
  ]) {
    const { api, calls } = fixture(() => response);
    await assert.rejects(api.registerDevice(device, installation));
    assert.deepEqual(calls, [{ path: '/v1/devices/capabilities', body: null }]);
  }
});

test('a rejected clocked write rechecks capability before a rollback fallback', async () => {
  let supportsClock = true;
  const { api, calls } = fixture(
    () => supportsClock ? Response.json({ iosPushClock: true }) : new Response('404 page not found\n', { status: 404 }),
    (body) => !supportsClock && body.pushClock ? Response.json({ error: 'invalid request' }, { status: 400 }) :
      new Response(null, { status: 204 }),
  );
  await api.registerDevice(device, installation);
  supportsClock = false;
  await api.registerDevice({ ...device, liveRevision: 13 }, installation);
  const { pushClock: _, ...legacy } = device;
  assert.deepEqual(calls.slice(2), [
    { path: '/v1/devices', body: { ...device, liveRevision: 13 } },
    { path: '/v1/devices/capabilities', body: null },
    { path: '/v1/devices', body: { ...legacy, liveRevision: 13 } },
  ]);
});
