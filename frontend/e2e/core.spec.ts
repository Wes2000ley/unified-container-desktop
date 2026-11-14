import { test, expect, type Page } from '@playwright/test';
import { readFile } from 'node:fs/promises';
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

async function transfers(page: Page, operation: string): Promise<Request[]> {
  return (await requests(page, 'transfer.open')).filter(
    (request) => request.params.operation === operation,
  );
}

function tarEntries(archive: Buffer): Record<string, string> {
  const entries: Record<string, string> = {};
  for (let offset = 0; offset + 512 <= archive.length;) {
    const name = archive
      .subarray(offset, offset + 100)
      .toString()
      .split('\0')[0];
    if (!name) break;
    const size = Number.parseInt(
      archive
        .subarray(offset + 124, offset + 136)
        .toString()
        .split('\0')[0] || '0',
      8,
    );
    entries[name] = archive
      .subarray(offset + 512, offset + 512 + size)
      .toString();
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  return entries;
}

async function openContainer(page: Page) {
  await page.goto('/');
  await page.getByRole('button', { name: 'Containers', exact: true }).click();
  await page.getByRole('button', { name: 'web-service', exact: true }).click();
}

test('follows a burst of logs in one session and closes it on navigation', async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await openContainer(page);
  await expect(page.getByText('Application started')).toBeVisible();
  await page.getByLabel('Follow').check();
  await expect(
    page.locator('pre').filter({ hasText: 'first line' }),
  ).toHaveText('first line\nburst 1\nburst 2\nburst 3\n');
  await expect
    .poll(async () => (await requests(page, 'container.logs.read')).length)
    .toBeGreaterThanOrEqual(4);
  const opened = await requests(page, 'container.logs.open');
  const reads = await requests(page, 'container.logs.read');
  expect(opened).toHaveLength(1);
  expect(new Set(reads.map((read) => read.params.sessionID)).size).toBe(1);
  const sessionID = reads[0]!.params.sessionID;
  await page.getByRole('button', { name: 'Stats', exact: true }).click();
  await expect(page.getByText('Live statistics')).toBeVisible();
  await expect(page.getByText('1.2%')).toBeVisible();
  await expect
    .poll(async () =>
      (await requests(page, 'container.logs.close')).map(
        (request) => request.params.sessionID,
      ),
    )
    .toContain(sessionID);
  expect(pageErrors).toEqual([]);
  await page.screenshot({
    path: 'test-results/container-stats.png',
    fullPage: true,
  });
});

test('sends real xterm keyboard input and closes the terminal session', async ({
  page,
}) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await openContainer(page);
  await page.getByRole('button', { name: 'Exec', exact: true }).click();
  await page.getByLabel('User', { exact: true }).fill('operator');
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(
    page.getByRole('status').filter({ hasText: 'Connected' }),
  ).toBeVisible();
  await page.locator('.xterm-helper-textarea').focus();
  await page.keyboard.type('whoami');
  await page.keyboard.press('Enter');
  await expect
    .poll(async () =>
      (await requests(page, 'container.exec.write'))
        .map((request) =>
          Buffer.from(String(request.params.data), 'base64').toString(),
        )
        .join(''),
    )
    .toBe('whoami\r');
  const [opened] = await requests(page, 'container.exec.open');
  expect(opened?.engine).toBe('windows');
  expect(opened?.params).toMatchObject({
    id: 'abc123',
    command: ['cmd.exe'],
    user: 'operator',
  });
  const [write] = await requests(page, 'container.exec.write');
  await page.getByRole('button', { name: 'Disconnect', exact: true }).click();
  await expect(
    page.getByRole('status').filter({ hasText: 'Disconnected' }),
  ).toBeVisible();
  expect(
    (await requests(page, 'container.exec.close')).at(-1)?.params.sessionID,
  ).toBe(write?.params.sessionID);
  expect(pageErrors).toEqual([]);
});

