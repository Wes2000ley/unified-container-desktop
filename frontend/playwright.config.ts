import { defineConfig, devices } from '@playwright/test';
const node = `"${process.execPath}"`;
export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL: 'http://127.0.0.1:4173',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'desktop',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1440, height: 1000 },
      },
    },
  ],
  webServer: {
    command: `${node} node_modules/vite/bin/vite.js build && ${node} node_modules/vite/bin/vite.js preview --host 127.0.0.1 --port 4173 --strictPort`,
    env: { UCD_TEST_BRIDGE: '1' },
    url: 'http://127.0.0.1:4173',
    reuseExistingServer: false,
    timeout: 120000,
  },
});
