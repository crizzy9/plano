const { lstatSync } = require('node:fs');
const { join } = require('node:path');
const { withAndroidManifest, withAppBuildGradle } = require('expo/config-plugins');

const googleServicesPlugin = /^\s*(?:apply\s+plugin:\s*|id\s*\(?\s*)['"]com\.google\.gms\.google-services['"]/m;

module.exports = (config) => {
  config = withAppBuildGradle(config, (current) => {
    if (!current.android?.googleServicesFile) {
      const copiedServices = join(current.modRequest.platformProjectRoot, 'app', 'google-services.json');
      // Incremental prebuild leaves the previous Firebase file and plugin in place.
      if (googleServicesPlugin.test(current.modResults.contents) || lstatSync(copiedServices, { throwIfNoEntry: false })) {
        throw new Error('Generated Android files still contain Firebase configuration. Set PLANO_GOOGLE_SERVICES_FILE for this build or generate Android in a fresh native directory.');
      }
    }
    return current;
  });

  return withAndroidManifest(config, (current) => {
    const manifest = current.modResults.manifest;
    manifest.$['xmlns:tools'] ||= 'http://schemas.android.com/tools';
    const application = manifest.application[0];
    application.service ||= [];
    const inheritedService = 'expo.modules.notifications.service.ExpoFirebaseMessagingService';
    if (!application.service.some((service) => service.$['android:name'] === inheritedService)) {
      // Plano subclasses Expo's receiver to keep its token events. Remove the second
      // Firebase intent handler so Android always dispatches data to our receiver.
      application.service.push({ $: { 'android:name': inheritedService, 'tools:node': 'remove' } });
    }
    return current;
  });
};
