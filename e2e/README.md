# Chromatic E2E Tests

End-to-end tests for the Chromatic streaming platform using [Playwright](https://playwright.dev/).

## Prerequisites

1. Node.js 18+ installed
2. Chromatic backend running on `localhost:3000`

## Setup

```bash
# Install dependencies
cd e2e
npm install

# Install Playwright browsers
npx playwright install
```

## Running Tests

```bash
# Run all tests
npm test

# Run tests with UI mode (interactive)
npm run test:ui

# Run tests with browser visible
npm run test:headed

# Run tests in debug mode
npm run test:debug

# Run specific test file
npx playwright test tests/admin-login.spec.ts

# Run tests with specific browser
npx playwright test --project=chromium
npx playwright test --project=firefox
npx playwright test --project=webkit
```

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `TEST_BASE_URL` | Base URL for the application | `http://localhost:3000` |
| `ADMIN_TOKEN` | Admin authentication token | `test-admin-token` |

Example:
```bash
TEST_BASE_URL=http://localhost:3000 ADMIN_TOKEN=my-secret npm test
```

## Test Structure

```
e2e/
├── playwright.config.ts    # Playwright configuration
├── package.json            # Dependencies
├── README.md               # This file
└── tests/
    ├── fixtures.ts         # Test fixtures and page objects
    ├── admin-login.spec.ts # Admin authentication tests
    ├── room-crud.spec.ts   # Room creation/update/delete tests
    ├── viewer-join.spec.ts # Viewer join flow tests
    ├── chat.spec.ts        # Chat functionality tests
    └── laser-pointer.spec.ts # Laser pointer tests
```

## Test Categories

### Admin Authentication (`admin-login.spec.ts`)
- Login with valid/invalid token
- Session persistence
- Logout functionality

### Room CRUD (`room-crud.spec.ts`)
- Create rooms (basic, with password, with waiting room)
- Update room settings
- Delete rooms
- Slug validation

### Viewer Join (`viewer-join.spec.ts`)
- Join public rooms
- Password-protected rooms
- Waiting room flow
- Admin admission

### Chat (`chat.spec.ts`)
- Send/receive messages
- HTML sanitization
- Rate limiting
- Message ordering

### Laser Pointer (`laser-pointer.spec.ts`)
- Toggle laser mode
- Remote cursor visibility
- Cursor cleanup on disconnect

## Writing New Tests

Use the provided fixtures for common operations:

```typescript
import { test, expect, generateSlug } from './fixtures';

test('my new test', async ({ adminPage, viewerPage, page }) => {
  // Use page objects
  await adminPage.login();

  // Create a test room
  const slug = generateSlug();
  await adminPage.createRoom({
    name: 'Test Room',
    slug: slug,
    watermarkMode: 'none',
  });

  // Join as viewer
  await viewerPage.joinRoom(slug, 'Test User');

  // Assert
  await expect(page).toHaveURL(/session/);
});
```

## Viewing Test Reports

After running tests:

```bash
npm run report
```

This opens the HTML report showing test results, screenshots, and traces.

## CI Integration

Tests run automatically in CI with these settings:
- Single worker (sequential execution)
- Retry failed tests twice
- Capture traces on first retry
- Screenshot on failure

Example GitHub Actions workflow:

```yaml
- name: Run E2E tests
  run: |
    cd e2e
    npm ci
    npx playwright install --with-deps
    npm test
  env:
    TEST_BASE_URL: http://localhost:3000
    ADMIN_TOKEN: ${{ secrets.ADMIN_TOKEN }}
```

## Troubleshooting

### Tests fail with "Connection refused"
Ensure the Chromatic backend is running:
```bash
make dev
# or
go run ./cmd/chromatic
```

### Browser installation fails
Run with full dependencies:
```bash
npx playwright install --with-deps
```

### Slow tests
Increase timeout in `playwright.config.ts` or per-test:
```typescript
test('slow test', async ({ page }) => {
  test.setTimeout(60000); // 60 seconds
  // ...
});
```

### Debug a specific test
```bash
npx playwright test --debug tests/chat.spec.ts -g "send and receive"
```
