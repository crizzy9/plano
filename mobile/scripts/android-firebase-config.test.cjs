const assert = require('node:assert/strict');
const { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const test = require('node:test');
const withPlanoMessaging = require('../plugins/withPlanoMessaging');

const applyGoogleServices = "apply plugin: 'com.google.gms.google-services'";
const staleConfig = /Set PLANO_GOOGLE_SERVICES_FILE for this build or generate Android in a fresh native directory/;

function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), 'plano-firebase-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const android = join(root, 'android');
  mkdirSync(join(android, 'app'), { recursive: true });
  return {
    copiedServices: join(android, 'app', 'google-services.json'),
    run(contents, googleServicesFile) {
      const config = withPlanoMessaging({ android: { googleServicesFile } });
      return config.mods.android.appBuildGradle({
        ...config,
        modRequest: { platformProjectRoot: android },
        modResults: { contents, language: 'groovy' },
      });
    },
  };
}

test('clean Android prebuild can run without Firebase configuration', async (t) => {
  const { run } = fixture(t);
  const contents = "apply plugin: 'com.android.application'";
  assert.equal((await run(contents)).modResults.contents, contents);
});

test('Android prebuild rejects an unconfigured Firebase copy without removing it', async (t) => {
  const { copiedServices, run } = fixture(t);
  writeFileSync(copiedServices, '{}');
  await assert.rejects(run(''), staleConfig);
  assert.equal(readFileSync(copiedServices, 'utf8'), '{}');
  assert.equal((await run(applyGoogleServices, './operator-firebase.json')).modResults.contents, applyGoogleServices);
});

test('Android prebuild rejects a leftover Google Services plugin even without the copied file', async (t) => {
  const { run } = fixture(t);
  await assert.rejects(run(applyGoogleServices), staleConfig);
});
