const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

function load(file, modules, globals = {}) {
  const source = readFileSync(join(__dirname, '../src', file), 'utf8');
  const { outputText } = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
  });
  const exports = {};
  runInNewContext(outputText, { exports, require: (id) => modules[id] ?? {}, ...globals });
  return exports;
}

function nodes(tree) {
  if (Array.isArray(tree)) return tree.flatMap(nodes);
  return tree && typeof tree === 'object' ? [tree, ...nodes(tree.props.children)] : [];
}

function pickupFixture(api) {
  const hooks = [];
  let index = 0;
  let refreshRevision = 0;
  const effects = [];
  const jsx = (type, props) => ({ type, props });
  const { PickupLinks } = load('pickup-links.tsx', {
    react: {
      useEffect(callback, dependencies) {
        const slot = index++;
        const previous = hooks[slot];
        if (previous && dependencies.every((value, i) => Object.is(value, previous.dependencies[i]))) return;
        effects.push(() => {
          previous?.cleanup?.();
          hooks[slot] = { dependencies, cleanup: callback() };
        });
      },
      useRef(current) {
        const slot = index++;
        return hooks[slot] ??= { current };
      },
      useState(initial) {
        const slot = index++;
        if (!(slot in hooks)) hooks[slot] = initial;
        return [hooks[slot], (value) => {
          hooks[slot] = typeof value === 'function' ? value(hooks[slot]) : value;
        }];
      },
    },
    'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: 'Fragment' },
    'react-native': { View: 'View' },
    './flight': { errorText: (cause) => cause.message, formatDay: (value) => value, formatTime: () => '12:00' },
    './ui': { Action: 'Action', Body: 'Body', ErrorMessage: 'ErrorMessage', Heading: 'Heading', Loading: 'Loading', layout: {} },
  });
  function render() {
    index = 0;
    const tree = PickupLinks({ api, flightId: 'UA123', refreshRevision });
    for (const effect of effects.splice(0)) effect();
    return nodes(tree);
  }
  return {
    render,
    refreshFromParent() {
      refreshRevision++;
      render();
    },
    press(label) {
      const action = render().find(({ type, props }) => type === 'Action' && props.label === label);
      assert.ok(action && !action.props.disabled, `${label} should be enabled`);
      action.props.onPress();
      render();
    },
  };
}

const oldLink = { id: 'old', expiresAt: 'old expiration' };
const newLink = { id: 'new', expiresAt: 'new expiration' };
const settle = () => new Promise(setImmediate);
const hasText = (tree, text) => tree.some(({ type, props }) =>
  type === 'Body' && (Array.isArray(props.children) ? props.children.join('') : props.children) === text);

test('reopening the list and refreshing after share cannot restore a revoked link', async () => {
  const deletion = Promise.withResolvers();
  const finishReads = Promise.withResolvers();
  let currentLinks = [oldLink];
  let reads = 0;
  const fixture = pickupFixture({
    async pickupLinks() {
      const links = currentLinks;
      if (++reads > 1) await finishReads.promise;
      return { links };
    },
    async revokePickupLink(id) {
      assert.equal(id, oldLink.id);
      await deletion.promise;
      currentLinks = [newLink];
    },
  });
  fixture.press('Manage pickup links');
  await settle();
  assert.ok(hasText(fixture.render(), 'Expires old expiration at 12:00'));

  fixture.press('Revoke link');
  fixture.press('Hide pickup links');
  fixture.press('Manage pickup links');
  fixture.refreshFromParent();
  deletion.resolve();
  await settle();
  fixture.render();
  finishReads.resolve();
  await settle();

  const current = fixture.render();
  assert.ok(!hasText(current, 'Expires old expiration at 12:00'));
  assert.ok(hasText(current, 'Expires new expiration at 12:00'));
  assert.equal(reads, 2);
});

test('a failed revoke refreshes the list and keeps the error available for retry', async () => {
  let reads = 0;
  const fixture = pickupFixture({
    async pickupLinks() {
      reads++;
      return { links: [oldLink] };
    },
    async revokePickupLink() {
      throw new Error('Couldn’t revoke this pickup link.');
    },
  });
  fixture.press('Manage pickup links');
  await settle();
  fixture.press('Revoke link');
  await settle();
  fixture.render();
  await settle();

  const current = fixture.render();
  assert.ok(hasText(current, 'Expires old expiration at 12:00'));
  assert.ok(current.some(({ props }) => props.message === 'Couldn’t revoke this pickup link.'));
  assert.ok(current.some(({ props }) => props.label === 'Revoke link' && !props.disabled));
  assert.equal(reads, 2);
});

test('an already-revoked pickup link succeeds while a missing route fails', async () => {
  let response = Response.json({ error: 'That pickup link is unavailable.' }, { status: 404 });
  const { createAPI } = load('api.ts', {
    'react-native': { Platform: { OS: 'android' } },
    'expo-secure-store': { getItemAsync: async () => 'fixture-installation' },
  }, {
    AbortController, setTimeout, clearTimeout, __DEV__: false, process: { env: {} },
    fetch: async (url, options) => {
      assert.equal(url, 'https://plano.test/v1/shares/old');
      assert.equal(options.method, 'DELETE');
      assert.equal(options.headers.Authorization, 'Bearer fixture-installation');
      return response;
    },
  });
  const api = createAPI('https://plano.test');
  await api.revokePickupLink(oldLink.id);
  response = new Response('404 page not found', { status: 404 });
  await assert.rejects(api.revokePickupLink(oldLink.id), /unavailable/);
});
