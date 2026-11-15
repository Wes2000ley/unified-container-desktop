import { beforeEach, expect, it, vi } from 'vitest';
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { unpackTar } from 'modern-tar';
import { Containers } from '../src/inventories';
import { CreateContainer } from '../src/resource-container-create';
import { Images } from '../src/resource-images';
import { Volumes } from '../src/resource-volumes';
import { Networks } from '../src/networks';
import { Registry } from '../src/registry';
import { Storage } from '../src/storage';
import { Settings } from '../src/settings';

const bridge = vi.hoisted(() => vi.fn());
vi.mock('@wailsio/runtime', () => ({ Call: { ByName: bridge } }));
vi.mock('../src/container-detail', () => ({ ContainerDetail: () => null }));
const calls = () =>
  bridge.mock.calls.map(([, method, engine, params]) => ({
    method,
    engine,
    params: JSON.parse(params as string) as Record<string, unknown>,
  }));

async function mockSettings(
  _binding: string,
  method: string,
  _engine: string,
  raw: string,
) {
  if (method === 'system.settings')
    return JSON.stringify({
      settings: {
        version: 1,
        dataRoot: 'C:\\ProgramData\\UnifiedContainerDesktop',
        windowsEndpoint: '',
        helperURL: '',
        caCert: '',
        clientCert: '',
        clientKey: '',
        wsl: {
          distro: 'UCD-Linux',
          serviceAccount: 'UcdService',
          cpus: 4,
          memoryMB: 4096,
          diskGB: 128,
          profileDirectory: 'C:\\ProgramData\\profile',
          installDirectory: 'C:\\ProgramData\\wsl',
        },
        projects: [],
        dockerProxy: {},
        containersProxy: {},
        network: {
          defaultNetworkingMode: 'ipv4only',
          dnsInhibition: 'auto',
          defaultPortBinding: 'local',
          hostNetworkingEnabled: false,
        },
        resourceSaver: { enabled: false, idleSeconds: 300 },
        registryMirrors: [],
      },
      principal: { sid: 'S-1', name: 'Administrator', role: 'administrator' },
    });
  if (method === 'wsl.status')
    return JSON.stringify({
      distro: 'UCD-Linux',
      running: true,
      cpus: 4,
      memoryMB: 4096,
      diskGB: 128,
      memoryUsedMB: 0,
      diskUsedBytes: 0,
      restartRequired: false,
    });
  if (method === 'engine.status')
    return JSON.stringify({
      id: 'linux',
      os: 'linux',
      online: true,
      containers: 0,
      running: 0,
      stopped: 0,
      cpus: 4,
      memoryBytes: 4096 * 1024 * 1024,
      resourceSaver: false,
      activeOperations: 0,
    });
  if (method === 'operator.list' || method === 'share.list')
    return JSON.stringify([]);
  if (method === 'system.configure') {
    const params = JSON.parse(raw) as {
      resourceSaver?: { enabled?: boolean };
    };
    return JSON.stringify({
      restartRequired: params.resourceSaver?.enabled !== true,
    });
  }
  return JSON.stringify({ restartRequired: true });
}

beforeEach(() => {
  bridge.mockReset();
});

it('retains the create form after a failure and sends Docker configuration on retry', async () => {
  let attempts = 0;
  bridge.mockImplementation(async () => {
    if (++attempts === 1) throw new Error('Engine unavailable');
    return JSON.stringify({ id: 'abc' });
  });
  const done = vi.fn();
  const user = userEvent.setup();
  render(<CreateContainer close={vi.fn()} onCreated={done} />);
  await user.type(screen.getByLabelText('Image'), 'example/app:1');
  await user.type(screen.getByLabelText(/Published ports/), '8080:80/tcp');
  await user.type(screen.getByLabelText(/Environment/), 'MODE=dev');
  await user.type(screen.getByLabelText('Memory limit (MiB)'), '256');
  await user.click(screen.getByRole('button', { name: 'Create and run' }));
  expect(await screen.findByText('Engine unavailable')).toBeInTheDocument();
  expect(screen.getByLabelText('Image')).toHaveValue('example/app:1');
  await user.click(screen.getByRole('button', { name: 'Create and run' }));
  await waitFor(() => expect(done).toHaveBeenCalledOnce());
  expect(calls().at(-1)).toMatchObject({
    method: 'container.run',
    engine: 'linux',
    params: {
      config: {
        Image: 'example/app:1',
        Env: ['MODE=dev'],
        ExposedPorts: { '80/tcp': {} },
      },
      hostConfig: {
        PortBindings: { '80/tcp': [{ HostPort: '8080' }] },
        Memory: 268435456,
      },
    },
  });
});

