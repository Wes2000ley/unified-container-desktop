import { test, expect, type Page } from '@playwright/test';
import path from 'node:path';

type Request = {
  method: string;
  engine: string;
  params: Record<string, unknown>;
};

async function requests(page: Page, method: string): Promise<Request[]> {
  return page.evaluate(
    (name) =>
      (
        (window as unknown as { __ucdTestRequests: Request[] })
          .__ucdTestRequests || []
      ).filter((request) => request.method === name),
    method,
  );
}

test('routes image pull, row push, and build through their selected engine dialogs', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Images', exact: true }).click();

  await page.getByRole('button', { name: 'Pull image', exact: true }).click();
  let dialog = page.getByRole('dialog', { name: 'Pull image' });
  // The wrapping label includes its option text; exact getByLabel('Engine') misses it.
  const pullEngine = dialog.getByRole('combobox', { name: 'Engine' });
  await expect(pullEngine).toHaveCount(1);
  await pullEngine.selectOption('windows');
  await dialog
    .getByLabel('Image reference', { exact: true })
    .fill('example/pulled:v1');
  await dialog.getByRole('button', { name: 'Pull image', exact: true }).click();
  await expect(
    page.getByText('pull completed.', { exact: true }),
  ).toBeVisible();
  await expect(dialog).toHaveCount(0);
  expect(await requests(page, 'image.pull')).toContainEqual({
    method: 'image.pull',
    engine: 'windows',
    params: { reference: 'example/pulled:v1' },
  });

  const row = page.getByRole('row').filter({ hasText: 'example/web:latest' });
  await expect(row).toContainText('windows');
  await expect(
    page.getByRole('button', { name: 'Push image', exact: true }),
  ).toHaveCount(0);
  await row.getByRole('button', { name: 'Push', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Push image' });
  await expect(dialog.getByRole('combobox', { name: 'Engine' })).toHaveValue(
    'windows',
  );
  await expect(
    dialog.getByLabel('Image reference', { exact: true }),
  ).toHaveValue('example/web:latest');
  await dialog.getByRole('button', { name: 'Push image', exact: true }).click();
  await expect(
    dialog.getByRole('status').getByText('push completed.', { exact: true }),
  ).toBeVisible();
  expect(await requests(page, 'image.push')).toContainEqual({
    method: 'image.push',
    engine: 'windows',
    params: {
      reference: 'example/web:latest',
      operationId: expect.any(String),
    },
  });
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await expect(dialog).toHaveCount(0);

  await page.getByRole('button', { name: 'Build image', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Build image' });
  await dialog
    .getByRole('combobox', { name: 'Engine' })
    .selectOption('windows');
  await dialog
    .getByLabel('Build context folder', { exact: false })
    .setInputFiles(path.resolve('tests/fixtures/compose-context'));
  await dialog.getByLabel('Tags', { exact: false }).fill('example/built:v1');
  await dialog
    .getByRole('button', { name: 'Build image', exact: true })
    .click();
  await expect(
    dialog.getByRole('status').getByText('build completed.', { exact: true }),
  ).toBeVisible();

  const builds = (await requests(page, 'transfer.open')).filter(
    (request) => request.params.operation === 'image.build',
  );
  expect(builds).toHaveLength(1);
  expect(builds[0]).toMatchObject({
    engine: 'windows',
    params: {
      dockerfile: 'FROM scratch\n',
      dockerfilePath: 'Dockerfile',
      tags: ['example/built:v1'],
    },
  });
  expect(await requests(page, 'transfer.write')).not.toHaveLength(0);
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await expect(dialog).toHaveCount(0);
});
