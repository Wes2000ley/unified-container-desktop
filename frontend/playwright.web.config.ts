import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: './e2e-web',
  workers: 1,
  retries: 0,
  reporter: [['list']],
  use: {
    ...devices['Desktop Chrome'],
    viewport: { width: 1440, height: 1000 },
    trace: 'off',
    screenshot: 'only-on-failure',
  },
});
