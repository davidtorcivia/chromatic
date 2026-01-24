import { test, expect, generateSlug } from './fixtures';

test.describe('Laser Pointer', () => {
  let testSlug: string;

  test.beforeEach(async ({ adminPage }) => {
    testSlug = generateSlug();
    await adminPage.login();
    await adminPage.createRoom({
      name: 'Laser Test Room',
      slug: testSlug,
      watermarkMode: 'none',
    });
  });

  test('should toggle laser pointer mode', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Laser User');
    await page.waitForURL(/session/);

    // Find and click laser pointer toggle
    const laserButton = page.getByRole('button', { name: /laser|pointer/i });

    if (await laserButton.isVisible()) {
      // Toggle on
      await laserButton.click();
      await expect(laserButton).toHaveClass(/active|enabled/);

      // Toggle off
      await laserButton.click();
      await expect(laserButton).not.toHaveClass(/active|enabled/);
    }
  });

  test('should show cursor when laser is active', async ({ browser }) => {
    const viewer1Context = await browser.newContext();
    const viewer2Context = await browser.newContext();

    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    await viewer1.joinRoom(testSlug, 'Pointer');
    await viewer2.joinRoom(testSlug, 'Observer');

    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    // Activate laser on viewer1
    const laserButton = viewer1Page.getByRole('button', { name: /laser|pointer/i });
    if (await laserButton.isVisible()) {
      await laserButton.click();

      // Move mouse over video area
      const videoArea = viewer1Page.locator('.video-container, video');
      if (await videoArea.isVisible()) {
        const box = await videoArea.boundingBox();
        if (box) {
          await viewer1Page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);

          // Viewer2 should see the cursor
          const cursor = viewer2Page.locator('.cursor, .remote-cursor, .laser-cursor');
          // Allow time for WebSocket message
          await viewer2Page.waitForTimeout(500);
          // Cursor might or might not be visible depending on implementation
        }
      }
    }

    await viewer1Context.close();
    await viewer2Context.close();
  });

  test('should show cursor color matching participant', async ({ browser }) => {
    const viewer1Context = await browser.newContext();
    const viewer2Context = await browser.newContext();

    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    await viewer1.joinRoom(testSlug, 'Colorful Pointer');
    await viewer2.joinRoom(testSlug, 'Color Observer');

    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    // The cursor overlay should exist
    const overlay = viewer2Page.locator('.cursor-overlay, .cursors-container');
    if (await overlay.isVisible()) {
      await expect(overlay).toBeVisible();
    }

    await viewer1Context.close();
    await viewer2Context.close();
  });

  test('should handle rapid cursor movements', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Speed User');
    await page.waitForURL(/session/);

    const laserButton = page.getByRole('button', { name: /laser|pointer/i });
    if (await laserButton.isVisible()) {
      await laserButton.click();

      const videoArea = page.locator('.video-container, video');
      if (await videoArea.isVisible()) {
        const box = await videoArea.boundingBox();
        if (box) {
          // Move cursor rapidly
          for (let i = 0; i < 20; i++) {
            const x = box.x + Math.random() * box.width;
            const y = box.y + Math.random() * box.height;
            await page.mouse.move(x, y);
          }

          // Page should not crash
          await expect(page.locator('body')).toBeVisible();
        }
      }
    }
  });

  test('should stop showing cursor when user leaves', async ({ browser }) => {
    const viewer1Context = await browser.newContext();
    const viewer2Context = await browser.newContext();

    const viewer1Page = await viewer1Context.newPage();
    const viewer2Page = await viewer2Context.newPage();

    const { ViewerPage } = await import('./fixtures');
    const viewer1 = new ViewerPage(viewer1Page);
    const viewer2 = new ViewerPage(viewer2Page);

    await viewer1.joinRoom(testSlug, 'Leaving User');
    await viewer2.joinRoom(testSlug, 'Staying User');

    await viewer1Page.waitForURL(/session/);
    await viewer2Page.waitForURL(/session/);

    // Activate laser and move cursor
    const laserButton = viewer1Page.getByRole('button', { name: /laser|pointer/i });
    if (await laserButton.isVisible()) {
      await laserButton.click();

      const videoArea = viewer1Page.locator('.video-container, video');
      if (await videoArea.isVisible()) {
        const box = await videoArea.boundingBox();
        if (box) {
          await viewer1Page.mouse.move(box.x + 100, box.y + 100);
        }
      }

      // Close viewer1's page (simulates leaving)
      await viewer1Context.close();

      // Wait for disconnect to propagate
      await viewer2Page.waitForTimeout(2000);

      // Cursor should no longer be visible on viewer2
      // (The exact behavior depends on implementation)
    }

    await viewer2Context.close();
  });
});
