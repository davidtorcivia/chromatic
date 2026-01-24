import { test as base, expect, type Page } from '@playwright/test';

/**
 * Test fixtures for Chromatic E2E tests
 */

// Admin credentials from environment or defaults
const ADMIN_TOKEN = process.env.ADMIN_TOKEN || 'test-admin-token';

// Page Object Model for Admin pages
export class AdminPage {
  constructor(private page: Page) {}

  async goto() {
    await this.page.goto('/admin');
  }

  async login(token: string = ADMIN_TOKEN) {
    await this.page.goto('/admin/login');
    await this.page.getByLabel('Admin Token').fill(token);
    await this.page.getByRole('button', { name: 'Sign In' }).click();
    // Wait for redirect to admin dashboard
    await this.page.waitForURL('/admin');
  }

  async logout() {
    await this.page.getByRole('button', { name: 'Logout' }).click();
    await this.page.waitForURL('/admin/login');
  }

  async createRoom(options: {
    name: string;
    slug: string;
    password?: string;
    waitingRoomEnabled?: boolean;
    watermarkMode?: 'none' | 'text' | 'logo' | 'both';
  }) {
    await this.page.goto('/admin/rooms/new');

    // Fill in room details
    await this.page.getByLabel('Room Name').fill(options.name);
    await this.page.getByLabel('URL Slug').fill(options.slug);

    if (options.password) {
      await this.page.getByLabel('Room Password').fill(options.password);
    }

    if (options.waitingRoomEnabled) {
      await this.page.getByLabel('Enable Waiting Room').check();
    }

    if (options.watermarkMode) {
      await this.page.getByLabel(options.watermarkMode, { exact: false }).check();
    }

    // Submit form
    await this.page.getByRole('button', { name: 'Create Room' }).click();

    // Wait for navigation to room page
    await this.page.waitForURL(`/admin/rooms/${options.slug}`);
  }

  async deleteRoom(slug: string) {
    await this.page.goto(`/admin/rooms/${slug}`);
    await this.page.getByRole('button', { name: 'Delete Room' }).click();
    // Confirm deletion in dialog
    await this.page.getByRole('button', { name: 'Confirm' }).click();
    await this.page.waitForURL('/admin');
  }
}

// Page Object Model for Viewer pages
export class ViewerPage {
  constructor(private page: Page) {}

  async joinRoom(slug: string, name: string, password?: string) {
    await this.page.goto(`/room/${slug}`);

    // Fill in name
    await this.page.getByLabel('Your Name').fill(name);

    // Fill in password if required
    if (password) {
      await this.page.getByLabel('Password').fill(password);
    }

    // Click join
    await this.page.getByRole('button', { name: 'Join' }).click();
  }

  async waitForWaitingRoom() {
    await expect(this.page.getByText('Waiting for host')).toBeVisible();
  }

  async waitForSession() {
    await this.page.waitForURL(/\/room\/.*\/session/);
  }

  async sendChatMessage(message: string) {
    await this.page.getByPlaceholder('Type a message').fill(message);
    await this.page.getByRole('button', { name: 'Send' }).click();
  }

  async expectChatMessage(message: string, sender?: string) {
    const chatArea = this.page.locator('.chat-messages');
    await expect(chatArea.getByText(message)).toBeVisible();
    if (sender) {
      await expect(chatArea.getByText(sender)).toBeVisible();
    }
  }

  async toggleMicrophone() {
    await this.page.getByRole('button', { name: /microphone/i }).click();
  }

  async leaveRoom() {
    await this.page.getByRole('button', { name: 'Leave' }).click();
  }
}

// Extended test fixture with page objects
export const test = base.extend<{
  adminPage: AdminPage;
  viewerPage: ViewerPage;
}>({
  adminPage: async ({ page }, use) => {
    await use(new AdminPage(page));
  },
  viewerPage: async ({ page }, use) => {
    await use(new ViewerPage(page));
  },
});

export { expect };

// Helper to generate unique room slugs
export function generateSlug(): string {
  return `test-${Date.now()}-${Math.random().toString(36).substring(2, 8)}`;
}

// Helper to wait for API response
export async function waitForApiResponse(
  page: Page,
  urlPattern: string | RegExp,
  options?: { status?: number; timeout?: number }
) {
  const response = await page.waitForResponse(
    (resp) =>
      (typeof urlPattern === 'string'
        ? resp.url().includes(urlPattern)
        : urlPattern.test(resp.url())) &&
      (options?.status === undefined || resp.status() === options.status),
    { timeout: options?.timeout }
  );
  return response;
}
