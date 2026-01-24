import { test, expect, generateSlug } from './fixtures';

test.describe('Room CRUD Operations', () => {
  test.beforeEach(async ({ adminPage }) => {
    await adminPage.login();
  });

  test('should create a basic room', async ({ adminPage, page }) => {
    const slug = generateSlug();
    const roomName = 'Test Room ' + slug;

    await adminPage.createRoom({
      name: roomName,
      slug: slug,
      watermarkMode: 'none',
    });

    // Should be on room detail page
    await expect(page).toHaveURL(`/admin/rooms/${slug}`);

    // Room name should be visible
    await expect(page.getByText(roomName)).toBeVisible();
  });

  test('should create a room with password', async ({ adminPage, page }) => {
    const slug = generateSlug();

    await adminPage.createRoom({
      name: 'Password Protected Room',
      slug: slug,
      password: 'test-password',
      watermarkMode: 'none',
    });

    // Should show that room has password
    await expect(page.getByText(/password protected/i)).toBeVisible();
  });

  test('should create a room with waiting room enabled', async ({ adminPage, page }) => {
    const slug = generateSlug();

    await adminPage.createRoom({
      name: 'Waiting Room Test',
      slug: slug,
      waitingRoomEnabled: true,
      watermarkMode: 'none',
    });

    // Should show waiting room is enabled
    await expect(page.getByText(/waiting room.*enabled/i)).toBeVisible();
  });

  test('should create a room with text watermark', async ({ adminPage, page }) => {
    const slug = generateSlug();

    await page.goto('/admin/rooms/new');
    await page.getByLabel('Room Name').fill('Watermark Room');
    await page.getByLabel('URL Slug').fill(slug);
    await page.getByLabel('Text', { exact: true }).check();
    await page.getByLabel('Watermark Text').fill('{{ name }} - {{ date }}');
    await page.getByRole('button', { name: 'Create Room' }).click();

    await page.waitForURL(`/admin/rooms/${slug}`);
    await expect(page.getByText(/watermark.*text/i)).toBeVisible();
  });

  test('should show error for duplicate slug', async ({ adminPage, page }) => {
    const slug = generateSlug();

    // Create first room
    await adminPage.createRoom({
      name: 'First Room',
      slug: slug,
      watermarkMode: 'none',
    });

    // Try to create another room with same slug
    await page.goto('/admin/rooms/new');
    await page.getByLabel('Room Name').fill('Duplicate Room');
    await page.getByLabel('URL Slug').fill(slug);
    await page.getByRole('button', { name: 'Create Room' }).click();

    // Should show error
    await expect(page.getByText(/already exists|duplicate/i)).toBeVisible();
  });

  test('should validate slug format', async ({ page }) => {
    await page.goto('/admin/rooms/new');

    // Try invalid slugs
    const invalidSlugs = ['AB', 'Test Room', 'test_room', 'a'.repeat(65)];

    for (const invalidSlug of invalidSlugs) {
      await page.getByLabel('URL Slug').fill(invalidSlug);
      await page.getByLabel('Room Name').fill('Test Room');
      await page.getByRole('button', { name: 'Create Room' }).click();

      // Should not navigate away
      await expect(page).toHaveURL('/admin/rooms/new');
    }
  });

  test('should update room name', async ({ adminPage, page }) => {
    const slug = generateSlug();

    await adminPage.createRoom({
      name: 'Original Name',
      slug: slug,
      watermarkMode: 'none',
    });

    // Click edit or update button
    await page.getByRole('button', { name: /edit|update/i }).first().click();

    // Change name
    const newName = 'Updated Name ' + Date.now();
    await page.getByLabel('Room Name').fill(newName);
    await page.getByRole('button', { name: /save|update/i }).click();

    // Should show updated name
    await expect(page.getByText(newName)).toBeVisible();
  });

  test('should delete room', async ({ adminPage, page }) => {
    const slug = generateSlug();

    await adminPage.createRoom({
      name: 'Room to Delete',
      slug: slug,
      watermarkMode: 'none',
    });

    // Delete the room
    await adminPage.deleteRoom(slug);

    // Should be back on admin dashboard
    await expect(page).toHaveURL('/admin');

    // Room should not be in list
    await expect(page.getByText(slug)).not.toBeVisible();
  });

  test('should list all rooms', async ({ adminPage, page }) => {
    // Create a few rooms
    const slugs = [generateSlug(), generateSlug(), generateSlug()];

    for (const slug of slugs) {
      await adminPage.createRoom({
        name: `Room ${slug}`,
        slug: slug,
        watermarkMode: 'none',
      });
    }

    // Go to room list
    await page.goto('/admin');

    // All rooms should be visible
    for (const slug of slugs) {
      await expect(page.getByText(slug)).toBeVisible();
    }
  });

  test('should toggle room status', async ({ adminPage, page }) => {
    const slug = generateSlug();

    await adminPage.createRoom({
      name: 'Status Test Room',
      slug: slug,
      watermarkMode: 'none',
    });

    // Room should start as pending
    await expect(page.getByText(/pending/i)).toBeVisible();

    // Find and click status toggle or start button
    const startButton = page.getByRole('button', { name: /start|go live/i });
    if (await startButton.isVisible()) {
      await startButton.click();
      // Status should change to live
      await expect(page.getByText(/live/i)).toBeVisible();
    }
  });
});
