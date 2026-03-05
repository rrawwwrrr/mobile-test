// WebdriverIO config for running ApiDemos tests against an external Appium server.
//
// Required environment variables:
//   APPIUM_HOST      – hostname of the Appium server (default: localhost)
//   APPIUM_PORT      – port of the Appium server (default: 4723)
//   ANDROID_SERIAL   – device serial number forwarded to Appium capabilities

const appiumHost = process.env.APPIUM_HOST || 'localhost';
const appiumPort = parseInt(process.env.APPIUM_PORT || '4723', 10);
const deviceSerial = process.env.ANDROID_SERIAL || '';

exports.config = {
  runner: 'local',

  // Connect to the external Appium server (no appium service).
  hostname: appiumHost,
  port: appiumPort,
  path: '/',

  specs: ['./test/specs/**/*.spec.js'],
  maxInstances: 1,

  capabilities: [
    {
      platformName: 'Android',
      'appium:deviceName': deviceSerial,
      'appium:udid': deviceSerial,
      'appium:automationName': 'UiAutomator2',
      'appium:app': '/app/ApiDemos-debug.apk',
      'appium:appPackage': 'io.appium.android.apis',
      'appium:appActivity': '.ApiDemos',
      'appium:noReset': false,
      'appium:newCommandTimeout': 90,
      'appium:androidInstallTimeout': 120000,
      // Non-rooted devices deny WRITE_SECURE_SETTINGS; ignore that error and continue.
      'appium:ignoreHiddenApiPolicyError': true,
    },
  ],

  framework: 'mocha',
  mochaOpts: {
    timeout: 120000,
  },

  reporters: [
    ['spec', {
      addConsoleLogs: true,
      realtimeReporting: true,
    }],
  ],

  // Log level: trace | debug | info | warn | error | silent
  logLevel: 'info',
};
