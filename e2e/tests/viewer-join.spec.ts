import { test, expect, generateSlug } from './fixtures';

const BASE_URL = process.env.TEST_BASE_URL || 'http://localhost:3000';

test.describe('Viewer join flow', () => {
  test('joins a public room and creates an HttpOnly-cookie session', async ({ adminPage, viewerPage, page }) => {
    const slug = generateSlug();
    await adminPage.createRoom({ name: 'Viewer Join Test', slug, watermarkMode: 'none' });

    await viewerPage.joinRoom(slug, 'Test Viewer');

    await page.waitForURL(new RegExp(`/room/${slug}/session`));
    await expect(page.locator('.session-page')).toBeVisible();
  });

  test('protects password rooms', async ({ adminPage, viewerPage, page }) => {
    const slug = generateSlug();
    const password = 'test-password-123';
    await adminPage.createRoom({
      name: 'Password Test Room',
      slug,
      password,
      watermarkMode: 'none',
    });

    await page.goto(`/room/${slug}`);
    await expect(page.getByText('Private session')).toBeVisible();
    await expect(page.getByLabel('Password')).toBeVisible();

    await viewerPage.joinRoom(slug, 'Wrong Password User', 'wrong-password');
    await expect(page.getByText(/invalid password|unauthorized/i)).toBeVisible();

    await viewerPage.joinRoom(slug, 'Correct Password User', password);
    await page.waitForURL(new RegExp(`/room/${slug}/session`));
  });

  test('sends waiting-room participants to the waiting page and allows admin admission', async ({ adminPage, page, browser }) => {
    const slug = generateSlug();
    await adminPage.createRoom({
      name: 'Waiting Room Test',
      slug,
      waitingRoomEnabled: true,
      watermarkMode: 'none',
    });

    const viewerContext = await browser.newContext({ baseURL: BASE_URL });
    const viewerPage = await viewerContext.newPage();
    const { ViewerPage } = await import('./fixtures');
    const viewer = new ViewerPage(viewerPage);

    await viewer.joinRoom(slug, 'Waiting Viewer');
    await viewerPage.waitForURL(new RegExp(`/room/${slug}/waiting`));

    await adminPage.authenticate();
    await page.goto(`/admin/rooms/${slug}`);
    await expect(page.getByText('Waiting Viewer')).toBeVisible();
    await page.getByRole('button', { name: 'Admit', exact: true }).click();

    await viewerPage.waitForURL(new RegExp(`/room/${slug}/session`), { timeout: 10000 });
    await viewerContext.close();
  });
});
