// Basic smoke tests for ApiDemos application.
// These verify that the app launches and primary navigation works.

// Scroll the main list until the item with given text is visible, then return it.
async function scrollToText(text) {
  return $(`android=new UiScrollable(new UiSelector().scrollable(true)).scrollIntoView(new UiSelector().text("${text}"))`);
}

describe('ApiDemos – Launch', () => {
  it('should display the main menu list', async () => {
    const list = await $('android.widget.ListView');
    await expect(list).toBeDisplayed();
  });

  it('should show top-level categories', async () => {
    const accessibility = await scrollToText('Accessibility');
    await expect(accessibility).toBeDisplayed();

    const views = await scrollToText('Views');
    await expect(views).toBeDisplayed();
  });
});

describe('ApiDemos – Navigation', () => {
  it('should navigate into Views and return', async () => {
    const views = await scrollToText('Views');
    await views.click();

    // Inside Views sub-menu there should be "Animation" among others
    const animation = await scrollToText('Animation');
    await expect(animation).toBeDisplayed();

    // Go back to main screen
    await driver.back();

    const list = await $('android.widget.ListView');
    await expect(list).toBeDisplayed();
  });

  it('should navigate into App section', async () => {
    const app = await scrollToText('App');
    await app.click();

    const activity = await scrollToText('Activity');
    await expect(activity).toBeDisplayed();

    await driver.back();
  });
});
