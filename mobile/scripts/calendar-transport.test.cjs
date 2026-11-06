const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const { outputText } = ts.transpileModule(readFileSync(join(__dirname, '../src/api.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
});
const server = 'https://calendar.plano.test';
const token = 'fixture-calendar-installation';
const flight = { day: '2027-05-18', code: 'UA123' };

function transport() {
  let host = server;
  const calls = [];
  const tokenStarted = Promise.withResolvers();
  const savedToken = Promise.withResolvers();
  const fetchStarted = Promise.withResolvers();
  const response = Promise.withResolvers();
  const modules = {
    'expo-secure-store': {
      async getItemAsync(key, options) {
        assert.equal(options?.keychainService, 'plano.background.v1');
        if (key === 'plano.server.v1') {
          calls.push('host');
          return host;
        }
        assert.ok(key.startsWith('plano.installation.'));
        calls.push('token');
        tokenStarted.resolve();
        return savedToken.promise;
      },
      async deleteItemAsync(key) {
        if (key === 'plano.server.v1') host = null;
      },
    },
    'react-native': { Platform: { OS: 'android' } },
    './live': {},
    './offline-flights': {},
  };
  const exports = {};
  runInNewContext(outputText, {
    exports, __DEV__: false, process: { env: {} },
    AbortController, URL, URLSearchParams, setTimeout, clearTimeout,
    require: (name) => modules[name],
    fetch: async (url, init) => {
      assert.equal(new URL(url).origin, server);
      assert.equal(init.method, 'POST');
      assert.equal(init.headers.Authorization, `Bearer ${token}`);
      calls.push(new URL(url).pathname);
      fetchStarted.resolve();
      return response.promise;
    },
  });
  return {
    api: exports, calls, tokenStarted: tokenStarted.promise,
    allowToken: () => savedToken.resolve(token),
    fetchStarted: fetchStarted.promise, respond: response.resolve,
    currentHost: () => host,
  };
}

for (const [path, send] of [
  ['/v1/import/calendar-sync', (api) => api.syncCalendarFlights([flight])],
  ['/v1/import', (api) => api.importFlights('calendar', 'fixture calendar')],
]) {
  test(`${path} checks authorization after the saved token and host reads`, { timeout: 2000 }, async () => {
    const fixture = transport();
    let activeConsent = 'first';
    const makeClient = (captured) => fixture.api.createAPI(server, {
      access: 'background',
      withRequestAuthorization: async (submit) => {
        fixture.calls.push('consent');
        if (activeConsent !== captured) throw new Error('Calendar consent retired.');
        return submit();
      },
    });
    fixture.respond(Response.json({ completed: [], flights: [], unmatched: [] }));
    const pending = send(makeClient(activeConsent));
    const retired = assert.rejects(pending, /Calendar consent retired/);
    await fixture.tokenStarted;
    activeConsent = 'second';
    fixture.allowToken();
    await retired;
    assert.deepEqual(fixture.calls, ['token', 'host', 'consent']);
    await send(makeClient(activeConsent));
    assert.deepEqual(fixture.calls.slice(3), ['token', 'host', 'consent', path]);
  });
}

test('calendar transport releases the host lock after starting the request', { timeout: 2000 }, async (t) => {
  const fixture = transport();
  t.after(() => fixture.respond(Response.json({ completed: [] })));
  const api = fixture.api.createAPI(server, {
    access: 'background',
    withRequestAuthorization: (submit) => submit(),
  });
  fixture.allowToken();
  const pending = api.syncCalendarFlights([flight]);
  await fixture.fetchStarted;
  await fixture.api.clearCalendarBackground(server);
  assert.equal(fixture.currentHost(), null);
  fixture.respond(Response.json({ completed: [] }));
  await pending;
});
