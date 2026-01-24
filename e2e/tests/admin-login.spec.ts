import { test, expect } from './fixtures';

test.describe('Admin Authentication', () => {
  test('should redirect to login when not authenticated', async ({ page }) => {
    await page.goto('/admin');
    await expect(page).toHaveURL('/admin/login');
  });

  test('should show error for invalid token', async ({ page }) => {
    await page.goto('/admin/login');
    await page.getByLabel('Admin Token').fill('invalid-token');
    await page.getByRole('button', { name: 'Sign In' }).click();

    // Should show error message
    await expect(page.getByText(/invalid|unauthorized/i)).toBeVisible();

    // Should stay on login page
    await expect(page).toHaveURL('/admin/login');
  });

  test('should login with valid token', async ({ adminPage, page }) => {
    await adminPage.login();

    // Should be on admin dashboard
    await expect(page).toHaveURL('/admin');

    // Should see admin dashboard content
    await expect(page.getByRole('heading', { name: /dashboard|rooms/i })).toBeVisible();
  });

  test('should logout successfully', async ({ adminPage, page }) => {
    // Login first
    await adminPage.login();
    await expect(page).toHaveURL('/admin');

    // Logout
    await adminPage.logout();

    // Should be redirected to login
    await expect(page).toHaveURL('/admin/login');

    // Trying to access admin should redirect to login
    await page.goto('/admin');
    await expect(page).toHaveURL('/admin/login');
  });

  test('should persist session across page reloads', async ({ adminPage, page }) => {
    await adminPage.login();

    // Reload the page
    await page.reload();

    // Should still be authenticated
    await expect(page).toHaveURL('/admin');
    await expect(page.getByRole('button', { name: 'Logout' })).toBeVisible();
  });

  test('login form should have proper accessibility', async ({ page }) => {
    await page.goto('/admin/login');

    // Check that form elements have proper labels
    const tokenInput = page.getByLabel('Admin Token');
    await expect(tokenInput).toBeVisible();
    await expect(tokenInput).toHaveAttribute('type', 'password');

    // Check submit button
    const submitButton = page.getByRole('button', { name: 'Sign In' });
    await expect(submitButton).toBeVisible();
    await expect(submitButton).toBeEnabled();
  });

  test('should handle empty token submission', async ({ page }) => {
    await page.goto('/admin/login');

    // Try to submit without entering token
    await page.getByRole('button', { name: 'Sign In' }).click();

    // Should show validation error or stay on page
    await expect(page).toHaveURL('/admin/login');
  });
});
