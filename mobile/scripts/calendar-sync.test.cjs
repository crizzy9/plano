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
const date = '2027-05-18';
const key = ({ day, code, utcTime }) => [day, code, utcTime].filter(Boolean).join(' ');
const flight = (number, status) => ({ title: `UA${number}`, startDate: `${date}T13:00:00Z`, timeZone: 'UTC', status });

function client(events, send, consent = 'timed', importLegacy) {
  let now = Date.parse('2027-05-17T12:00:00Z');
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
    'expo-crypto': {},
    'expo-secure-store': {
      getItemAsync: async (name) => storage.get(name),
      setItemAsync: async (name, value) => storage.set(name, value),
    },
    'expo-task-manager': { isAvailableAsync: async () => false, isTaskDefined: () => true },
    'react-native': { Platform: { OS: 'android' } },
    'expo-calendar': {
      EntityTypes: { EVENT: 'event' },
      getCalendarPermissions: async () => ({ granted: true }),
      getCalendars: async (entity) => {
        assert.equal(entity, 'event');
        return [{ id: 'private calendar ID' }];
      },
      listEvents: async () => events,
    },
    './api': {
      loadServer: async () => server,
      serverStorageKey: (scope, host) => `${scope}:${host}`,
      createAPI: (_, options) => {
        assert.equal(options.access, 'background');
        return {
          hasSavedInstallationToken: async () => true,
          syncCalendarFlights: async (flights) => {
            modern.push(JSON.parse(JSON.stringify(flights)));
            return send ? send(flights, storage) : flights.map(key);
          },
          importFlights: async (source, text) => {
            assert.equal(source, 'calendar');
            legacy.push(text);
            return importLegacy ? importLegacy(text) : {
              unmatched: [], flights: (text.match(/BEGIN:VEVENT/g) ?? []).map((_, index) => ({ id: `flight-${index}` })),
            };
          },
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
    storage.set(consentKey, 'timed:00000000-0000-4000-8000-000000000002');
    return null;
  });
  await sync.resume();
  assert.equal(sync.modern.length, 1);
  assert.deepEqual(sync.legacy, []);
});
