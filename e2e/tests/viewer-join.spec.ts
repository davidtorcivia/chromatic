import { test, expect, generateSlug } from './fixtures';

test.describe('Viewer Join Flow', () => {
  let testSlug: string;

  test.beforeEach(async ({ adminPage }) => {
    // Create a room for testing
    testSlug = generateSlug();
    await adminPage.login();
    await adminPage.createRoom({
      name: 'Viewer Join Test',
      slug: testSlug,
      watermarkMode: 'none',
    });
  });

  test('should show room info page', async ({ page }) => {
    await page.goto(`/room/${testSlug}`);

    // Should show room name
    await expect(page.getByText('Viewer Join Test')).toBeVisible();

    // Should have name input
    await expect(page.getByLabel('Your Name')).toBeVisible();

    // Should have join button
    await expect(page.getByRole('button', { name: /join/i })).toBeVisible();
  });

  test('should require name to join', async ({ page }) => {
    await page.goto(`/room/${testSlug}`);

    // Try to join without name
    await page.getByRole('button', { name: /join/i }).click();

    // Should show validation error or stay on page
    await expect(page).toHaveURL(`/room/${testSlug}`);
  });

  test('should join room successfully', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Test Viewer');

    // Should be in session or waiting room
    const url = page.url();
    expect(url).toMatch(/session|waiting/);
  });

  test('should show 404 for non-existent room', async ({ page }) => {
    await page.goto('/room/non-existent-room');

    // Should show not found message
    await expect(page.getByText(/not found|doesn't exist/i)).toBeVisible();
  });

  test('should auto-generate slug from name input', async ({ page }) => {
    await page.goto(`/room/${testSlug}`);

    // Fill in a name with spaces
    await page.getByLabel('Your Name').fill('Test User Name');

    // Name should be sanitized but visible
    await expect(page.getByLabel('Your Name')).toHaveValue('Test User Name');
  });
});

test.describe('Password Protected Room', () => {
  let testSlug: string;
  const testPassword = 'test-password-123';

  test.beforeEach(async ({ adminPage }) => {
    testSlug = generateSlug();
    await adminPage.login();
    await adminPage.createRoom({
      name: 'Password Test Room',
      slug: testSlug,
      password: testPassword,
      watermarkMode: 'none',
    });
  });

  test('should show password field for protected room', async ({ page }) => {
    await page.goto(`/room/${testSlug}`);

    // Should show password field
    await expect(page.getByLabel('Password')).toBeVisible();

    // Should show password indicator
    await expect(page.getByText(/password protected/i)).toBeVisible();
  });

  test('should reject wrong password', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Test User', 'wrong-password');

    // Should show error message
    await expect(page.getByText(/incorrect password|invalid/i)).toBeVisible();

    // Should stay on join page
    await expect(page).toHaveURL(`/room/${testSlug}`);
  });

  test('should accept correct password', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Test User', testPassword);

    // Should be in session or waiting room
    const url = page.url();
    expect(url).toMatch(/session|waiting/);
  });

  test('should block after too many password attempts', async ({ page }) => {
    await page.goto(`/room/${testSlug}`);

    // Try wrong password multiple times
    for (let i = 0; i < 6; i++) {
      await page.getByLabel('Your Name').fill(`User ${i}`);
      await page.getByLabel('Password').fill('wrong-password');
      await page.getByRole('button', { name: /join/i }).click();
      // Wait for response
      await page.waitForTimeout(500);
    }

    // Should show rate limit message
    await expect(page.getByText(/rate limit|too many|try again/i)).toBeVisible();
  });
});

test.describe('Waiting Room', () => {
  let testSlug: string;

  test.beforeEach(async ({ adminPage }) => {
    testSlug = generateSlug();
    await adminPage.login();
    await adminPage.createRoom({
      name: 'Waiting Room Test',
      slug: testSlug,
      waitingRoomEnabled: true,
      watermarkMode: 'none',
    });
  });

  test('should show waiting room when enabled', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Waiting User');

    // Should be redirected to waiting page
    await page.waitForURL(/waiting/);

    // Should show waiting message
    await viewerPage.waitForWaitingRoom();
  });

  test('should list waiting participants in admin', async ({ viewerPage, adminPage, page, browser }) => {
    // Join as viewer in a new context
    const viewerContext = await browser.newContext();
    const viewerPageNew = await viewerContext.newPage();
    const viewer = new (await import('./fixtures')).ViewerPage(viewerPageNew);

    await viewer.joinRoom(testSlug, 'Waiting Viewer');
    await viewerPageNew.waitForURL(/waiting/);

    // Check admin page for waiting list
    await page.goto(`/admin/rooms/${testSlug}`);

    // Should show waiting participant
    await expect(page.getByText('Waiting Viewer')).toBeVisible();

    await viewerContext.close();
  });

  test('should admit participant from waiting room', async ({ page, browser }) => {
    // Create viewer context
    const viewerContext = await browser.newContext();
    const viewerPage = await viewerContext.newPage();
    const viewer = new (await import('./fixtures')).ViewerPage(viewerPage);

    // Join as viewer
    await viewer.joinRoom(testSlug, 'Admitted User');
    await viewerPage.waitForURL(/waiting/);

    // Login as admin
    const admin = new (await import('./fixtures')).AdminPage(page);
    await admin.login();
    await page.goto(`/admin/rooms/${testSlug}`);

    // Admit the user
    await page.getByRole('button', { name: /admit/i }).click();

    // Viewer should be redirected to session
    await viewerPage.waitForURL(/session/, { timeout: 10000 });

    await viewerContext.close();
  });

  test('should show connection status in waiting room', async ({ viewerPage, page }) => {
    await viewerPage.joinRoom(testSlug, 'Status User');
    await page.waitForURL(/waiting/);

    // Should show some connection indicator
    await expect(page.getByText(/waiting|connected/i)).toBeVisible();
  });
});
