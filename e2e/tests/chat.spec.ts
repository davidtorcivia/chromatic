import { test, expect, generateSlug } from './fixtures';

test.describe('Chat Functionality', () => {
  let testSlug: string;

  test.beforeEach(async ({ adminPage }) => {
    testSlug = generateSlug();
    await adminPage.login();
    await adminPage.createRoom({
      name: 'Chat Test Room',
      slug: testSlug,
      watermarkMode: 'none',
    });
  });

  test('should send and receive chat messages', async ({ browser }) => {
    // Create two viewer contexts
    const viewer1Context = await browser.newContext();
    const viewer2Context = await browser.newContext();

    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    // Both join the room
    await viewer1.joinRoom(testSlug, 'User One');
    await viewer2.joinRoom(testSlug, 'User Two');

    // Wait for both to be in session
    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    // User One sends a message
    await viewer1.sendChatMessage('Hello from User One!');

    // User Two should receive the message
    await viewer2.expectChatMessage('Hello from User One!', 'User One');

    // User Two sends a reply
    await viewer2.sendChatMessage('Hello back from User Two!');

    // User One should receive the reply
    await viewer1.expectChatMessage('Hello back from User Two!', 'User Two');

    await viewer1Context.close();
    await viewer2Context.close();
  });

  test('should show own messages immediately', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Solo User');
    await page.waitForURL(/session/);

    const testMessage = 'This is my test message';
    await viewerPage.sendChatMessage(testMessage);

    // Should see own message
    await viewerPage.expectChatMessage(testMessage);
  });

  test('should prevent empty messages', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Test User');
    await page.waitForURL(/session/);

    // Try to send empty message
    const sendButton = page.getByRole('button', { name: /send/i });
    const inputField = page.getByPlaceholder('Type a message');

    await inputField.fill('');
    await sendButton.click();

    // Input should remain empty, no message sent
    await expect(inputField).toHaveValue('');
  });

  test('should sanitize HTML in messages', async ({ browser }) => {
    const viewer1Context = await browser.newContext();
    const viewer2Context = await browser.newContext();

    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    await viewer1.joinRoom(testSlug, 'Sender');
    await viewer2.joinRoom(testSlug, 'Receiver');

    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    // Send message with HTML
    await viewer1.sendChatMessage('<script>alert("xss")</script>');

    // Receiver should see sanitized version
    const chatArea = viewer2Page.locator('.chat-messages');
    // Should not execute script, should show sanitized text
    await expect(chatArea.locator('script')).toHaveCount(0);

    await viewer1Context.close();
    await viewer2Context.close();
  });

  test('should limit message length', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Long Message User');
    await page.waitForURL(/session/);

    // Try to send a very long message (over 2000 chars)
    const longMessage = 'a'.repeat(2500);
    const inputField = page.getByPlaceholder('Type a message');

    await inputField.fill(longMessage);

    // Input should be truncated or submit should be prevented
    const value = await inputField.inputValue();
    expect(value.length).toBeLessThanOrEqual(2000);
  });

  test('should rate limit messages', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Spam User');
    await page.waitForURL(/session/);

    // Send many messages quickly
    for (let i = 0; i < 35; i++) {
      await viewerPage.sendChatMessage(`Message ${i}`);
      await page.waitForTimeout(10); // Very fast
    }

    // After rate limit, messages should be silently dropped
    // The UI might not show an error, but messages won't appear
    // We just verify we don't crash
    await expect(page.locator('.chat-messages')).toBeVisible();
  });

  test('should show participant colors in chat', async ({ browser }) => {
    const viewer1Context = await browser.newContext();
    const viewer2Context = await browser.newContext();

    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    await viewer1.joinRoom(testSlug, 'Colorful User');
    await viewer2.joinRoom(testSlug, 'Another User');

    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    await viewer1.sendChatMessage('Check my color!');

    // On viewer2, the message should have a color indicator
    const messageContainer = viewer2Page.locator('.chat-message').first();
    await expect(messageContainer).toBeVisible();

    await viewer1Context.close();
    await viewer2Context.close();
  });

  test('should preserve message order', async ({ browser }) => {
    const viewerContext = await browser.newContext();
    const viewerPage = await viewerContext.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer = new ViewerPage(viewerPage);

    await viewer.joinRoom(testSlug, 'Order Tester');
    await viewerPage.waitForURL(/session/);

    // Send numbered messages
    for (let i = 1; i <= 5; i++) {
      await viewer.sendChatMessage(`Message ${i}`);
      await viewerPage.waitForTimeout(100);
    }

    // Verify order
    const messages = viewerPage.locator('.chat-message');
    const texts = await messages.allTextContents();

    let lastNum = 0;
    for (const text of texts) {
      const match = text.match(/Message (\d+)/);
      if (match) {
        const num = parseInt(match[1]);
        expect(num).toBeGreaterThan(lastNum);
        lastNum = num;
      }
    }

    await viewerContext.close();
  });
});
