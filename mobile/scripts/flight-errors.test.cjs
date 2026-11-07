const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const test = require('node:test');
const { runInNewContext } = require('node:vm');
const ts = require('typescript');

const { outputText } = ts.transpileModule(readFileSync(join(__dirname, '../src/flight.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
});
const exportsUnderTest = {};
runInNewContext(outputText, { exports: exportsUnderTest, Error });
const { errorText } = exportsUnderTest;
const storageMessage = 'Plano couldn’t access secure storage. Unlock your device and try again. If this continues, contact the app developer.';

test('the captured iOS SecureStore failure shows storage recovery without native diagnostics', () => {
  // Actual Settings error from the iOS demo run on 2026-11-07.
  const captured = new Error("FunctionCallException: Calling the 'getValueWithKeyAsync' function has failed (at ExpoModulesCore/AsyncFunctionDefinition.swift:123)\n→ Caused by: KeyChainException: A required entitlement isn't present. (at ExpoSecureStore/SecureStoreModule.swift:168)");
  assert.equal(errorText(captured), storageMessage);
  assert.doesNotMatch(errorText(captured), /Exception|Expo|\.swift|:\d+/);
  assert.match(captured.message, /ExpoSecureStore\/SecureStoreModule\.swift:168/);
});

test('known Keychain codes and SecureStore operations retain the same recovery copy', () => {
  const coded = Object.assign(new Error('User interaction is not allowed.'), { code: 'ERR_KEY_CHAIN' });
  assert.equal(errorText(coded), storageMessage);
  for (const operation of ['get', 'set', 'delete']) {
    const cause = new Error(`FunctionCallException: Calling the '${operation}ValueWithKeyAsync' function has failed\n→ Caused by: KeyChainException: I/O error.`);
    assert.equal(errorText(cause), storageMessage);
  }
});

test('validation, API, permission, revocation and unrelated native errors keep their deliberate copy', () => {
  for (const message of [
    'Use an HTTPS address for your Plano server.',
    'Check the flight date, airport codes, and optional details.',
    'Too many requests. Try again in a moment.',
    'Plano’s server is temporarily unavailable.',
    'Couldn’t reach Plano. Check the server address and your connection.',
    'Allow notifications for Plano in your device settings.',
    'Access for this server ended.',
    'Plano paused new alerts. Existing alerts may continue until your server receives the pending revocation.',
    'Plano couldn’t save its Live Activity registration.',
    'The server could not access its keychain or secure storage.',
    "FunctionCallException: Calling the 'getPermissionsAsync' function has failed (at ExpoModulesCore/AsyncFunctionDefinition.swift:123)\n→ Caused by: PermissionException: Allow notifications.",
    "FunctionCallException: Calling the 'getValueWithKeyAsync' function has failed\n→ Caused by: InvalidKeyException: Invalid key",
  ]) {
    assert.equal(errorText(new Error(message)), message);
  }
  const apiError = Object.assign(new Error('Access for this server ended.'), { status: 401, code: 'ERR_FUNCTION_CALL' });
  assert.equal(errorText(apiError), apiError.message);
});

test('unknown non-Error failures keep the existing fallback', () => {
  for (const cause of [undefined, null, 'failure', { message: 'failure', code: 'ERR_KEY_CHAIN' }]) {
    assert.equal(errorText(cause), 'Something went wrong. Try again.');
  }
});