it('confirms bulk kill and keeps only failed containers selected for retry', async () => {
  const containers = ['one', 'two'].map((name) => ({
    id: name,
    name,
    engine: 'linux',
    image: 'example',
    state: 'running',
    status: 'running',
    ports: [],
    cpuPercent: 0,
    memoryBytes: 0,
    metricsAvailable: false,
  }));
  bridge.mockImplementation(
    async (_bridge, method: string, _engine: string, raw: string) => {
      const params = JSON.parse(raw) as { id?: string };
      if (method === 'container.list')
        return JSON.stringify({ items: containers, errors: [] });
      if (method === 'container.action' && params.id === 'two')
        throw new Error('cannot kill');
      return 'null';
    },
  );
  const user = userEvent.setup();
  render(<Containers revision={0} canOperate />);
  await screen.findByRole('button', { name: 'one' });
  await user.click(screen.getByRole('checkbox', { name: 'Select one' }));
  await user.click(screen.getByRole('checkbox', { name: 'Select two' }));
  await user.click(screen.getByRole('button', { name: 'Kill' }));
  expect(
    calls().filter((call) => call.method === 'container.action'),
  ).toHaveLength(0);
  const dialog = screen.getByRole('dialog', { name: 'Kill 2 container(s)?' });
  await user.click(
    within(dialog).getByRole('button', { name: 'Kill containers' }),
  );
  expect(
    await screen.findByText(/two \(linux\): cannot kill/),
  ).toBeInTheDocument();
  expect(
    screen.getByRole('checkbox', { name: 'Select one' }),
  ).not.toBeChecked();
  expect(screen.getByRole('checkbox', { name: 'Select two' })).toBeChecked();
});

it('shows volume attachments to a viewer without invoking the file helper', async () => {
  const volume = {
    name: 'data',
    engine: 'linux',
    driver: 'local',
    mountpoint: '/data',
    size: 0,
    usageCount: 0,
  };
  bridge.mockImplementation(async (_bridge, method: string) =>
    JSON.stringify(
      method === 'volume.list'
        ? { items: [volume], errors: [] }
        : { volume, containers: [] },
    ),
  );
  const user = userEvent.setup();
  render(<Volumes revision={0} canOperate={false} />);
  await user.click(await screen.findByRole('button', { name: 'data' }));
  expect(
    await screen.findByText('No attached containers.'),
  ).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Back up' })).toBeDisabled();
  expect(calls().some((call) => call.method.startsWith('volume.files.'))).toBe(
    false,
  );
});

it('confirms a volume file overwrite and retains the edited text after failure', async () => {
  const volume = {
    name: 'data',
    engine: 'linux',
    driver: 'local',
    mountpoint: '/data',
    size: 0,
    usageCount: 0,
  };
  let writes = 0;
  bridge.mockImplementation(async (_bridge, method: string) => {
    if (method === 'volume.list')
      return JSON.stringify({ items: [volume], errors: [] });
    if (method === 'volume.inspect')
      return JSON.stringify({ volume, containers: [] });
    if (method === 'volume.files.list')
      return JSON.stringify({
        entries: [
          {
            name: 'config.txt',
            path: 'config.txt',
            type: 'file',
            size: 3,
            modified: '',
          },
        ],
      });
    if (method === 'volume.files.read')
      return JSON.stringify({ data: btoa('old') });
    if (method === 'volume.files.write' && ++writes === 1)
      throw new Error('disk full');
    return 'null';
  });
  const user = userEvent.setup();
  render(<Volumes revision={0} canOperate />);
  await user.click(await screen.findByRole('button', { name: 'data' }));
  await user.click(await screen.findByRole('button', { name: 'Edit text' }));
  const editor = await screen.findByRole('dialog', { name: 'Edit config.txt' });
  await user.clear(within(editor).getByLabelText('File contents'));
  await user.type(within(editor).getByLabelText('File contents'), 'new');
  await user.click(within(editor).getByRole('button', { name: 'Save file' }));
  const confirmation = screen.getByRole('dialog', { name: 'Overwrite file?' });
  expect(calls().some((call) => call.method === 'volume.files.write')).toBe(
    false,
  );
  await user.click(
    within(confirmation).getByRole('button', { name: 'Overwrite file' }),
  );
  expect(
    await within(confirmation).findByText('disk full'),
  ).toBeInTheDocument();
  expect(within(editor).getByLabelText('File contents')).toHaveValue('new');
  await user.click(
    within(confirmation).getByRole('button', { name: 'Overwrite file' }),
  );
  await waitFor(() =>
    expect(
      screen.queryByRole('dialog', { name: 'Overwrite file?' }),
    ).not.toBeInTheDocument(),
  );
  expect(
    calls()
      .filter((call) => call.method === 'volume.files.write')
      .at(-1),
  ).toMatchObject({
    engine: 'linux',
    params: { name: 'data', path: 'config.txt', data: btoa('new') },
  });
});

