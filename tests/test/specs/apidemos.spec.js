// Basic smoke tests for ApiDemos application.
// These verify that the app launches and primary navigation works.

describe('ApiDemos – Launch', () => {
  it('should display the main menu list', async () => {
    const list = await $('android.widget.ListView');
    await expect(list).toBeDisplayed();
  });

  it('should show top-level categories', async () => {
    const accessibility = await $('//android.widget.TextView[@text="Accessibility"]');
    await expect(accessibility).toBeDisplayed();

    const views = await $('//android.widget.TextView[@text="Views"]');
    await expect(views).toBeDisplayed();
  });
});

describe('ApiDemos – Navigation', () => {
  it('should navigate into Views and return', async () => {
    const views = await $('//android.widget.TextView[@text="Views"]');
    await views.click();

    // Inside Views sub-menu there should be "Animation" among others
    const animation = await $('//android.widget.TextView[@text="Animation"]');
    await expect(animation).toBeDisplayed();

    // Go back to main screen
    await driver.back();

    const list = await $('android.widget.ListView');
    await expect(list).toBeDisplayed();
  });

  it('should navigate into App section', async () => {
    const app = await $('//android.widget.TextView[@text="App"]');
    await app.click();

    const activity = await $('//android.widget.TextView[@text="Activity"]');
    await expect(activity).toBeDisplayed();

    await driver.back();
  });
});
