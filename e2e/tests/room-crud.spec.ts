import { test, expect, generateSlug } from './fixtures';

test.describe('Room management', () => {
  test('creates a basic room and opens its management page', async ({ adminPage, page }) => {
    const slug = generateSlug();
    const roomName = `Test Room ${slug}`;

    await adminPage.createRoom({ name: roomName, slug, watermarkMode: 'none' });
    await adminPage.authenticate();
    await page.goto(`/admin/rooms/${slug}`);

    await expect(page).toHaveURL(`/admin/rooms/${slug}`);
    await expect(page.getByRole('heading', { name: roomName })).toBeVisible();
    await expect(page.getByText('Invite Link')).toBeVisible();
  });

  test('updates and deletes a room', async ({ adminPage, page }) => {
    const slug = generateSlug();
    await adminPage.createRoom({ name: 'Original Name', slug, watermarkMode: 'none' });
    await adminPage.authenticate();
    await page.goto(`/admin/rooms/${slug}`);

    const newName = `Updated ${slug}`;
    await page.getByLabel('Room Name').fill(newName);
    await page.getByRole('button', { name: 'Save Changes' }).click();
    await expect(page.getByText('Room updated successfully')).toBeVisible();
    await expect(page.getByRole('heading', { name: newName })).toBeVisible();

    await page.getByRole('button', { name: 'Delete Room' }).click();
    await page.getByRole('dialog').getByRole('button', { name: 'Delete Room' }).click();
    await page.waitForURL('/admin/rooms');

    await page.goto(`/admin/rooms/${slug}`);
    await expect(page.getByText(/room not found/i)).toBeVisible();
  });
});