it('streams a confirmed volume restore through a transfer session', async () => {
  const volume = {
    name: 'data',
    engine: 'linux',
    driver: 'local',
    mountpoint: '/data',
    size: 0,
    usageCount: 0,
  };
  bridge.mockImplementation(async (_bridge, method: string) => {
    if (method === 'volume.list')
      return JSON.stringify({ items: [volume], errors: [] });
    if (method === 'volume.inspect')
      return JSON.stringify({ volume, containers: [] });
    if (method === 'volume.files.list') return JSON.stringify({ entries: [] });
    if (method === 'transfer.open')
      return JSON.stringify({ sessionID: 'restore-session', name: 'data.tar' });
    return 'null';
  });
  const user = userEvent.setup();
  render(<Volumes revision={0} canOperate />);
  await user.click(await screen.findByRole('button', { name: 'data' }));
  const archive = new File(['backup'], 'data.tar');
  Object.defineProperty(archive, 'slice', {
    value: () => ({
      arrayBuffer: async () => new TextEncoder().encode('backup').buffer,
    }),
  });
  await user.upload(screen.getByLabelText(/Restore backup/), archive);
  await user.click(screen.getByRole('button', { name: 'Restore backup' }));
  const confirmation = screen.getByRole('dialog', { name: 'Restore backup?' });
  expect(calls().some((entry) => entry.method === 'transfer.open')).toBe(false);
  await user.click(
    within(confirmation).getByRole('button', { name: 'Restore backup' }),
  );
  await waitFor(() =>
    expect(calls().some((entry) => entry.method === 'transfer.finish')).toBe(
      true,
    ),
  );
  expect(
    calls().find((entry) => entry.method === 'transfer.open'),
  ).toMatchObject({
    engine: 'linux',
    params: {
      operation: 'volume.restore',
      name: 'data',
      confirmed: true,
      size: 6,
    },
  });
  expect(
    calls().find((entry) => entry.method === 'transfer.write'),
  ).toMatchObject({
    params: { sessionID: 'restore-session', data: btoa('backup') },
  });
  await waitFor(() =>
    expect(calls().some((entry) => entry.method === 'transfer.close')).toBe(
      true,
    ),
  );
});

it('previews exact volume filters, confirms them and retains them after an error', async () => {
  let attempts = 0;
  bridge.mockImplementation(async (_bridge, method: string) => {
    if (method === 'storage.df')
      return JSON.stringify({
        Containers: {},
        Images: {},
        Volumes: {},
        BuildCache: {},
      });
    if (method === 'storage.preview')
      return JSON.stringify({
        scope: 'volumes',
        all: true,
        totalCount: 3,
        activeCount: 1,
        reclaimable: 2048,
        description: 'Remove unused volumes.',
      });
    if (method === 'storage.prune' && ++attempts === 1)
      throw new Error('engine refused cleanup');
    return JSON.stringify({ SpaceReclaimed: 2048 });
  });
  const user = userEvent.setup();
  render(<Storage revision={0} canOperate />);
  await user.selectOptions(screen.getByLabelText('Resource scope'), 'volumes');
  await user.click(screen.getByLabelText('Include all eligible resources'));
  const filterText =
    'label=project=my-app\nlabel=component=cache\nlabel!=retain=true';
  await user.type(screen.getByLabelText(/Docker filters/), filterText);
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }));
  const dialog = await screen.findByRole('dialog', { name: 'Prune volumes?' });
  const filters = {
    label: ['project=my-app', 'component=cache'],
    'label!': ['retain=true'],
  };
  expect(
    calls().find((call) => call.method === 'storage.preview')?.params,
  ).toMatchObject({ filters });
  expect(
    within(dialog).getByText(/estimate above covers the whole scope/),
  ).toBeInTheDocument();
  expect(dialog.querySelector('pre')?.textContent).toBe(filterText);
  expect(calls().some((call) => call.method === 'storage.prune')).toBe(false);
  await user.click(
    within(dialog).getByRole('button', { name: 'Prune selected scope' }),
  );
  expect(
    await within(dialog).findByText('engine refused cleanup'),
  ).toBeInTheDocument();
  expect(screen.getByLabelText(/Docker filters/)).toHaveValue(filterText);
  await user.click(
    within(dialog).getByRole('button', { name: 'Prune selected scope' }),
  );
  await waitFor(() =>
    expect(
      calls().filter((call) => call.method === 'storage.prune'),
    ).toHaveLength(2),
  );
  for (const request of calls().filter(
    (call) => call.method === 'storage.prune',
  ))
    expect(request).toMatchObject({
      engine: 'linux',
      params: { scope: 'volumes', all: true, confirmed: true, filters },
    });
});

