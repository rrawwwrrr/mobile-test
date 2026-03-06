// WebdriverIO config for running ApiDemos tests against an external Appium server.
//
// Required environment variables:
//   APPIUM_HOST      – hostname of the Appium server (default: localhost)
//   APPIUM_PORT      – port of the Appium server (default: 4723)
//   ANDROID_SERIAL   – device serial number forwarded to Appium capabilities

const appiumHost = process.env.APPIUM_HOST || 'localhost';
const appiumPort = parseInt(process.env.APPIUM_PORT || '4723', 10);
const deviceSerial = process.env.ANDROID_SERIAL || '';

// Appium downloads the APK by URL directly into the Appium container —
// no need to share files between the test container and the Appium container.
const apkUrl = process.env.APIDEMOS_APK_URL ||
  'https://github.com/appium/android-apidemos/releases/download/v6.0.6/ApiDemos-debug.apk';

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
      'appium:app': apkUrl,
      'appium:appPackage': 'io.appium.android.apis',
      'appium:appActivity': '.ApiDemos',
      // Don't clear app data (pm clear is denied on Realme without root),
      // but always force-restart the app so each session starts on the main screen.
      'appium:noReset': true,
      'appium:forceAppLaunch': true,
      'appium:newCommandTimeout': 90,
      'appium:androidInstallTimeout': 120000,
      // UiAutomator2 server install can be slow on first run — give it more time.
      'appium:uiautomator2ServerLaunchTimeout': 60000,
      'appium:uiautomator2ServerInstallTimeout': 60000,
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

  async before() {
    // Press Home to dismiss any system overlay (crash dialogs, setup wizards, etc.)
    // that would block interactions, then bring the app back to the foreground.
    // keycode 3 = KEYCODE_HOME
    try {
      await driver.pressKeyCode(3);
      await driver.activateApp('io.appium.android.apis');
    } catch (e) {
      console.warn('[before] HOME/activate failed:', e.message);
    }
  },
};
