const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const { outputText } = ts.transpileModule(readFileSync(join(__dirname, '../src/calendar-sync.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
});
const server = 'https://plano.example';
const legacyKey = `calendar-imports:${server}`;
const currentKey = `calendar-sync-imports:${server}`;
const consentKey = `calendar-sync:${server}`;
const renewedConsent = 'timed:00000000-0000-4000-8000-000000000002';
const date = '2027-05-18';
const key = ({ day, code, utcTime }) => [day, code, utcTime].filter(Boolean).join(' ');
const flight = (number, status) => ({ title: `UA${number}`, startDate: `${date}T13:00:00Z`, timeZone: 'UTC', status });

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function client(events, send, consent = 'timed', importLegacy, { beforeAuth, beforeStore, beforeRead, beforeCalendars } = {}) {
  let now = Date.parse('2027-05-17T12:00:00Z');
  let permissionGranted = true;
  let calendarReads = 0;
  const storage = new Map([[consentKey, consent]]);
  const modern = [];
  const legacy = [];
  const exports = {};
  class Clock extends Date {
    constructor(value = now) { super(value); }
    static now() { return now; }
  }
  const modules = {
    'expo-background-task': {},
    'expo-crypto': { randomUUID: () => renewedConsent.slice('timed:'.length) },
    'expo-secure-store': {
      getItemAsync: async (name) => {
        await beforeRead?.(name);
        return storage.get(name);
      },
      setItemAsync: async (name, value) => {
        await beforeStore?.(name);
        storage.set(name, value);
      },
      deleteItemAsync: async (name) => storage.delete(name),
    },
    'expo-task-manager': { isAvailableAsync: async () => false, isTaskDefined: () => true },
    'react-native': { Platform: { OS: 'android' } },
    'expo-calendar': {
      EntityTypes: { EVENT: 'event' },
      getCalendarPermissions: async () => ({ granted: permissionGranted }),
      requestCalendarPermissions: async () => ({ granted: permissionGranted }),
      getCalendars: async (entity) => {
        assert.equal(entity, 'event');
        await beforeCalendars?.();
        return [{ id: 'private calendar ID' }];
      },
      listEvents: async () => {
        calendarReads += 1;
        return events;
      },
    },
    './api': {
      loadServer: async () => server,
      prepareCalendarBackground: async () => {},
      clearCalendarBackground: async () => {},
      serverStorageKey: (scope, host) => `${scope}:${host}`,
      createAPI: (_, options) => {
        assert.equal(options.access, 'background');
        async function authenticated(kind, request) {
          await beforeAuth?.(kind);
          return options.withRequestAuthorization ? options.withRequestAuthorization(request) : request();
        }
        return {
          hasSavedInstallationToken: async () => true,
          syncCalendarFlights: (flights) => authenticated('modern', async () => {
            modern.push(JSON.parse(JSON.stringify(flights)));
            return send ? send(flights, storage) : flights.map(key);
          }),
          importFlights: (source, text) => authenticated('legacy', async () => {
            assert.equal(source, 'calendar');
            legacy.push(text);
            return importLegacy ? importLegacy(text) : {
              unmatched: [], flights: (text.match(/BEGIN:VEVENT/g) ?? []).map((_, index) => ({ id: `flight-${index}` })),
            };
          }),
        };
      },
    },
    './flight': { dateKey: (instant) => instant.toISOString().slice(0, 10) },
  };
  runInNewContext(outputText, {
    Date: Clock,
    exports,
    require: (name) => {
      assert.ok(Object.hasOwn(modules, name), `Unexpected import ${name}`);
      return modules[name];
    },
  });
  return {
    storage, modern, legacy,
    resume: exports.resumeCalendarSync,
    setEnabled: (enabled) => exports.setCalendarSyncEnabled(server, enabled),
    setPermission: (granted) => { permissionGranted = granted; },
    calendarReads: () => calendarReads,
    advance: () => { now += exports.calendarSyncIntervalMs; },
  };
}

test('old acknowledgments cannot starve later calendar entries or be evicted by modern acknowledgments', async () => {
  const existing = Array.from({ length: 64 }, (_, index) => flight(100 + index));
  const fresh = Array.from({ length: 14 }, (_, index) => flight(500 + index));
  const sync = client([...fresh, flight(600, 'canceled'), ...existing, flight(601, 'cancelled')]);
  const oldLog = JSON.stringify({ attemptedAt: 0, nextBatch: 0, imported: existing.map(({ title }) => `${date} ${title} 130000`) });
  sync.storage.set(legacyKey, oldLog);
  await sync.resume();
  assert.deepEqual(sync.modern.map((group) => group.length), [4, 4, 4]);
  sync.advance();
  await sync.resume();
  assert.deepEqual(sync.modern.flat().map(({ code }) => code), fresh.map(({ title }) => title));
  assert.equal(sync.storage.get(legacyKey), oldLog);
  assert.equal(JSON.parse(sync.storage.get(currentKey)).imported.length, fresh.length);
  assert.deepEqual(sync.legacy, []);
});

test('a partial reply only acknowledges the submitted keys it confirms', async () => {
  const sync = client([flight(100), flight(101), flight(102)], (flights) => [key(flights[1]), `${date} UA999`]);
  await sync.resume();
  assert.deepEqual(JSON.parse(sync.storage.get(currentKey)).imported, [`${date} UA101 130000`]);
  sync.advance();
  await sync.resume();
  assert.deepEqual(sync.modern[1].map(({ code }) => code), ['UA102', 'UA100']);
});

test('stale legacy keys age out without overriding the modern throttle after clock rollback', async () => {
  const events = [flight(100), flight(101)];
  const sync = client(events);
  const attemptedAt = Date.parse('2027-05-20T12:00:00Z');
  sync.storage.set(legacyKey, JSON.stringify({
    attemptedAt, nextBatch: 5, imported: ['2027-05-10 UA123', `${date} UA100 130000`],
  }));
  await sync.resume();
  assert.deepEqual(JSON.parse(sync.storage.get(legacyKey)), {
    consent: 'timed', attemptedAt, nextBatch: 5, imported: [`${date} UA100 130000`],
  });
  events.push(flight(102));
  await sync.resume();
  assert.deepEqual(sync.modern.flat().map(({ code }) => code), ['UA101']);
});

test('an old server keeps date-only consent and a separate legacy acknowledgment', async () => {
  const sync = client([flight(100)], () => null, 'on');
  await sync.resume();
  assert.deepEqual(sync.modern, [[{ day: date, code: 'UA100' }]]);
  assert.match(sync.legacy[0], /DTSTART;VALUE=DATE:20270518/);
  assert.deepEqual(JSON.parse(sync.storage.get(legacyKey)).imported, [`${date} UA100`]);
  assert.deepEqual(JSON.parse(sync.storage.get(currentKey)).imported, []);
});

test('legacy sync acknowledges both codeshares when the server deduplicates their flight', async () => {
  const sync = client([flight(100), { ...flight(100), title: 'LH8100' }], () => null, 'timed', () => ({
    unmatched: [], flights: [{ id: 'flight-100' }],
  }));
  await sync.resume();
  assert.equal(sync.legacy.length, 1);
  assert.match(sync.legacy[0], /SUMMARY:LH8100/);
  assert.match(sync.legacy[0], /SUMMARY:UA100/);
  assert.deepEqual(JSON.parse(sync.storage.get(legacyKey)).imported, [`${date} LH8100 130000`, `${date} UA100 130000`]);
  assert.deepEqual(JSON.parse(sync.storage.get(currentKey)).imported, []);
  sync.advance();
  await sync.resume();
  assert.equal(sync.legacy.length, 1);
  assert.equal(sync.modern.length, 1);
});

test('legacy sync retries only the unmatched entry from a deduplicated batch', async () => {
  const sync = client([flight(100), { ...flight(100), title: 'LH8100' }, flight(102)], () => null, 'timed', (text) => ({
    unmatched: [`UA102 · ${date}`],
    flights: text.includes('SUMMARY:UA100') ? [{ id: 'flight-100' }] : [],
  }));
  await sync.resume();
  assert.deepEqual(JSON.parse(sync.storage.get(legacyKey)).imported, [`${date} LH8100 130000`, `${date} UA100 130000`]);
  sync.advance();
  await sync.resume();
  assert.equal(sync.legacy.length, 2);
  assert.match(sync.legacy[1], /SUMMARY:UA102/);
  assert.doesNotMatch(sync.legacy[1], /SUMMARY:(LH8100|UA100)/);
});

test('legacy sync leaves incomplete or inconsistent replies unacknowledged', async () => {
  const result = { unmatched: [], flights: [{ id: 'flight-100' }] };
  for (const reply of [
    { ...result, truncated: true },
    { ...result, nextOffset: 1 },
    { ...result, unmatched: [`UA999 · ${date}`] },
    { ...result, flights: [] },
    { ...result, flights: [...result.flights, ...result.flights] },
  ]) {
    const sync = client([flight(100)], () => null, 'timed', () => reply);
    await sync.resume();
    assert.deepEqual(JSON.parse(sync.storage.get(legacyKey)).imported, [], JSON.stringify(reply));
  }
});

test('a retired consent cannot retry through the legacy import path', async () => {
  const sync = client([flight(100)], (_, storage) => {
    storage.set(consentKey, renewedConsent);
    return null;
  });
  await sync.resume();
  assert.equal(sync.modern.length, 1);
  assert.deepEqual(sync.legacy, []);
});

for (const kind of ['modern', 'legacy']) {
  test(`renewed consent skips a pending ${kind} request and starts a fresh run`, async () => {
    const events = [flight(100)];
    const entered = deferred();
    const release = deferred();
    let paused = false;
    const sync = client(events, kind === 'legacy' ? () => null : undefined, 'timed', undefined, {
      async beforeAuth(requestKind) {
        if (paused || requestKind !== kind) return;
        paused = true;
        entered.resolve();
        await release.promise;
      },
    });
    const pending = sync.resume();
    await entered.promise;
    await sync.setEnabled(false);
    events.splice(0, 1, flight(200));
    await sync.setEnabled(true);
    release.resolve();
    await pending;
    await new Promise(setImmediate);

    if (kind === 'modern') {
      assert.deepEqual(sync.modern.flat().map(({ code }) => code), ['UA200']);
      assert.deepEqual(sync.legacy, []);
    } else {
      assert.deepEqual(sync.modern.flat().map(({ code }) => code), ['UA100', 'UA200']);
      assert.equal(sync.legacy.length, 1);
      assert.match(sync.legacy[0], /SUMMARY:UA200/);
    }
    const log = JSON.parse(sync.storage.get(kind === 'legacy' ? legacyKey : currentKey));
    assert.equal(log.consent, renewedConsent);
    assert.deepEqual(log.imported, [`${date} UA200 130000`]);
  });
}

test('calendar sync rechecks OS permission after authentication', async () => {
  const entered = deferred();
  const release = deferred();
  const sync = client([flight(100)], undefined, 'timed', undefined, {
    async beforeAuth() {
      entered.resolve();
      await release.promise;
    },
  });
  const pending = sync.resume();
  await entered.promise;
  sync.setPermission(false);
  release.resolve();
  await pending;
  assert.deepEqual(sync.modern, []);
  assert.deepEqual(sync.legacy, []);
});

test('calendar sync rechecks OS permission after the final saved consent read', { timeout: 2000 }, async () => {
  const entered = deferred();
  const release = deferred();
  let authenticating = false;
  const sync = client([flight(100)], undefined, 'timed', undefined, {
    beforeAuth() { authenticating = true; },
    async beforeRead(name) {
      if (!authenticating || name !== consentKey) return;
      authenticating = false;
      entered.resolve();
      await release.promise;
    },
  });
  const pending = sync.resume();
  await entered.promise;
  sync.setPermission(false);
  release.resolve();
  await pending;
  assert.deepEqual(sync.modern, []);
});

test('disabling calendar sync retires a scan before it reads events', async () => {
  const entered = deferred();
  const release = deferred();
  const sync = client([flight(100)], undefined, 'timed', undefined, {
    async beforeCalendars() {
      entered.resolve();
      await release.promise;
    },
  });
  const pending = sync.resume();
  await entered.promise;
  await sync.setEnabled(false);
  release.resolve();
  await pending;
  assert.equal(sync.calendarReads(), 0);
  assert.deepEqual(sync.modern, []);
});

test('disabling calendar sync drains pending journal writes before deleting them', async () => {
  const entered = deferred();
  const release = deferred();
  const sync = client([flight(100)], undefined, 'timed', undefined, {
    async beforeStore(name) {
      if (name !== currentKey) return;
      entered.resolve();
      await release.promise;
    },
  });
  const pending = sync.resume();
  await entered.promise;
  let disabled = false;
  const off = sync.setEnabled(false).then(() => { disabled = true; });
  await new Promise(setImmediate);
  const drained = !disabled;
  release.resolve();
  await Promise.all([pending, off]);
  assert.equal(drained, true);
  assert.equal(sync.storage.has(currentKey), false);
  assert.equal(sync.storage.has(legacyKey), false);
  assert.equal(sync.storage.has(consentKey), false);
  assert.deepEqual(sync.modern, []);
});
