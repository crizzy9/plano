module.exports = ({ config }) => ({
  ...config,
  name: 'Plano',
  slug: 'plano',
  scheme: 'plano',
  version: '0.1.0',
  userInterfaceStyle: 'automatic',
  ios: {
    bundleIdentifier: process.env.PLANO_IOS_BUNDLE_ID || 'app.plano.mobile',
    infoPlist: {
      NSSupportsLiveActivities: true,
      NSAppTransportSecurity: { NSAllowsLocalNetworking: true },
    },
  },
  android: {
    package: process.env.PLANO_ANDROID_PACKAGE || 'app.plano.mobile',
    googleServicesFile: process.env.PLANO_GOOGLE_SERVICES_FILE || undefined,
  },
  plugins: [
    'expo-secure-store',
    ['expo-calendar', { calendarPermission: 'Allow Plano to find flight numbers and departure dates in calendar titles.' }],
    'expo-notifications',
    '@bacons/apple-targets',
    './plugins/withPlanoMessaging',
  ],
});