test('prepares Windows file access only after confirmation', async ({
  page,
}) => {
  await page.addInitScript(() => {
    (window as unknown as { __ucdTestOptions: unknown }).__ucdTestOptions = {
      unpreparedWindowsFiles: true,
    };
  });
  await openContainer(page);
  await page.getByRole('button', { name: 'Files', exact: true }).click();
  const error = page.getByText(/use Prepare Files to install it/);
  const file = page.getByRole('button', { name: 'config.txt', exact: true });
  await expect(error).toBeVisible();
  await expect(file).toHaveCount(0);
  const initialLists = await requests(page, 'container.files.list');
  expect(initialLists).toHaveLength(1);
  expect(initialLists[0]).toMatchObject({
    engine: 'windows',
    params: { id: 'abc123', path: 'C:\\' },
  });

  await page.getByRole('button', { name: 'Prepare file access' }).click();
  let dialog = page.getByRole('dialog', { name: 'Prepare file access?' });
  await expect(dialog.getByText(/briefly stopped and restarted/)).toBeVisible();
  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(error).toBeVisible();
  expect(await requests(page, 'container.files.prepare')).toHaveLength(0);
  expect(await requests(page, 'container.files.list')).toHaveLength(1);

  await page.getByRole('button', { name: 'Prepare file access' }).click();
  dialog = page.getByRole('dialog', { name: 'Prepare file access?' });
  await dialog.getByRole('button', { name: 'Prepare file access' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByText('File access is prepared and the container was restarted.'),
  ).toBeVisible();
  await expect(file).toBeVisible();
  expect(await requests(page, 'container.files.prepare')).toEqual([
    {
      method: 'container.files.prepare',
      engine: 'windows',
      params: { id: 'abc123', confirmed: true },
    },
  ]);
  expect((await requests(page, 'container.files.list')).at(-1)).toMatchObject({
    engine: 'windows',
    params: { id: 'abc123', path: 'C:\\' },
  });
  await file.click();
  await expect(page.getByLabel('File contents')).toHaveValue('old config\n');
  await page.getByRole('button', { name: 'Stats', exact: true }).click();
  await page.getByRole('button', { name: 'Files', exact: true }).click();
  await expect(file).toBeVisible();
  expect(await requests(page, 'container.files.prepare')).toHaveLength(1);
  expect(await requests(page, 'container.action')).toHaveLength(0);
});

test('confirms file writes and deletion, retains failed edits, and transfers bytes', async ({
  page,
}) => {
  await page.addInitScript(() => {
    (window as unknown as { __ucdTestOptions: unknown }).__ucdTestOptions = {
      failOnce: { 'container.files.write': 'disk full' },
    };
  });
  await openContainer(page);
  await page.getByRole('button', { name: 'Files', exact: true }).click();
  await page.getByRole('button', { name: 'config.txt', exact: true }).click();
  await page.getByLabel('File contents').fill('updated config\n');
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  let confirm = page.getByRole('dialog', { name: 'Overwrite file?' });
  await expect(confirm).toBeVisible();
  expect(await requests(page, 'container.files.write')).toHaveLength(0);
  await confirm.getByRole('button', { name: 'Overwrite file' }).click();
  await expect(confirm.getByText('disk full')).toBeVisible();
  await expect(page.getByLabel('File contents')).toHaveValue(
    'updated config\n',
  );
  await confirm.getByRole('button', { name: 'Overwrite file' }).click();
  await expect(confirm).toHaveCount(0);
  expect(
    (await requests(page, 'container.files.write')).at(-1)?.params,
  ).toMatchObject({
    id: 'abc123',
    path: 'C:/config.txt',
    data: Buffer.from('updated config\n').toString('base64'),
  });

  await page.getByLabel('Upload file').setInputFiles({
    name: 'config.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('uploaded\n'),
  });
  confirm = page.getByRole('dialog', { name: 'Overwrite file?' });
  await expect(confirm).toBeVisible();
  expect(await transfers(page, 'container.files.upload')).toHaveLength(0);
  await confirm.getByRole('button', { name: 'Overwrite file' }).click();
  await expect(confirm).toHaveCount(0);
  expect((await transfers(page, 'container.files.upload'))[0]).toMatchObject({
    engine: 'windows',
    params: { id: 'abc123', path: 'C:\\config.txt', size: 9 },
  });
  expect((await requests(page, 'transfer.write')).at(-1)?.params.data).toBe(
    Buffer.from('uploaded\n').toString('base64'),
  );
  expect(await requests(page, 'transfer.finish')).toHaveLength(1);
  expect(await requests(page, 'transfer.close')).toHaveLength(1);

  const row = page.getByRole('row').filter({ hasText: 'config.txt' });
  const downloadPromise = page.waitForEvent('download');
  await row.getByRole('button', { name: 'Download' }).click();
  const download = await downloadPromise;
  expect(download.suggestedFilename()).toBe('config.txt');
  expect(await readFile(await download.path(), 'utf8')).toBe('uploaded\n');
  await row.getByRole('button', { name: 'Delete' }).click();
  confirm = page.getByRole('dialog', { name: 'Delete config.txt?' });
  expect(await requests(page, 'container.files.remove')).toHaveLength(0);
  await confirm.getByRole('button', { name: 'Cancel' }).click();
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: 'Delete' }).click();
  await page
    .getByRole('dialog', { name: 'Delete config.txt?' })
    .getByRole('button', { name: 'Delete', exact: true })
    .click();
  await expect(row).toHaveCount(0);
  expect(
    (await requests(page, 'container.files.remove'))[0]?.params,
  ).toMatchObject({ id: 'abc123', path: 'C:/config.txt', recursive: false });
});

