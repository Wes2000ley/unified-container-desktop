import { test, expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import { once } from 'node:events';
import path from 'node:path';

let server: ChildProcessWithoutNullStreams;
let launchURL: string;

test.beforeAll(async () => {
  const binary = process.env.UCD_CLI || path.resolve('../bin/ucd.exe');
  server = spawn(binary, ['web'], { windowsHide: true });
  launchURL = await new Promise<string>((resolve, reject) => {
    let output = '';
    const timeout = setTimeout(
      () => reject(new Error('Browser adapter did not start')),
      15000,
    );
    server.on('error', reject);
    server.once('exit', (code) =>
      reject(new Error(`Browser adapter exited (${code})`)),
    );
    server.stdout.on('data', (chunk: Buffer) => {
      output += chunk.toString();
      const line = output.split(/\r?\n/)[0] || '';
      if (/^http:\/\/127\.0\.0\.1:\d+\/#[-_A-Za-z0-9]{43}$/.test(line)) {
        clearTimeout(timeout);
        resolve(line);
      }
    });
  });
});

test.afterAll(async () => {
  if (server && server.exitCode === null) {
    const exited = once(server, 'exit');
    server.kill();
    await exited;
  }
});

test('serves the shared production UI, forwards actual service responses and retains a tab session', async ({
  page,
}) => {
  const errors: string[] = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const settings = page.waitForResponse(
    (response) =>
      response.url().endsWith('/v1/call') &&
      response.request().postDataJSON()?.method === 'system.settings',
  );
  await page.goto(launchURL);
  const envelope = (await (await settings).json()) as {
    result?: unknown;
    error?: { message: string };
  };
  await expect(
    page.getByRole('heading', { name: 'Dashboard', exact: true }),
  ).toBeVisible();
  if (envelope.error) {
    expect(envelope.error.message).toContain(
      'verify the service is installed and running',
    );
    await expect(
      page.getByText(/verify the service is installed and running/).first(),
    ).toBeVisible();
  } else {
    expect(envelope.result).toBeTruthy();
    await expect(page.getByText('Controller connected')).toBeVisible();
  }
  expect(new URL(page.url()).hash).toBe('');
  expect(
    await page.evaluate(() => sessionStorage.getItem('ucd-web-token')),
  ).toHaveLength(43);
  expect(await page.evaluate(() => '__ucdTestRequests' in window)).toBe(false);

  await page.getByRole('link', { name: 'Skip to content' }).focus();
  await page.keyboard.press('Enter');
  expect(new URL(page.url()).hash).toBe('#main');
  const refreshed = page.waitForResponse(
    (response) =>
      response.url().endsWith('/v1/call') &&
      response.request().postDataJSON()?.method === 'system.settings',
  );
  await page.reload();
  expect((await refreshed).status()).not.toBe(401);
  for (const section of [
    'Projects',
    'Containers',
    'Images',
    'Volumes',
    'Networks',
    'Storage',
    'Registries',
    'Settings',
  ]) {
    await page.getByRole('button', { name: section, exact: true }).click();
    await expect(
      page.getByRole('heading', { name: section, exact: true }),
    ).toBeVisible();
  }
  await page.screenshot({
    path: 'test-results/web-production.png',
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test('requires the launch token in a fresh browser context', async ({
  browser,
}) => {
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await page.goto(new URL('/', launchURL).href);
    await expect(
      page.getByText(/Web session token is missing/).first(),
    ).toBeVisible();
    const unauthorized = await context.request.post(
      new URL('/v1/call', launchURL).href,
      {
        headers: { Origin: new URL(launchURL).origin },
        data: { method: 'engine.list' },
      },
    );
    expect(unauthorized.status()).toBe(401);
  } finally {
    await context.close();
  }
});