it('keeps malformed cleanup filters and does not preview them', async () => {
  bridge.mockImplementation(async (_bridge, method: string) =>
    JSON.stringify(
      method === 'storage.df'
        ? { Containers: {}, Images: {}, Volumes: {}, BuildCache: {} }
        : null,
    ),
  );
  const user = userEvent.setup();
  render(<Storage revision={0} canOperate />);
  await user.type(screen.getByLabelText(/Docker filters/), 'label');
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }));
  expect(
    await screen.findByText(
      'Use one Docker filter per line in key=value format.',
    ),
  ).toBeInTheDocument();
  expect(screen.getByLabelText(/Docker filters/)).toHaveValue('label');
  expect(calls().some((call) => call.method === 'storage.preview')).toBe(false);
});

it('builds the chosen Dockerfile and displays correlated progress', async () => {
  let finish: (value: string) => void = () => {};
  bridge.mockImplementation(async (_bridge, method: string) => {
    if (method === 'image.list')
      return JSON.stringify({ items: [], errors: [] });
    if (method === 'transfer.open')
      return JSON.stringify({
        sessionID: 'build-session',
        name: 'context.tar',
      });
    if (method === 'transfer.finish')
      return await new Promise<string>((resolve) => {
        finish = resolve;
      });
    return 'null';
  });
  const user = userEvent.setup();
  const view = render(<Images revision={0} canOperate events={[]} />);
  await user.click(screen.getByRole('button', { name: 'Build image' }));
  const dialog = screen.getByRole('dialog', { name: 'Build image' });
  const file = new File(['FROM busybox\n'], 'Customfile');
  Object.defineProperty(file, 'webkitRelativePath', {
    value: 'context/build/Customfile',
  });
  Object.defineProperty(file, 'text', { value: async () => 'FROM busybox\n' });
  Object.defineProperty(file, 'stream', {
    value: () =>
      new ReadableStream({
        start(c) {
          c.enqueue(new TextEncoder().encode('FROM busybox\n'));
          c.close();
        },
      }),
  });
  const script = new File(['echo ok\n'], 'run.sh');
  Object.defineProperty(script, 'webkitRelativePath', {
    value: 'context/run.sh',
  });
  Object.defineProperty(script, 'stream', {
    value: () =>
      new ReadableStream({
        start(c) {
          c.enqueue(new TextEncoder().encode('echo ok\n'));
          c.close();
        },
      }),
  });
  const ignore = new File(['ignored\n'], 'Customfile.dockerignore');
  Object.defineProperty(ignore, 'webkitRelativePath', {
    value: 'context/build/Customfile.dockerignore',
  });
  Object.defineProperty(ignore, 'stream', {
    value: () =>
      new ReadableStream({
        start(c) {
          c.enqueue(new TextEncoder().encode('ignored\n'));
          c.close();
        },
      }),
  });
  await user.upload(within(dialog).getByLabelText(/Build context folder/), [
    file,
    script,
    ignore,
  ]);
  expect(
    (within(dialog).getByLabelText(/Build context folder/) as HTMLInputElement)
      .files,
  ).toHaveLength(3);
  await user.clear(within(dialog).getByLabelText('Dockerfile path'));
  await user.type(
    within(dialog).getByLabelText('Dockerfile path'),
    'build/Customfile',
  );
  await user.type(within(dialog).getByLabelText(/Tags/), 'example:1');
  await user.type(within(dialog).getByLabelText(/Executable files/), 'run.sh');
  await user.click(within(dialog).getByRole('button', { name: 'Build image' }));
  await waitFor(() =>
    expect(calls().some((call) => call.method === 'transfer.finish')).toBe(
      true,
    ),
  );
  const request = calls().find((call) => call.method === 'transfer.open')!;
  expect(request.params).toMatchObject({
    operation: 'image.build',
    dockerfile: 'FROM busybox\n',
    dockerfilePath: 'build/Customfile',
    tags: ['example:1'],
  });
  const chunks = calls()
    .filter((call) => call.method === 'transfer.write')
    .map((call) =>
      Uint8Array.from(atob(call.params.data as string), (character) =>
        character.charCodeAt(0),
      ),
    );
  const archive = new Uint8Array(
    chunks.reduce((sum, chunk) => sum + chunk.length, 0),
  );
  let offset = 0;
  for (const chunk of chunks) {
    archive.set(chunk, offset);
    offset += chunk.length;
  }
  const entries = await unpackTar(archive);
  expect(entries.map((entry) => entry.header.name)).toEqual([
    'build/Customfile',
    'run.sh',
    'build/Customfile.dockerignore',
  ]);
  expect(
    entries.find((entry) => entry.header.name === 'run.sh')?.header.mode,
  ).toBe(0o755);
  expect(
    new TextDecoder().decode(
      entries.find((entry) => entry.header.name.endsWith('.dockerignore'))
        ?.data,
    ),
  ).toBe('ignored\n');
  view.rerender(
    <Images
      revision={0}
      canOperate
      events={[
        {
          sequence: 1,
          time: '',
          engine: 'linux',
          project: request.params.operationId as string,
          type: 'image.progress',
          message: 'Step 1 complete\n',
        },
      ]}
    />,
  );
  expect(
    await within(dialog).findByText('Step 1 complete'),
  ).toBeInTheDocument();
  finish(JSON.stringify({ output: 'Build complete' }));
  expect(await within(dialog).findByText('Build complete')).toBeInTheDocument();
});

