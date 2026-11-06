const assert = require('node:assert/strict');
const { resolve } = require('node:path');
const test = require('node:test');
const { getPrebuildConfigAsync } = require('@expo/prebuild-config');
const { compileModsAsync } = require('expo/config-plugins');

const app = 'example.invalid.plano';
const team = 'FIXTURE123';
const projectRoot = resolve(__dirname, '..');

async function introspect(environment) {
  const fixture = {
    CI: '1', EXPO_OFFLINE: '1', EXPO_NO_TELEMETRY: '1', EXPO_NO_DOTENV: '1',
    PLANO_IOS_BUNDLE_ID: app, PLANO_IOS_TEAM_ID: team,
    PLANO_IOS_APNS_ENVIRONMENT: environment, PLANO_GOOGLE_SERVICES_FILE: undefined,
  };
  const saved = Object.fromEntries(Object.keys(fixture).map((key) => [key, process.env[key]]));
  function setEnvironment(values) {
    for (const [key, value] of Object.entries(values)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
  setEnvironment(fixture);
  try {
    const platforms = ['ios'];
    const { exp } = await getPrebuildConfigAsync(projectRoot, { platforms });
    await compileModsAsync(exp, { projectRoot, platforms, introspect: true, assertMissingModProviders: false });
    return exp;
  } finally {
    setEnvironment(saved);
  }
}

test('iOS prebuild config selects the APNs environment and preserves the custom activity target', async () => {
  for (const [environment, expected] of [[undefined, 'development'], ['production', 'production']]) {
    const config = await introspect(environment);
    const { entitlements, infoPlist } = config._internal.modResults.ios;
    assert.equal(config.ios.bundleIdentifier, app);
    assert.equal(config.ios.appleTeamId, team);
    assert.equal(entitlements['aps-environment'], expected);
    assert.equal(infoPlist.NSSupportsLiveActivities, true);
    assert.ok(config.extra.eas.build.experimental.ios.appExtensions.some((extension) =>
      extension.targetName === 'PlanoActivity' && extension.bundleIdentifier === `${app}.activity` &&
      !extension.entitlements?.['aps-environment']));
  }
});

test('iOS prebuild config rejects a misspelled APNs environment', async () => {
  await assert.rejects(introspect('sandbox'), /PLANO_IOS_APNS_ENVIRONMENT must be development or production/);
});