test('creates, renames and confirms removal of a container', async ({
  page,
}) => {
  await page.addInitScript(() => {
    (window as unknown as { __ucdTestOptions: unknown }).__ucdTestOptions = {
      linuxOnline: true,
    };
  });
  await page.goto('/');
  await page.getByRole('button', { name: 'Containers', exact: true }).click();
  const original = page.getByRole('row').filter({ hasText: 'web-service' });
  await original.getByRole('button', { name: 'Tools' }).click();
  const exportDialog = page.getByRole('dialog', {
    name: 'Container tools · web-service',
  });
  await exportDialog.getByLabel('Operation').selectOption('export');
  await expect(
    exportDialog.getByText(
      'Windows Docker does not support container filesystem export. Use Save in Images to download a Windows image.',
    ),
  ).toBeVisible();
  await expect(
    exportDialog.getByRole('button', { name: 'Download export' }),
  ).toBeDisabled();
  expect(await transfers(page, 'container.export')).toHaveLength(0);
  await exportDialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();

  const linuxRow = page.getByRole('row').filter({ hasText: 'api-service' });
  await linuxRow.getByRole('button', { name: 'Tools' }).click();
  const linuxExport = page.getByRole('dialog', {
    name: 'Container tools · api-service',
  });
  await linuxExport.getByLabel('Operation').selectOption('export');
  await expect(
    linuxExport.getByText(
      'Export downloads the container filesystem; mounted volume data is excluded.',
    ),
  ).toBeVisible();
  const exportEvent = page.waitForEvent('download');
  await linuxExport.getByRole('button', { name: 'Download export' }).click();
  const download = await exportEvent;
  expect(download.suggestedFilename()).toBe('api-service.tar');
  expect(tarEntries(await readFile(await download.path()))).toEqual({
    'config.txt': 'old config\n',
  });
  expect((await transfers(page, 'container.export'))[0]).toMatchObject({
    engine: 'linux',
    params: { id: 'linux123' },
  });
  await linuxExport
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await page.getByRole('button', { name: /Create container/ }).click();
  const create = page.getByRole('dialog', { name: 'Create container' });
  await create.getByLabel('Image', { exact: true }).fill('example/api:v1');
  await create.getByLabel('Name', { exact: false }).fill('api-worker');
  await create.getByLabel('Engine').selectOption('windows');
  await create
    .getByLabel('Published ports', { exact: false })
    .fill('8081:80/tcp');
  await create.getByLabel('Environment', { exact: false }).fill('MODE=test');
  await create.getByRole('button', { name: 'Create and run' }).click();
  await expect(create).toHaveCount(0);
  const [run] = await requests(page, 'container.run');
  expect(run).toMatchObject({
    engine: 'windows',
    params: {
      name: 'api-worker',
      config: {
        Image: 'example/api:v1',
        Env: ['MODE=test'],
        ExposedPorts: { '80/tcp': {} },
      },
      hostConfig: { PortBindings: { '80/tcp': [{ HostPort: '8081' }] } },
    },
  });
  let row = page.getByRole('row').filter({ hasText: 'api-worker' });
  await row.getByRole('button', { name: 'Tools' }).click();
  const tools = page.getByRole('dialog', {
    name: 'Container tools · api-worker',
  });
  await tools.getByLabel('Operation').selectOption('rename');
  await tools.getByLabel('New name').fill('api-renamed');
  await tools.getByRole('button', { name: 'Apply' }).click();
  await expect(tools.getByText('rename completed.')).toBeVisible();
  await tools
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  row = page.getByRole('row').filter({ hasText: 'api-renamed' });
  await row.getByRole('button', { name: 'Remove' }).click();
  const confirmation = page.getByRole('dialog', {
    name: 'Remove 1 container(s)?',
  });
  await expect(confirmation).toBeVisible();
  expect(
    (await requests(page, 'container.action')).filter(
      (request) => request.params.action === 'remove',
    ),
  ).toHaveLength(0);
  await confirmation.getByRole('button', { name: 'Remove containers' }).click();
  await expect(row).toHaveCount(0);
});