it('uploads a build context larger than 8 MiB without buffering its file', async () => {
  bridge.mockImplementation(async (_bridge, method: string) => {
    if (method === 'image.list')
      return JSON.stringify({ items: [], errors: [] });
    if (method === 'transfer.open')
      return JSON.stringify({ sessionID: 'large-build', name: 'context.tar' });
    return JSON.stringify(
      method === 'transfer.finish' ? { output: 'built' } : null,
    );
  });
  const recipe = new File(['FROM scratch\n'], 'Dockerfile');
  Object.defineProperty(recipe, 'webkitRelativePath', {
    value: 'context/Dockerfile',
  });
  Object.defineProperty(recipe, 'text', {
    value: async () => 'FROM scratch\n',
  });
  Object.defineProperty(recipe, 'stream', {
    value: () =>
      new ReadableStream({
        start(c) {
          c.enqueue(new TextEncoder().encode('FROM scratch\n'));
          c.close();
        },
      }),
  });
  const size = 8 * 1024 * 1024 + 17;
  const large = new File([new Uint8Array(size)], 'large.bin');
  Object.defineProperty(large, 'webkitRelativePath', {
    value: 'context/large.bin',
  });
  const stream = vi.fn(() => {
    let remaining = size;
    return new ReadableStream<Uint8Array>({
      pull(controller) {
        if (!remaining) {
          controller.close();
          return;
        }
        const length = Math.min(128 * 1024, remaining);
        remaining -= length;
        controller.enqueue(new Uint8Array(length));
      },
    });
  });
  Object.defineProperty(large, 'stream', { value: stream });
  const user = userEvent.setup();
  render(<Images revision={0} canOperate />);
  await user.click(screen.getByRole('button', { name: 'Build image' }));
  const dialog = screen.getByRole('dialog', { name: 'Build image' });
  await user.upload(within(dialog).getByLabelText(/Build context folder/), [
    recipe,
    large,
  ]);
  await user.click(within(dialog).getByRole('button', { name: 'Build image' }));
  expect(
    await within(dialog).findByText('built', {}, { timeout: 12000 }),
  ).toBeInTheDocument();
  expect(stream).toHaveBeenCalledOnce();
  const chunks = calls()
    .filter((entry) => entry.method === 'transfer.write')
    .map((entry) => atob(entry.params.data as string).length);
  expect(Math.max(...chunks)).toBeLessThanOrEqual(256 * 1024);
  expect(chunks.reduce((sum, chunk) => sum + chunk, 0)).toBeGreaterThan(size);
}, 15000);

