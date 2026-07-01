import { test, expect } from '@playwright/test';

const ADMIN_TOKEN = process.env.ADMIN_TOKEN || 'test-admin-token';

test.describe('Admin authentication', () => {
  test('redirects unauthenticated admin users to the root login form', async ({ page }) => {
    await page.goto('/admin');

    await expect(page).toHaveURL('/');
    await expect(page.getByRole('heading', { name: 'Admin Login' })).toBeVisible();
    await expect(page.getByLabel('Admin Token')).toHaveAttribute('type', 'password');
  });

  test('rejects an invalid admin token', async ({ request }) => {
    const response = await request.post('/api/auth/login', {
      data: { token: 'invalid-token' },
      headers: { 'X-Real-IP': '198.51.100.230' },
    });

    expect(response.status()).toBe(401);
    await expect(response).not.toBeOK();
  });

  test('logs in, persists the admin session, and logs out', async ({ page }) => {
    await page.goto('/');
    await page.getByLabel('Admin Token').fill(ADMIN_TOKEN);
    await page.getByRole('button', { name: 'Login' }).click();
    await expect(page).toHaveURL(/\/admin(\/setup)?$/);

    await page.goto('/admin/rooms');
    await expect(page.getByRole('heading', { name: 'Rooms' })).toBeVisible();

    await page.locator('button[title="Logout"]').click();
    await page.waitForURL('/');
    await expect(page.getByRole('heading', { name: 'Admin Login' })).toBeVisible();
  });
});
