const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

function connectionFixture(connection) {
  const hooks = [];
  const timers = new Map();
  let index = 0;
  let focusCleanup;
  let nextTimer = 0;
  const jsx = (type, props) => ({ type, props });
  const modules = {
    react: {
      useCallback: (callback) => callback,
      useEffect: (effect) => { hooks[index++] ??= { cleanup: effect() }; },
      useRef: (current) => hooks[index++] ??= { current },
      useState(initial) {
        const slot = index++;
        if (!(slot in hooks)) hooks[slot] = initial;
        return [hooks[slot], (value) => {
          hooks[slot] = typeof value === 'function' ? value(hooks[slot]) : value;
        }];
      },
    },
    'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: 'Fragment' },
    '@react-navigation/native': { useFocusEffect: (effect) => { focusCleanup ??= effect(); } },
    'react-native': {
      AppState: { currentState: 'active', addEventListener: () => ({ remove() {} }) },
      Platform: { OS: 'android' }, StyleSheet: { create: (styles) => styles }, Text: 'Text', View: 'View',
    },
    './flight': { errorText: (cause) => cause.message },
    './theme': { usePalette: () => ({ colors: {} }) },
    './ui': { Action: 'Action', Body: 'Body', ErrorMessage: 'ErrorMessage', Heading: 'Heading', layout: {} },
  };
  function load(file, extra = '') {
    const source = readFileSync(join(__dirname, '../src', file), 'utf8');
    const { outputText } = ts.transpileModule(source + extra, {
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
    });
    const exports = {};
    runInNewContext(outputText, {
      exports,
      require: (id) => modules[id] ?? {},
      AbortController,
      URL,
      setTimeout(callback, delay) {
        const id = ++nextTimer;
        timers.set(id, { callback, delay });
        return id;
      },
      clearTimeout: (id) => timers.delete(id),
    });
    return exports;
  }
  modules['./screens'] = load('screens.tsx');
  const { CheckedConnection } = load('tools.tsx', '\nexport { CheckedConnection };');
  const props = {
    api: { connection },
    inbound: { id: 'UA123' },
    outbound: { id: 'UA456' },
    monitorBusy: false,
    changeMonitoring: async () => assert.fail('no monitor mutation expected'),
  };
  const settle = () => new Promise(setImmediate);
  return {
    render() {
      index = 0;
      return CheckedConnection(props);
    },
    settle,
    async poll() {
      assert.equal(timers.size, 1);
      const [[id, { callback, delay }]] = timers;
      assert.equal(delay, 30_000);
      timers.delete(id);
      callback();
      await settle();
    },
  };
}

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes);
  return tree && typeof tree === 'object' ? [tree, ...nodes(tree.props.children)] : [];
}

const assessment = {
  risk: 'Relaxed', airport: 'EWR', availableMinutes: 120, transferMinutes: 35,
  departureGate: 'C12', instructions: ['Follow the AirTrain signs.'],
  mapUrl: 'https://www.newarkairport.com/', guideUrl: 'https://www.newarkairport.com/flights/connections-guide',
  monitorEligible: true,
};

test('a failed connection poll hides prior guidance until a fresh assessment arrives', async () => {
  let calls = 0;
  const fixture = connectionFixture(async (inbound, outbound, signal) => {
    assert.equal(inbound, 'UA123');
    assert.equal(outbound, 'UA456');
    assert.ok(signal instanceof AbortSignal && !signal.aborted);
    switch (++calls) {
      case 1: return assessment;
      case 2: throw new Error('This pair no longer connects at the same airport.');
      default: return { ...assessment, departureGate: 'B60', instructions: ['Follow the signs to Terminal B.'] };
    }
  });
  fixture.render();
  await fixture.settle();
  const ready = nodes(fixture.render());
  assert.ok(ready.some(({ type, props }) => type === 'Body' && props.children === assessment.instructions[0]));
  assert.ok(ready.some(({ props }) => props.label === 'Open EWR terminal map'));
  assert.ok(ready.some(({ props }) => props.label === 'Monitor connection'));

  await fixture.poll();
  const failed = nodes(fixture.render());
  assert.ok(failed.some(({ props }) => props.message === 'This pair no longer connects at the same airport.'));
  assert.deepEqual(failed.filter(({ type }) => type === 'Action').map(({ props }) => props.label), ['Recheck connection']);
  assert.ok(failed.every(({ type }) => !['Heading', 'Body', 'Text', 'ConnectionGate'].includes(type.name ?? type)));

  await fixture.poll();
  const recovered = nodes(fixture.render());
  assert.ok(recovered.some(({ type, props }) => type === 'Body' && props.children === 'Follow the signs to Terminal B.'));
  assert.ok(recovered.some(({ type, props }) => type.name === 'ConnectionGate' && props.reported === 'B60'));
  assert.ok(recovered.some(({ props }) => props.label === 'Monitor connection'));
  assert.ok(recovered.every(({ props }) => props.message === undefined));
});

test('only a confirmed disruption or departure hides guidance for a missed connection', async () => {
  let guidanceAvailable = true;
  const fixture = connectionFixture(async () => ({
    ...assessment, risk: 'Missed', availableMinutes: -10, forecastTransferMinutes: 65, monitoring: true, guidanceAvailable,
  }));
  fixture.render();
  await fixture.settle();
  const projected = nodes(fixture.render());
  assert.ok(projected.some(({ props }) => props.label === 'Open EWR terminal map'));
  assert.ok(projected.some(({ props }) => props.children === assessment.instructions[0]));

  guidanceAvailable = false;
  await fixture.poll();
  const terminal = nodes(fixture.render());
  assert.ok(terminal.some(({ type, props }) => type === 'Heading' && props.children.join('') === 'Missed connection at EWR'));
  assert.deepEqual(terminal.filter(({ type }) => type === 'Action').map(({ props }) => props.label), ['Stop monitoring']);
  assert.ok(terminal.every(({ type, props }) => type.name !== 'ConnectionGate' && props.children !== assessment.instructions[0]));
  assert.ok(terminal.every(({ type, props }) => type !== 'Body' || !String(props.children).startsWith('Historical gates suggest')));
});