it('keeps the reachable network inventory usable when one engine fails', async () => {
  bridge.mockImplementation(async (_bridge, method: string, engine: string) => {
    if (method === 'network.list' && engine === 'windows')
      throw new Error('engine offline');
    if (method === 'network.list')
      return JSON.stringify([
        {
          Id: 'net-id',
          Name: 'bridge',
          Driver: 'bridge',
          Scope: 'local',
          Internal: false,
          Attachable: false,
        },
      ]);
    if (method === 'network.inspect') return JSON.stringify({ Name: 'bridge' });
    return 'null';
  });
  const user = userEvent.setup();
  render(<Networks revision={0} canOperate={false} />);
  expect(
    await screen.findByText('windows: engine offline'),
  ).toBeInTheDocument();
  expect(
    screen.getByText('bridge', { selector: 'strong' }),
  ).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Inspect' }));
  expect(
    await screen.findByRole('dialog', { name: 'Network · bridge' }),
  ).toBeInTheDocument();
  expect(
    calls().find((call) => call.method === 'network.inspect'),
  ).toMatchObject({ engine: 'linux', params: { id: 'net-id' } });
});

it('applies a selected DNS inhibition override to a Linux network', async () => {
  bridge.mockImplementation(async (_bridge, method: string) =>
    method === 'network.list' ? JSON.stringify([]) : 'null',
  );
  const user = userEvent.setup();
  render(<Networks revision={0} canOperate />);
  await user.click(screen.getByRole('button', { name: /Create network/ }));
  const dialog = await screen.findByRole('dialog', { name: 'Create network' });
  const inhibition = within(dialog).getByLabelText('DNS inhibition override');
  expect(inhibition).toHaveValue('global');
  expect(
    within(inhibition).getByRole('option', {
      name: 'Inhibit IPv4 (A) records',
    }),
  ).toBeInTheDocument();
  expect(
    within(inhibition).getByRole('option', {
      name: 'Inhibit IPv6 (AAAA) records',
    }),
  ).toBeInTheDocument();
  await user.type(within(dialog).getByLabelText('Name'), 'ipv6-network');
  await user.selectOptions(inhibition, 'ipv6');
  await user.click(
    within(dialog).getByRole('button', { name: 'Create network' }),
  );
  await waitFor(() =>
    expect(
      calls().find((call) => call.method === 'network.create'),
    ).toMatchObject({
      engine: 'linux',
      params: {
        name: 'ipv6-network',
        options: { 'com.ucd.dns-inhibition': 'ipv6' },
      },
    }),
  );
});

it('omits the DNS override for global Linux defaults and Windows networks', async () => {
  bridge.mockImplementation(async (_bridge, method: string) =>
    method === 'network.list' ? JSON.stringify([]) : 'null',
  );
  const user = userEvent.setup();
  render(<Networks revision={0} canOperate />);
  await user.click(screen.getByRole('button', { name: /Create network/ }));
  let dialog = await screen.findByRole('dialog', { name: 'Create network' });
  await user.type(within(dialog).getByLabelText('Name'), 'global-network');
  await user.click(
    within(dialog).getByRole('button', { name: 'Create network' }),
  );
  await waitFor(() =>
    expect(
      calls().filter((call) => call.method === 'network.create'),
    ).toHaveLength(1),
  );
  expect(
    calls().find((call) => call.method === 'network.create'),
  ).toMatchObject({ engine: 'linux' });
  expect(
    calls().find((call) => call.method === 'network.create')?.params,
  ).not.toHaveProperty('options');

  await user.click(screen.getByRole('button', { name: /Create network/ }));
  dialog = await screen.findByRole('dialog', { name: 'Create network' });
  await user.selectOptions(within(dialog).getByLabelText('Engine'), 'windows');
  expect(
    within(dialog).queryByLabelText('DNS inhibition override'),
  ).not.toBeInTheDocument();
  await user.type(within(dialog).getByLabelText('Name'), 'windows-network');
  await user.click(
    within(dialog).getByRole('button', { name: 'Create network' }),
  );
  await waitFor(() =>
    expect(
      calls().filter((call) => call.method === 'network.create'),
    ).toHaveLength(2),
  );
  expect(
    calls().filter((call) => call.method === 'network.create')[1],
  ).toMatchObject({ engine: 'windows' });
  expect(
    calls().filter((call) => call.method === 'network.create')[1]?.params,
  ).not.toHaveProperty('options');
});

