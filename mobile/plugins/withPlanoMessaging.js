const { withAndroidManifest } = require('expo/config-plugins');

module.exports = (config) => withAndroidManifest(config, (current) => {
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
