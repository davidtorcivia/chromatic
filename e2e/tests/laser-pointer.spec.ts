import { test, expect, generateSlug } from './fixtures';

test.describe('Review tools', () => {
  test('toggles laser pointer mode without destabilizing the session', async ({ adminPage, viewerPage, page }) => {
    const slug = generateSlug();
    await adminPage.createRoom({ name: 'Laser Test Room', slug, watermarkMode: 'none' });

    await viewerPage.joinRoom(slug, 'Laser User');
    await page.waitForURL(/session/);

    const laserButton = page.getByRole('button', { name: /laser/i });
    await expect(laserButton).toBeVisible();

    await laserButton.click();
    await expect(laserButton).toHaveClass(/active/);

    await laserButton.click();
    await expect(laserButton).not.toHaveClass(/active/);
    await expect(page.locator('.session-page')).toBeVisible();
  });
});