test('follows project logs and reports the unavailable engine independently', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Projects', exact: true }).click();
  await page.getByRole('button', { name: 'Logs', exact: true }).click();
  const dialog = page.getByRole('dialog', { name: 'Web stack · Logs' });
  await expect(dialog.getByLabel('windows project logs')).toContainText(
    'web | ready\nweb | processing',
  );
  await expect(dialog.getByText('engine unavailable')).toBeVisible();
  await dialog.getByLabel('Search project logs').fill('processing');
  await expect(dialog.getByLabel('windows project logs')).toHaveText(
    'web | processing',
  );
  const download = page.waitForEvent('download');
  await dialog.getByRole('button', { name: 'Export logs' }).click();
  expect(await readFile(await (await download).path(), 'utf8')).toContain(
    'web | ready\nweb | processing',
  );
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await expect
    .poll(async () => (await requests(page, 'project.logs.close')).length)
    .toBe(1);
});

test('streams a container upload larger than 8 MiB in bounded chunks', async ({
  page,
}) => {
  await openContainer(page);
  await page.getByRole('button', { name: 'Files', exact: true }).click();
  const contents = Buffer.alloc(8 * 1024 * 1024 + 1, 0x5a);
  contents[contents.length - 1] = 0x7f;
  await page.getByLabel('Upload file').setInputFiles({
    name: 'large.bin',
    mimeType: 'application/octet-stream',
    buffer: contents,
  });
  const dialog = page.getByRole('dialog', { name: 'Upload file?' });
  expect(await transfers(page, 'container.files.upload')).toHaveLength(0);
  await dialog.getByRole('button', { name: 'Upload file' }).click();
  await expect(dialog).toHaveCount(0);
  const summary = await page.evaluate(() => {
    const calls = (window as unknown as { __ucdTestRequests: Request[] })
      .__ucdTestRequests;
    const writes = calls.filter(
      (request) => request.method === 'transfer.write',
    );
    const chunks = writes.map((request) => atob(String(request.params.data)));
    return {
      count: chunks.length,
      maximum: Math.max(...chunks.map((chunk) => chunk.length)),
      total: chunks.reduce((sum, chunk) => sum + chunk.length, 0),
      lastByte: chunks.at(-1)?.charCodeAt(0),
      writes: writes.map((request) => request.params.sessionID),
      finish: calls
        .filter((request) => request.method === 'transfer.finish')
        .map((request) => request.params.sessionID),
      close: calls
        .filter((request) => request.method === 'transfer.close')
        .map((request) => request.params.sessionID),
    };
  });
  expect(summary).toMatchObject({
    count: 33,
    maximum: 256 * 1024,
    total: contents.length,
    lastByte: 0x7f,
  });
  expect(new Set(summary.writes).size).toBe(1);
  expect(summary.finish).toEqual([summary.writes[0]]);
  expect(summary.close).toEqual([summary.writes[0]]);
  await expect(
    page.getByRole('row').filter({ hasText: 'large.bin' }),
  ).toBeVisible();
});