it('uses one shared registry list and signs in and out without an engine selector', async () => {
  bridge.mockImplementation(async (_bridge, method: string) => {
    if (method === 'registry.list')
      return JSON.stringify([{ server: 'registry.example', username: 'user' }]);
    return 'null';
  });
  const user = userEvent.setup();
  render(<Registry canOperate />);
  expect(await screen.findByText('registry.example')).toBeInTheDocument();
  expect(screen.queryByLabelText('Engine')).not.toBeInTheDocument();
  expect(calls().find((call) => call.method === 'registry.list')).toMatchObject(
    {
      engine: '',
    },
  );
  await user.type(screen.getByLabelText('Registry server'), 'second.example');
  await user.type(screen.getByLabelText('Username'), 'second-user');
  await user.type(screen.getByLabelText('Password or token'), 'secret-token');
  await user.click(screen.getByRole('button', { name: 'Sign in' }));
  expect(
    calls().find((call) => call.method === 'registry.login'),
  ).toMatchObject({
    engine: '',
    params: {
      server: 'second.example',
      username: 'second-user',
      password: 'secret-token',
    },
  });
  await user.click(screen.getByRole('button', { name: 'Sign out' }));
  const dialog = screen.getByRole('dialog', {
    name: 'Sign out of registry.example?',
  });
  await user.click(within(dialog).getByRole('button', { name: 'Sign out' }));
  await waitFor(() =>
    expect(
      calls().find((call) => call.method === 'registry.logout'),
    ).toMatchObject({
      engine: '',
      params: { server: 'registry.example' },
    }),
  );
});

it('saves distinct daemon and container proxy settings through machine settings', async () => {
  bridge.mockImplementation(mockSettings);
  const user = userEvent.setup();
  render(<Settings revision={0} admin events={[]} />);
  await user.click(screen.getByRole('button', { name: 'Proxies' }));
  await screen.findByRole('heading', { name: 'Proxies and registry mirrors' });
  expect(
    screen.getByRole('group', { name: 'Docker engine traffic' }),
  ).toBeInTheDocument();
  expect(
    screen.getByRole('group', { name: 'Container and build traffic' }),
  ).toBeInTheDocument();
  expect(
    screen.getByText(
      /Used by the Linux and Windows Docker engines for registry pulls and pushes/,
    ),
  ).toBeInTheDocument();
  expect(
    screen.getByText(/Applications must honor the proxy variables/),
  ).toBeInTheDocument();
  await user.type(
    screen.getByLabelText('Docker proxy HTTP URL'),
    'http://docker-proxy.example:8080',
  );
  await user.type(
    screen.getByLabelText('Docker proxy HTTPS URL'),
    'http://docker-proxy.example:8080',
  );
  await user.type(
    screen.getByLabelText('Docker proxy SOCKS5 URL'),
    'socks5://docker-proxy.example:1080',
  );
  await user.type(
    screen.getByLabelText('Container proxy HTTP URL'),
    'http://container-proxy.example:8080',
  );
  await user.type(
    screen.getByLabelText('Container proxy Bypass list'),
    'localhost,.internal.example',
  );
  await user.click(screen.getByRole('button', { name: 'Save proxy settings' }));
  await waitFor(() =>
    expect(
      calls().find((call) => call.method === 'system.configure')?.params,
    ).toMatchObject({
      dockerProxy: {
        http: 'http://docker-proxy.example:8080',
        https: 'http://docker-proxy.example:8080',
        socks5: 'socks5://docker-proxy.example:1080',
        noProxy: '',
      },
      containersProxy: {
        http: 'http://container-proxy.example:8080',
        https: '',
        socks5: '',
        noProxy: 'localhost,.internal.example',
      },
    }),
  );
  expect(
    await screen.findByText(
      /New containers and builds on both engines receive the configured proxy defaults/,
    ),
  ).toBeInTheDocument();
});

