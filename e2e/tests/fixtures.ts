import { test as base, expect, type APIRequestContext, type BrowserContext, type Page } from '@playwright/test';

/**
 * Test fixtures for Chromatic E2E tests
 */

// Admin credentials from environment or defaults
const ADMIN_TOKEN = process.env.ADMIN_TOKEN || 'test-admin-token';
const BASE_URL = process.env.TEST_BASE_URL || 'http://localhost:3000';
let cachedAdminCookies: Awaited<ReturnType<typeof getAdminCookies>> | null = null;
let setupRequestCounter = 0;

function nextSetupHeaders() {
  setupRequestCounter += 1;
  return {
    Authorization: `Bearer ${ADMIN_TOKEN}`,
    'X-Real-IP': `198.51.100.${setupRequestCounter}`,
  };
}

async function getAdminCookies(request: APIRequestContext) {
  if (cachedAdminCookies) {
    return cachedAdminCookies;
  }

  const response = await request.post('/api/auth/login', {
    data: { token: ADMIN_TOKEN },
    headers: { 'X-Real-IP': '198.51.100.250' },
  });
  expect(response.ok(), await response.text()).toBeTruthy();

  const setCookie = response.headers()['set-cookie'];
  expect(setCookie, 'admin login should set a session cookie').toBeTruthy();

  const [cookiePair] = setCookie.split(';');
  const [name, ...valueParts] = cookiePair.split('=');
  cachedAdminCookies = [{
    name,
    value: valueParts.join('='),
    url: BASE_URL,
    httpOnly: true,
    secure: BASE_URL.startsWith('https://'),
    sameSite: 'Strict' as const,
  }];
  return cachedAdminCookies;
}

// Page Object Model for Admin pages
export class AdminPage {
  constructor(private page: Page, private context: BrowserContext, private request: APIRequestContext) {}

  async authenticate() {
    await this.context.addCookies(await getAdminCookies(this.request));
  }

  async goto() {
    await this.authenticate();
    await this.page.goto('/admin');
  }

  async login() {
    await this.goto();
    await expect(this.page).toHaveURL(/\/admin(\/setup)?$/);
  }

  async logout() {
    await this.page.getByRole('button', { name: 'Logout' }).click();
    await this.page.waitForURL('/');
  }

  async createRoom(options: {
    name: string;
    slug: string;
    password?: string;
    waitingRoomEnabled?: boolean;
    watermarkMode?: 'none' | 'text' | 'logo' | 'both';
  }) {
    const response = await this.request.post('/api/rooms', {
      data: {
        name: options.name,
        slug: options.slug,
        password: options.password,
        waitingRoomEnabled: Boolean(options.waitingRoomEnabled),
        watermarkMode: options.watermarkMode ?? 'none',
      },
      headers: nextSetupHeaders(),
    });
    expect(response.ok(), await response.text()).toBeTruthy();

    return await response.json();
  }

  async deleteRoom(slug: string) {
    await this.authenticate();
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
    await this.page.getByLabel(/your name/i).fill(name);

    // Fill in password if required
    if (password) {
      await this.page.getByLabel('Password').fill(password);
    }

    // Click join
    await this.page.getByRole('button', { name: /join session/i }).click();
  }

  async waitForWaitingRoom() {
    await expect(this.page.getByText(/waiting|host/i)).toBeVisible();
  }

  async waitForSession() {
    await this.page.waitForURL(/\/room\/.*\/session/);
  }

  async sendChatMessage(message: string) {
    await this.openChat();
    await this.page.getByPlaceholder('Message').fill(message);
    await this.page.getByRole('button', { name: 'Send' }).click();
  }

  async expectChatMessage(message: string, sender?: string) {
    await this.openChat();
    const chatArea = this.page.locator('.chat-messages');
    await expect(chatArea.getByText(message)).toBeVisible();
    if (sender) {
      await expect(chatArea.locator('.chat-message-author').getByText(sender, { exact: true })).toBeVisible();
    }
  }

  async openChat() {
    if (await this.page.locator('.chat-panel').isVisible().catch(() => false)) {
      return;
    }
    await this.page.getByRole('button', { name: /chat/i }).click();
    await expect(this.page.locator('.chat-panel')).toBeVisible();
  }

  async toggleMicrophone() {
    await this.page.getByRole('button', { name: /microphone/i }).click();
  }

  async leaveRoom() {
    await this.page.getByRole('button', { name: /leave/i }).click();
  }
}

// Extended test fixture with page objects
export const test = base.extend<{
  adminPage: AdminPage;
  viewerPage: ViewerPage;
}>({
  adminPage: async ({ page, context, request }, use) => {
    await use(new AdminPage(page, context, request));
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
