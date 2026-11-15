import { test, expect } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import path from 'node:path';

test('keeps settings drafts while switching categories and supports keyboard navigation', async ({
  page,
}) => {
  await page.goto('/');
  await page
    .getByRole('button', { name: 'View containers', exact: true })
    .click();
  await expect(
    page.getByRole('heading', { name: 'Containers', exact: true }),
  ).toBeVisible();
  await expect(page.getByText('Clear selection', { exact: true })).toHaveCount(
    0,
  );
  await page.getByLabel('Select web-service', { exact: true }).check();
  await expect(
    page.getByRole('button', { name: 'Clear selection' }),
  ).toBeVisible();
  await page.getByRole('button', { name: 'Clear selection' }).click();
  await expect(
    page.getByLabel('Select web-service', { exact: true }),
  ).not.toBeChecked();
  await page.screenshot({
    path: 'test-results/containers.png',
    fullPage: true,
  });
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByLabel('Refresh data every').selectOption('15');
  const sections = page.getByRole('navigation', { name: 'Settings sections' });
  await sections.getByRole('button', { name: 'Proxies', exact: true }).click();
  await page
    .getByLabel('Docker proxy HTTP URL')
    .fill('http://draft.example:8080');
  for (const name of [
    'Resources',
    'File sharing',
    'Network',
    'Access',
    'Activity',
    'General',
  ]) {
    await sections.getByRole('button', { name, exact: true }).click();
    expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
  }
  await expect(page.getByLabel('Refresh data every')).toHaveValue('15');
  const proxies = sections.getByRole('button', {
    name: 'Proxies',
    exact: true,
  });
  await proxies.focus();
  await page.keyboard.press('Enter');
  await expect(proxies).toHaveAttribute('aria-current', 'page');
  await expect(page.getByLabel('Docker proxy HTTP URL')).toHaveValue(
    'http://draft.example:8080',
  );
  await page.screenshot({ path: 'test-results/proxies.png', fullPage: true });
  await page.setViewportSize({ width: 1100, height: 800 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
});

test('renders workspace, retains failed pull input, and confirms volume removal', async ({
  page,
}) => {
  await page.goto('/');
  await expect(page.getByText('Controller connected')).toBeVisible();
  await expect(
    page.getByText('Linux engine is unavailable').first(),
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({ path: 'test-results/dashboard.png', fullPage: true });
  await page.getByRole('button', { name: 'Images', exact: true }).click();
  await page.getByRole('button', { name: /Pull image/ }).click();
  const dialog = page.getByRole('dialog', { name: 'Pull image' });
  await dialog.getByLabel('Image reference').fill('broken:latest');
  await dialog.getByRole('button', { name: 'Pull image' }).click();
  await expect(
    dialog.getByText('registry authentication failed'),
  ).toBeVisible();
  await expect(dialog.getByLabel('Image reference')).toHaveValue(
    'broken:latest',
  );
  await page.screenshot({
    path: 'test-results/pull-error.png',
    fullPage: true,
  });
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await page.getByRole('button', { name: 'Volumes', exact: true }).click();
  await page.getByRole('button', { name: 'Remove', exact: true }).click();
  const confirmation = page.getByRole('dialog', {
    name: 'Remove volume app-data?',
  });
  await expect(confirmation).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByText('app-data', { exact: true })).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Remove', exact: true }),
  ).toBeFocused();
  await page.getByRole('button', { name: 'Remove', exact: true }).click();
  await page
    .getByRole('dialog', { name: 'Remove volume app-data?' })
    .getByRole('button', { name: 'Remove volume' })
    .click();
  await expect(page.getByText('app-data removed.')).toBeVisible();
});

test('configures interface preferences and Windows desktop behavior', async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.goto('/');
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('button', { name: 'Activity', exact: true }).click();
  await expect(
    page.getByRole('heading', { name: 'Recent events' }),
  ).toBeVisible();
  await page.getByRole('button', { name: 'General', exact: true }).click();
  await page.getByLabel('Refresh data every').selectOption('15');
  await page.getByLabel('Default log lines').selectOption('1000');
  await page
    .getByLabel('Include timestamps in container logs by default')
    .check();
  await page.getByRole('button', { name: 'Save preferences' }).click();
  await expect(
    page.getByText('Preferences saved on this device.'),
  ).toBeVisible();

  await page
    .getByLabel('Start Unified Container Desktop when I sign in to Windows')
    .check();
  await page.getByLabel('Start minimized to the notification area').check();
  await page.getByRole('button', { name: 'Save desktop preferences' }).click();
  await expect(page.getByText('Desktop preferences saved.')).toBeVisible();
  const saved = await page.evaluate(
    () =>
      (
        window as unknown as {
          __ucdTestRequests: {
            method: string;
            params: Record<string, boolean>;
          }[];
        }
      ).__ucdTestRequests.find(
        (request) => request.method === 'desktop.preferences.set',
      )?.params,
  );
  expect(saved).toEqual({
    startAtLogin: true,
    startMinimized: true,
    closeToTray: true,
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  expect(pageErrors).toEqual([]);
  await page.screenshot({ path: 'test-results/settings.png', fullPage: true });
});

test('reports partial Compose action failures without a success message', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Projects', exact: true }).click();
  await expect(page.getByText('Web stack')).toBeVisible();
  await page.screenshot({ path: 'test-results/projects.png', fullPage: true });
  await page.getByRole('button', { name: 'Deploy', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Web stack · Deploy' });
  await expect(dialog.getByText(/engine unavailable/)).toBeVisible();
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await expect(
    page.getByRole('alert').filter({ hasText: 'linux: engine unavailable' }),
  ).toBeVisible();
  await expect(page.getByText('Deploy completed for Web stack.')).toHaveCount(
    0,
  );
});

test('imports a Compose file and its build context without duplicating compose.yaml', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Projects', exact: true }).click();
  await page.getByRole('button', { name: /New project/ }).click();
  const dialog = page.getByRole('dialog', { name: 'Create project' });
  const context = path.resolve('tests/fixtures/compose-context');
  await dialog.getByLabel('Project name').fill('context-test');
  await dialog
    .getByLabel('Or select a YAML file')
    .setInputFiles(path.join(context, 'compose.yaml'));
  await dialog
    .getByLabel('Build context directory (optional)')
    .setInputFiles(context);
  await dialog.getByRole('button', { name: 'Create project' }).click();
  await expect(
    page.getByRole('heading', { name: 'context-test' }),
  ).toBeVisible();
  const imported = await page.evaluate(() => {
    const calls = (
      window as unknown as {
        __ucdTestRequests: {
          method: string;
          params: { files: { path: string; content: string }[] };
        }[];
      }
    ).__ucdTestRequests;
    return calls.find((call) => call.method === 'project.import')?.params.files;
  });
  expect(imported).toEqual([
    {
      path: 'Dockerfile',
      content: Buffer.from('FROM scratch\n').toString('base64'),
    },
  ]);
});