test('inspects, tags, pushes, builds and transfers images through the GUI', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Images', exact: true }).click();
  const row = page.getByRole('row').filter({ hasText: 'example/web:latest' });
  await row.getByRole('button', { name: 'Details' }).click();
  const details = page.getByRole('dialog', {
    name: 'Image details and layers · example/web:latest',
  });
  await expect(details.getByText(/COPY app \/app/)).toBeVisible();
  expect((await requests(page, 'image.inspect'))[0]?.params).toEqual({
    reference: 'example/web:latest',
  });
  expect((await requests(page, 'image.history'))[0]?.params).toEqual({
    reference: 'example/web:latest',
  });
  await details
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();

  await row.getByRole('button', { name: 'Tag', exact: true }).click();
  let dialog = page.getByRole('dialog', { name: 'Tag image' });
  await dialog.getByLabel('Target reference').fill('example/web:stable');
  await dialog.getByRole('button', { name: 'Tag image' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(row).toContainText('example/web:stable');
  expect((await requests(page, 'image.tag'))[0]?.params).toEqual({
    reference: 'example/web:latest',
    target: 'example/web:stable',
  });

  await row.getByRole('button', { name: 'Push', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Push image' });
  await dialog.getByRole('button', { name: 'Push image' }).click();
  await expect(dialog.getByText('image.push completed')).toBeVisible();
  expect((await requests(page, 'image.push'))[0]?.params).toMatchObject({
    reference: 'example/web:latest',
    operationId: expect.any(String),
  });
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  const save = page.waitForEvent('download');
  await row.getByRole('button', { name: 'Save', exact: true }).click();
  const saved = await save;
  expect(saved.suggestedFilename()).toBe('images.tar');
  const savedArchive = await readFile(await saved.path());
  expect((await transfers(page, 'image.save'))[0]?.params).toMatchObject({
    references: ['example/web:latest'],
  });

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
    .getByLabel('Build arguments', { exact: false })
    .fill('MODE=test');
  await dialog.getByRole('button', { name: 'Build image' }).click();
  await expect(dialog.getByText('image.build completed')).toBeVisible();
  expect((await transfers(page, 'image.build'))[0]).toMatchObject({
    engine: 'windows',
    params: {
      dockerfile: 'FROM scratch\n',
      dockerfilePath: 'Dockerfile',
      tags: ['example/built:v1'],
      buildArgs: { MODE: 'test' },
    },
  });
  const context = Buffer.concat(
    (await requests(page, 'transfer.write')).map((request) =>
      Buffer.from(String(request.params.data), 'base64'),
    ),
  );
  expect(tarEntries(context)).toEqual({
    Dockerfile: 'FROM scratch\n',
    'compose.yaml': await readFile(
      path.resolve('tests/fixtures/compose-context/compose.yaml'),
      'utf8',
    ),
  });
  await page.screenshot({
    path: 'test-results/image-build.png',
    fullPage: true,
  });
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await expect(
    page.getByRole('row').filter({ hasText: 'example/built:v1' }),
  ).toBeVisible();
  await row.getByRole('button', { name: 'Remove', exact: true }).click();
  const confirmation = page.getByRole('dialog', { name: 'Remove image?' });
  expect(await requests(page, 'image.remove')).toHaveLength(0);
  await confirmation.getByRole('button', { name: 'Remove image' }).click();
  await expect(row).toHaveCount(0);
  await page.getByRole('button', { name: 'Load image', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Load image' });
  await dialog
    .getByRole('combobox', { name: 'Engine' })
    .selectOption('windows');
  await dialog.getByLabel('Archive file', { exact: false }).setInputFiles({
    name: 'images.tar',
    mimeType: 'application/x-tar',
    buffer: savedArchive,
  });
  await dialog.getByRole('button', { name: 'Load image' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(row).toBeVisible();
  expect((await transfers(page, 'image.load'))[0]?.params).toMatchObject({
    size: savedArchive.length,
  });
});

test('retains a rejected image archive and imports a valid filesystem tar', async ({
  page,
}) => {
  await page.addInitScript(() => {
    (window as unknown as { __ucdTestOptions: unknown }).__ucdTestOptions = {
      failOnce: { 'image.load': 'invalid image archive' },
    };
  });
  await page.goto('/');
  await page.getByRole('button', { name: 'Volumes', exact: true }).click();
  await page.getByRole('button', { name: 'app-data', exact: true }).click();
  const backupEvent = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Back up' }).click();
  const archive = await readFile(await (await backupEvent).path());

  await page.getByRole('button', { name: 'Images', exact: true }).click();
  await page.getByRole('button', { name: 'Load image', exact: true }).click();
  let dialog = page.getByRole('dialog', { name: 'Load image' });
  await dialog
    .getByRole('combobox', { name: 'Engine' })
    .selectOption('windows');
  const invalid = Buffer.from('not a Docker image archive');
  await dialog.getByLabel('Archive file', { exact: false }).setInputFiles({
    name: 'bad.tar',
    mimeType: 'application/x-tar',
    buffer: invalid,
  });
  await dialog.getByRole('button', { name: 'Load image' }).click();
  await expect(dialog.getByText('invalid image archive')).toBeVisible();
  await expect(dialog.getByLabel('Archive file', { exact: false })).toHaveValue(
    /bad\.tar$/,
  );
  expect((await transfers(page, 'image.load'))[0]).toMatchObject({
    engine: 'windows',
    params: { size: invalid.length },
  });
  expect((await requests(page, 'transfer.write')).at(-1)?.params.data).toBe(
    invalid.toString('base64'),
  );
  expect(await requests(page, 'transfer.close')).toHaveLength(1);
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();

  await page.getByRole('button', { name: 'Import image', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Import image' });
  await dialog
    .getByRole('combobox', { name: 'Engine' })
    .selectOption('windows');
  await dialog.getByLabel('Archive file', { exact: false }).setInputFiles({
    name: 'filesystem.tar',
    mimeType: 'application/x-tar',
    buffer: archive,
  });
  await dialog.getByLabel('Image reference').fill('example/imported:v1');
  await dialog.getByRole('button', { name: 'Import image' }).click();
  await expect(dialog).toHaveCount(0);
  await expect(
    page.getByRole('row').filter({ hasText: 'example/imported:v1' }),
  ).toBeVisible();
  expect((await transfers(page, 'image.import'))[0]).toMatchObject({
    engine: 'windows',
    params: {
      reference: 'example/imported:v1',
      size: archive.length,
    },
  });
  expect((await requests(page, 'transfer.write')).at(-1)?.params.data).toBe(
    archive.toString('base64'),
  );
});

test('manages volume data, confirms replacement and destructive actions, and transfers a backup', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Volumes', exact: true }).click();
  await page.getByRole('button', { name: /Create volume/ }).click();
  let dialog = page.getByRole('dialog', { name: 'Create volume' });
  await dialog
    .getByRole('combobox', { name: 'Engine' })
    .selectOption('windows');
  await dialog.getByLabel('Name', { exact: true }).fill('new-data');
  await dialog.getByRole('button', { name: 'Create volume' }).click();
  await expect(page.getByText('new-data', { exact: true })).toBeVisible();
  expect((await requests(page, 'volume.create'))[0]).toMatchObject({
    engine: 'windows',
    params: { name: 'new-data' },
  });
  await page.getByRole('button', { name: 'app-data', exact: true }).click();
  await expect(page.getByText('No attached containers.')).toBeVisible();
  await expect(page.getByText(/notes\.txt · file/)).toBeVisible();

  const note = page
    .locator('.simple-list > div')
    .filter({ hasText: 'notes.txt' });
  await note.getByRole('button', { name: 'Edit text' }).click();
  dialog = page.getByRole('dialog', { name: 'Edit notes.txt' });
  await dialog.getByLabel('File contents').fill('changed notes\n');
  await dialog.getByRole('button', { name: 'Save file' }).click();
  await page
    .getByRole('dialog', { name: 'Overwrite file?' })
    .getByRole('button', { name: 'Overwrite file' })
    .click();
  await expect(dialog).toHaveCount(0);
  expect((await requests(page, 'volume.files.write'))[0]?.params).toMatchObject(
    {
      name: 'app-data',
      path: 'notes.txt',
      data: Buffer.from('changed notes\n').toString('base64'),
    },
  );

  await page.getByLabel('Upload file').setInputFiles({
    name: 'notes.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('replacement\n'),
  });
  page.once('dialog', (confirmation) => void confirmation.dismiss());
  await page.getByRole('button', { name: 'Upload', exact: true }).click();
  expect(await transfers(page, 'volume.files.upload')).toHaveLength(0);
  page.once('dialog', (confirmation) => void confirmation.accept());
  await page.getByRole('button', { name: 'Upload', exact: true }).click();
  await expect(page.getByText('File uploaded.')).toBeVisible();
  expect(
    (await transfers(page, 'volume.files.upload'))[0]?.params,
  ).toMatchObject({
    name: 'app-data',
    path: 'notes.txt',
    size: Buffer.byteLength('replacement\n'),
  });
  expect((await requests(page, 'transfer.write')).at(-1)?.params.data).toBe(
    Buffer.from('replacement\n').toString('base64'),
  );

  const fileDownload = page.waitForEvent('download');
  await note.getByRole('button', { name: 'Download' }).click();
  expect(await readFile(await (await fileDownload).path(), 'utf8')).toBe(
    'replacement\n',
  );
  await page.getByLabel('New directory').fill('scratch');
  await page.getByRole('button', { name: 'Create directory' }).click();
  const directory = page
    .locator('.simple-list > div')
    .filter({ hasText: 'scratch' });
  await expect(directory).toBeVisible();
  await directory.getByRole('button', { name: 'Delete' }).click();
  dialog = page.getByRole('dialog', { name: 'Delete scratch?' });
  expect(await requests(page, 'volume.files.remove')).toHaveLength(0);
  await dialog.getByRole('button', { name: 'Delete entry' }).click();
  await expect(directory).toHaveCount(0);
  expect(
    (await requests(page, 'volume.files.remove'))[0]?.params,
  ).toMatchObject({ name: 'app-data', path: 'scratch', recursive: true });

  const backupEvent = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Back up' }).click();
  const backup = await backupEvent;
  expect(backup.suggestedFilename()).toBe('app-data.tar');
  const archive = await readFile(await backup.path());
  expect(archive.length).toBe(2048);
  await page.getByRole('button', { name: 'Clone', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Clone app-data' });
  await dialog.getByLabel('New volume name').fill('app-copy');
  await dialog.getByRole('button', { name: 'Clone', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect((await requests(page, 'volume.clone'))[0]?.params).toEqual({
    name: 'app-data',
    target: 'app-copy',
  });

  await page.getByLabel('Restore backup', { exact: false }).setInputFiles({
    name: 'app-data.tar',
    mimeType: 'application/x-tar',
    buffer: archive,
  });
  await page
    .getByRole('button', { name: 'Restore backup', exact: true })
    .click();
  dialog = page.getByRole('dialog', { name: 'Restore backup?' });
  expect(await transfers(page, 'volume.restore')).toHaveLength(0);
  await dialog.getByRole('button', { name: /Restore/ }).click();
  await expect(dialog).toHaveCount(0);
  expect((await transfers(page, 'volume.restore'))[0]?.params).toMatchObject({
    name: 'app-data',
    confirmed: true,
    size: archive.length,
  });
  expect((await requests(page, 'transfer.write')).at(-1)?.params.data).toBe(
    archive.toString('base64'),
  );
  await page.getByRole('button', { name: 'Empty volume', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Empty volume?' });
  expect(await requests(page, 'volume.empty')).toHaveLength(0);
  await dialog.getByRole('button', { name: /Empty/ }).click();
  await expect(page.getByText('Directory is empty')).toBeVisible();
  expect((await requests(page, 'volume.empty'))[0]?.params).toEqual({
    name: 'app-data',
    confirmed: true,
  });
});

test('handles cancellation of the native save dialog', async ({ page }) => {
  await page.addInitScript(() => {
    (window as unknown as { __ucdTestOptions: unknown }).__ucdTestOptions = {
      cancelDownload: true,
    };
  });
  let downloads = 0;
  page.on('download', () => {
    downloads++;
  });
  await page.goto('/');
  await page.getByRole('button', { name: 'Volumes', exact: true }).click();
  await page.getByRole('button', { name: 'app-data', exact: true }).click();
  await page.getByRole('button', { name: 'Back up' }).click();
  await expect
    .poll(async () => (await requests(page, 'bridge.download')).length)
    .toBe(1);
  expect((await requests(page, 'bridge.download'))[0]).toMatchObject({
    engine: 'windows',
    params: { name: 'app-data.tar' },
  });
  await expect(page.getByRole('button', { name: 'Back up' })).toBeEnabled();
  await expect(page.getByRole('alert')).toHaveCount(0);
  expect(downloads).toBe(0);
});

test('manages networks, registry sign-ins, and previewed storage cleanup', async ({
  page,
}) => {
  await page.goto('/');
  await page.getByRole('button', { name: 'Networks', exact: true }).click();
  const row = page.getByRole('row').filter({ hasText: 'app-net' });
  await row.getByRole('button', { name: 'Inspect' }).click();
  let dialog = page.getByRole('dialog', { name: 'Network · app-net' });
  await expect(dialog.getByText(/"Name": "app-net"/)).toBeVisible();
  await dialog
    .getByRole('button', { name: 'Close', exact: true })
    .last()
    .click();
  await row.getByRole('button', { name: 'Connect', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Connect to app-net' });
  await dialog.getByLabel('Container name or ID').fill('web-service');
  await dialog.getByLabel('Aliases', { exact: false }).fill('web\nfrontend');
  await dialog.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect((await requests(page, 'network.connect'))[0]).toMatchObject({
    engine: 'windows',
    params: {
      id: 'net1',
      container: 'web-service',
      aliases: ['web', 'frontend'],
    },
  });
  await page.getByRole('button', { name: /Create network/ }).click();
  dialog = page.getByRole('dialog', { name: 'Create network' });
  await dialog
    .getByRole('combobox', { name: 'Engine' })
    .selectOption('windows');
  await dialog.getByLabel('Name', { exact: true }).fill('test-net');
  await dialog.getByLabel('Attachable').check();
  await dialog.getByRole('button', { name: 'Create network' }).click();
  await expect(page.getByText('test-net', { exact: true })).toBeVisible();
  expect((await requests(page, 'network.create'))[0]?.params).toMatchObject({
    name: 'test-net',
    attachable: true,
  });
  const created = page.getByRole('row').filter({ hasText: 'test-net' });
  await created.getByRole('button', { name: 'Remove' }).click();
  dialog = page.getByRole('dialog', { name: 'Remove network test-net?' });
  expect(await requests(page, 'network.remove')).toHaveLength(0);
  await dialog.getByRole('button', { name: 'Remove network' }).click();
  await expect(created).toHaveCount(0);

  await page.getByRole('button', { name: 'Registries', exact: true }).click();
  await expect(page.getByLabel('Engine')).toHaveCount(0);
  await page.getByLabel('Registry server').fill('registry.example.test');
  await page.getByLabel('Username').fill('fixture-user');
  await page.getByLabel('Password or token').fill('fixture-token');
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  await expect
    .poll(async () => (await requests(page, 'registry.login')).length)
    .toBe(1);
  expect((await requests(page, 'registry.login'))[0]?.engine).toBe('');
  await expect(
    page.getByText('registry.example.test', { exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel('Password or token')).toHaveValue('');
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  dialog = page.getByRole('dialog', {
    name: 'Sign out of registry.example.test?',
  });
  expect(await requests(page, 'registry.logout')).toHaveLength(0);
  await dialog.getByRole('button', { name: 'Sign out' }).click();
  await expect
    .poll(async () => (await requests(page, 'registry.logout')).length)
    .toBe(1);
  expect((await requests(page, 'registry.logout'))[0]?.engine).toBe('');
  await expect(page.getByText('No registry sign-ins')).toBeVisible();

  await page.getByRole('button', { name: 'Storage', exact: true }).click();
  await page
    .getByRole('combobox', { name: 'Engine', exact: true })
    .selectOption('windows');
  await page.getByLabel('Resource scope').selectOption('volumes');
  await page.getByLabel('Include all eligible resources').check();
  await page.getByRole('button', { name: 'Preview cleanup' }).click();
  dialog = page.getByRole('dialog', { name: 'Prune volumes?' });
  await expect(dialog.getByText(/Estimated reclaimable/)).toBeVisible();
  expect(await requests(page, 'storage.prune')).toHaveLength(0);
  expect((await requests(page, 'storage.preview'))[0]).toMatchObject({
    engine: 'windows',
    params: { scope: 'volumes', all: true },
  });
  await dialog.getByRole('button', { name: 'Prune selected scope' }).click();
  await expect(
    page.getByRole('dialog', { name: 'Cleanup result' }),
  ).toBeVisible();
  expect((await requests(page, 'storage.prune'))[0]).toMatchObject({
    engine: 'windows',
    params: { scope: 'volumes', all: true, confirmed: true },
  });
});

test('viewer cannot invoke operator actions or load protected container data', async ({
  page,
}) => {
  await page.addInitScript(() => {
    (window as unknown as { __ucdTestOptions: unknown }).__ucdTestOptions = {
      role: 'viewer',
    };
  });
  await openContainer(page);
  await expect(page.getByText(/Read-only access/)).toBeVisible();
  await expect(
    page.getByRole('button', { name: 'Stop', exact: true }),
  ).toBeDisabled();
  await page.getByRole('button', { name: 'Exec', exact: true }).click();
  await expect(
    page.getByText(/Container Operators membership is required for exec/),
  ).toBeVisible();
  await page.getByRole('button', { name: 'Files', exact: true }).click();
  await expect(
    page.getByText(/Container Operators membership is required for files/),
  ).toBeVisible();
  expect(await requests(page, 'container.exec.open')).toHaveLength(0);
  expect(await requests(page, 'container.files.list')).toHaveLength(0);
  await page.getByRole('button', { name: 'Images', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Pull image' })).toBeDisabled();
  await page.getByRole('button', { name: 'Storage', exact: true }).click();
  await expect(
    page.getByRole('button', { name: 'Preview cleanup' }),
  ).toBeDisabled();
  expect(await requests(page, 'storage.prune')).toHaveLength(0);
});