it('saves Linux network defaults and applies Resource saver without restarting the engine', async () => {
  bridge.mockImplementation(mockSettings);
  const user = userEvent.setup();
  render(<Settings revision={0} admin events={[]} />);
  await user.click(screen.getByRole('button', { name: 'Network' }));
  expect(
    await screen.findByText(
      /Default published port binding applies to newly created standalone containers and managed Compose on both engines\./,
    ),
  ).toBeInTheDocument();
  expect(
    screen.getByText(
      /The subnet, IP mode, DNS filtering, and host networking settings configure Linux Docker; Windows networking otherwise uses native per-network HNS settings\./,
    ),
  ).toBeInTheDocument();
  expect(
    await screen.findByText(
      /Default networking mode affects newly created custom Linux bridge networks, including Compose-created networks\./,
    ),
  ).toBeInTheDocument();
  expect(
    screen.getByText(
      /Existing custom networks retain their addressing; this mode does not disable IPv4 on Docker's built-in bridge\./,
    ),
  ).toBeInTheDocument();
  await user.selectOptions(
    await screen.findByLabelText('Default networking mode'),
    'dual-stack',
  );
  await user.selectOptions(
    screen.getByLabelText('DNS record filtering'),
    'ipv6',
  );
  await user.selectOptions(
    screen.getByLabelText('Default published port binding (both engines)'),
    'all',
  );
  await user.click(
    screen.getByLabelText(
      'Enable Linux host networking for containers using --network host',
    ),
  );
  await user.click(
    screen.getByRole('button', { name: 'Save network defaults' }),
  );
  await waitFor(() =>
    expect(
      calls().find((call) => call.method === 'system.configure')?.params,
    ).toMatchObject({
      network: {
        dockerSubnet: '',
        defaultNetworkingMode: 'dual-stack',
        dnsInhibition: 'ipv6',
        defaultPortBinding: 'all',
        hostNetworkingEnabled: true,
      },
    }),
  );
  expect(
    await screen.findByText(
      'Saved. The Linux engine needs a restart to apply bridge and DNS defaults. Existing networks are unchanged.',
    ),
  ).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Resources' }));
  await user.click(screen.getByLabelText('Enable Resource saver'));
  const slider = screen.getByRole('slider', {
    name: 'Resource saver threshold (seconds)',
  });
  expect(slider).toBeEnabled();
  expect(slider).toHaveAttribute('min', '31');
  fireEvent.change(slider, { target: { value: '3600' } });
  await user.click(screen.getByRole('button', { name: 'Save resource saver' }));
  await waitFor(() =>
    expect(
      calls()
        .filter((call) => call.method === 'system.configure')
        .at(-1)?.params,
    ).toMatchObject({ resourceSaver: { enabled: true, idleSeconds: 3600 } }),
  );
  expect(
    await screen.findByText(
      'Saved. Resource saver will sleep the Linux engine after it has no running containers for the selected idle period.',
    ),
  ).toBeInTheDocument();
});

it('shows when the Linux engine is sleeping in Resource saver', async () => {
  bridge.mockImplementation(
    async (binding: string, method: string, engine: string, raw: string) => {
      if (method === 'engine.status')
        return JSON.stringify({
          id: 'linux',
          os: 'linux',
          online: true,
          containers: 0,
          running: 0,
          stopped: 0,
          cpus: 4,
          memoryBytes: 4096 * 1024 * 1024,
          resourceSaver: true,
          activeOperations: 0,
        });
      return mockSettings(binding, method, engine, raw);
    },
  );
  render(<Settings revision={0} admin events={[]} />);
  await userEvent.click(screen.getByRole('button', { name: 'Resources' }));
  expect(await screen.findByText('Sleeping')).toBeInTheDocument();
  expect(
    screen.getByText(
      'It wakes automatically when an operation needs the engine.',
    ),
  ).toBeInTheDocument();
});

it('creates a virtual file share from an administrator-approved local directory', async () => {
  bridge.mockImplementation(mockSettings);
  const user = userEvent.setup();
  render(<Settings revision={0} admin events={[]} />);
  await user.click(screen.getByRole('button', { name: 'File sharing' }));
  await user.click(
    await screen.findByRole('button', { name: /Add virtual file share/ }),
  );
  const dialog = screen.getByRole('dialog', { name: 'Add virtual file share' });
  expect(
    within(dialog).getByText(
      /UNC paths and directories inside its protected data folders/,
    ),
  ).toBeInTheDocument();
  await user.type(within(dialog).getByLabelText('Share name'), 'project-data');
  await user.type(
    within(dialog).getByLabelText('Windows directory'),
    'D:\\Projects\\project-data',
  );
  await user.click(
    within(dialog).getByRole('button', { name: 'Add virtual file share' }),
  );
  await waitFor(() =>
    expect(
      calls().find((call) => call.method === 'share.create')?.params,
    ).toEqual({
      name: 'project-data',
      windowsPath: 'D:\\Projects\\project-data',
    }),
  );
});
