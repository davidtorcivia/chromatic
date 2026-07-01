import { test, expect, generateSlug } from './fixtures';

const BASE_URL = process.env.TEST_BASE_URL || 'http://localhost:3000';

test.describe('Chat', () => {
  test('sends and receives messages between two viewers', async ({ adminPage, browser }) => {
    const slug = generateSlug();
    await adminPage.createRoom({ name: 'Chat Test Room', slug, watermarkMode: 'none' });

    const viewer1Context = await browser.newContext({ baseURL: BASE_URL });
    const viewer2Context = await browser.newContext({ baseURL: BASE_URL });
    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    await viewer1.joinRoom(slug, 'User One');
    await viewer2.joinRoom(slug, 'User Two');
    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    await viewer1.sendChatMessage('Hello from User One');
    await viewer2.expectChatMessage('Hello from User One', 'User One');

    await viewer2.sendChatMessage('Hello from User Two');
    await viewer1.expectChatMessage('Hello from User Two', 'User Two');

    await viewer1Context.close();
    await viewer2Context.close();
  });

  test('keeps empty and oversized messages from being sent', async ({ adminPage, viewerPage, page }) => {
    const slug = generateSlug();
    await adminPage.createRoom({ name: 'Chat Validation Room', slug, watermarkMode: 'none' });

    await viewerPage.joinRoom(slug, 'Validation User');
    await page.waitForURL(/session/);
    await viewerPage.openChat();

    const input = page.getByPlaceholder('Message');
    const send = page.getByRole('button', { name: 'Send' });
    await expect(send).toBeDisabled();

    await input.fill('a'.repeat(2500));
    await expect(input).toHaveValue('a'.repeat(2000));
  });
});
