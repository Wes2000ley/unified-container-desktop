import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { call } from '../src/api';
import type { Container } from '../src/api';
import { downloadTransfer, uploadTransfer } from '../src/transfers';
import { ContainerDetail } from '../src/container-detail';
import { savePreferences } from '../src/preferences';

const terminal = vi.hoisted(() => ({
  input: null as null | ((data: string) => void),
  output: [] as Uint8Array[],
}));
vi.mock('../src/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../src/api')>()),
  call: vi.fn(),
}));
vi.mock('../src/transfers', () => ({
  downloadTransfer: vi.fn(),
  uploadTransfer: vi.fn(),
}));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    rows = 24;
    cols = 80;
    loadAddon() {}
    open() {}
    onData(callback: (data: string) => void) {
      terminal.input = callback;
      return { dispose() {} };
    }
    write(data: Uint8Array) {
      terminal.output.push(data);
    }
    clear() {}
    focus() {}
    dispose() {}
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit() {}
  },
}));

const request = vi.mocked(call);
const container: Container = {
  id: 'abc123',
  name: 'api',
  engine: 'linux',
  image: 'example/api',
  state: 'running',
  status: 'Up',
  ports: [],
  cpuPercent: 0,
  memoryBytes: 0,
  metricsAvailable: true,
};
const initialLogs = { text: 'INFO ready\nERROR connection failed\n' };
function renderDetail(canOperate = true) {
  return render(
    <ContainerDetail
      container={container}
      canOperate={canOperate}
      onChanged={vi.fn()}
      onClose={vi.fn()}
    />,
  );
}
beforeEach(() => {
  request.mockReset();
  vi.mocked(downloadTransfer).mockReset().mockResolvedValue(true);
  vi.mocked(uploadTransfer).mockReset().mockResolvedValue(undefined);
  terminal.input = null;
  terminal.output = [];
  vi.stubGlobal(
    'ResizeObserver',
    class {
      observe() {}
      disconnect() {}
    },
  );
});

it('filters log lines, reports invalid regex and follows the real stream through EOF', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.logs.open') return { sessionID: 'log-session' };
    if (method === 'container.logs.read')
      return { data: btoa('ERROR live\n'), eof: true };
    if (method === 'container.logs.close') return null;
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  expect(await screen.findByText(/INFO ready/)).toHaveTextContent(
    'ERROR connection failed',
  );
  await user.type(screen.getByLabelText('Filter lines'), 'ERROR');
  expect(screen.queryByText(/INFO ready/)).not.toBeInTheDocument();
  expect(screen.getByText(/ERROR connection failed/)).toBeInTheDocument();
  await user.click(screen.getByLabelText('Regular expression'));
  await user.clear(screen.getByLabelText('Filter lines'));
  await user.type(screen.getByLabelText('Filter lines'), '(');
  expect(screen.getByRole('alert')).toHaveTextContent(
    'valid regular expression',
  );
  await user.clear(screen.getByLabelText('Filter lines'));
  await user.click(screen.getByLabelText('Follow'));
  expect(await screen.findByText('ERROR live')).toBeInTheDocument();
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith('container.logs.close', 'linux', {
      sessionID: 'log-session',
    }),
  );
  expect(request).toHaveBeenCalledWith(
    'container.logs.open',
    'linux',
    expect.objectContaining({ id: 'abc123', stdout: true, stderr: true }),
    expect.objectContaining({ signal: expect.any(AbortSignal) }),
  );
});

it('uses the saved log line and timestamp defaults for a new container view', async () => {
  savePreferences({
    refreshSeconds: 15,
    logTail: 1000,
    logTimestamps: true,
  });
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    throw new Error(`Unexpected ${method}`);
  });
  renderDetail();
  expect(await screen.findByText(/INFO ready/)).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith(
    'container.logs',
    'linux',
    {
      id: 'abc123',
      tail: 1000,
      timestamps: true,
      stdout: true,
      stderr: true,
    },
    { signal: expect.any(AbortSignal) },
  );
});

it('shows structured inspection and mount information, with raw JSON on request', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.inspect')
      return {
        Id: 'abc123',
        Config: { Image: 'example/api', Cmd: ['serve'], Env: ['MODE=test'] },
        Mounts: [
          {
            Type: 'bind',
            Source: '/srv/data',
            Destination: '/data',
            RW: false,
          },
        ],
      };
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Inspect' }));
  expect(
    await screen.findByText('MODE=test', { exact: false }),
  ).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Raw JSON' }));
  expect(screen.getByText(/"Id": "abc123"/)).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Mounts' }));
  expect(await screen.findByText('/srv/data')).toBeInTheDocument();
  expect(screen.getByText('Read only')).toBeInTheDocument();
});

it('confirms overwrites and retains file edits after a write failure', async () => {
  let attempts = 0;
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.files.list')
      return {
        entries: [
          {
            name: 'config.txt',
            path: '/config.txt',
            type: 'file',
            size: 3,
            modified: '',
          },
        ],
      };
    if (method === 'container.files.read') return { data: btoa('old') };
    if (method === 'container.files.write') {
      attempts++;
      if (attempts === 1) throw new Error('disk full');
      return null;
    }
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Files' }));
  await user.click(await screen.findByRole('button', { name: 'config.txt' }));
  const editor = await screen.findByLabelText('File contents');
  await user.clear(editor);
  await user.type(editor, 'updated');
  const discard = vi.spyOn(window, 'confirm').mockReturnValue(false);
  await user.click(screen.getByRole('button', { name: 'Stats' }));
  expect(editor).toHaveValue('updated');
  expect(discard).toHaveBeenCalled();
  discard.mockRestore();
  await user.click(screen.getByRole('button', { name: 'Save' }));
  const dialog = screen.getByRole('dialog', { name: 'Overwrite file?' });
  expect(attempts).toBe(0);
  await user.click(
    within(dialog).getByRole('button', { name: 'Overwrite file' }),
  );
  expect(await within(dialog).findByText('disk full')).toBeInTheDocument();
  expect(editor).toHaveValue('updated');
  await user.click(
    within(dialog).getByRole('button', { name: 'Overwrite file' }),
  );
  await waitFor(() =>
    expect(
      screen.queryByRole('dialog', { name: 'Overwrite file?' }),
    ).not.toBeInTheDocument(),
  );
  expect(request).toHaveBeenLastCalledWith('container.files.list', 'linux', {
    id: 'abc123',
    path: '/',
  });
  expect(request).toHaveBeenCalledWith('container.files.write', 'linux', {
    id: 'abc123',
    path: '/config.txt',
    data: btoa('updated'),
  });
  expect(screen.getByText('UTF-8 text')).toBeInTheDocument();
});

it('opens an interactive terminal and forwards input before disconnecting', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.exec.open') return { sessionID: 'exec-session' };
    if (method === 'container.exec.read') return await new Promise(() => {});
    if (
      method === 'container.exec.write' ||
      method === 'container.exec.close' ||
      method === 'container.exec.resize'
    )
      return null;
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Exec' }));
  await user.type(screen.getByLabelText('User'), 'nobody');
  await user.type(screen.getByLabelText('Working directory'), '/tmp');
  await user.type(screen.getByLabelText('Environment'), 'MODE=test');
  await user.click(screen.getByRole('button', { name: 'Connect' }));
  expect(await screen.findByText('Connected')).toBeInTheDocument();
  expect(request).toHaveBeenCalledWith(
    'container.exec.open',
    'linux',
    expect.objectContaining({
      id: 'abc123',
      command: ['/bin/sh'],
      user: 'nobody',
      workingDir: '/tmp',
      env: ['MODE=test'],
      rows: 24,
      cols: 80,
    }),
    expect.objectContaining({ signal: expect.any(AbortSignal) }),
  );
  terminal.input?.('hello\r');
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith(
      'container.exec.write',
      'linux',
      { sessionID: 'exec-session', data: btoa('hello\r') },
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    ),
  );
  await user.click(screen.getByRole('button', { name: 'Disconnect' }));
  expect(request).toHaveBeenCalledWith('container.exec.close', 'linux', {
    sessionID: 'exec-session',
  });
  expect(await screen.findByText('Disconnected')).toBeInTheDocument();
});

it('distinguishes unavailable statistics from zero and blocks operator-only tabs', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.stats')
      return {
        metricsAvailable: true,
        cpuPercent: 0,
        memoryBytes: 0,
        pids: 0,
        sampleTime: '2026-11-13T00:00:00Z',
      };
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail(false);
  await user.click(screen.getByRole('button', { name: 'Stats' }));
  expect(await screen.findByText('0.0%')).toBeInTheDocument();
  expect(
    screen.getByText('Memory limit: unavailable', { exact: false }),
  ).toBeInTheDocument();
  expect(screen.getAllByText('Unavailable').length).toBeGreaterThan(0);
  await user.click(screen.getByRole('button', { name: 'Files' }));
  expect(
    screen.getByText(/Container Operators membership is required/),
  ).toBeInTheDocument();
  expect(
    request.mock.calls.some(([method]) => method === 'container.files.list'),
  ).toBe(false);
});

it('closes and cancels an active log stream when leaving the tab', async () => {
  let readSignal: AbortSignal | undefined;
  request.mockImplementation(async (method, _engine, _params, options) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.logs.open') return { sessionID: 'live-session' };
    if (method === 'container.logs.read') {
      readSignal = options?.signal;
      return await new Promise(() => {});
    }
    if (method === 'container.logs.close') return null;
    if (method === 'container.stats') return { metricsAvailable: false };
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await screen.findByText(/INFO ready/);
  await user.click(screen.getByLabelText('Follow'));
  await waitFor(() => expect(readSignal).toBeDefined());
  await user.click(screen.getByRole('button', { name: 'Stats' }));
  expect(readSignal?.aborted).toBe(true);
  expect(request).toHaveBeenCalledWith('container.logs.close', 'linux', {
    sessionID: 'live-session',
  });
});

it('confirms an upload even when the directory listing shows no existing file', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.files.list') return { entries: [] };
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Files' }));
  expect(
    screen.queryByRole('button', { name: 'Prepare file access' }),
  ).not.toBeInTheDocument();
  await screen.findByText('This directory is empty.');
  await user.click(screen.getByRole('button', { name: 'Up' }));
  await waitFor(() =>
    expect(
      request.mock.calls.filter(
        ([method]) => method === 'container.files.list',
      ),
    ).toHaveLength(2),
  );
  expect(
    await screen.findByText('This directory is empty.'),
  ).toBeInTheDocument();
  const file = new File([new Uint8Array(9 * 1024 * 1024)], 'notes.txt');
  Object.defineProperty(file, 'arrayBuffer', {
    value: () => {
      throw new Error('The entire upload must not be buffered');
    },
  });
  await user.upload(screen.getByLabelText('Upload file'), file);
  const dialog = await screen.findByRole('dialog', { name: 'Upload file?' });
  expect(dialog).toHaveTextContent('An existing file at this path');
  expect(dialog).toHaveTextContent(
    'ACLs and extended attributes may not be preserved',
  );
  expect(uploadTransfer).not.toHaveBeenCalled();
  await user.click(within(dialog).getByRole('button', { name: 'Upload file' }));
  await waitFor(() =>
    expect(uploadTransfer).toHaveBeenCalledWith(
      'linux',
      'container.files.upload',
      { id: 'abc123', path: '/notes.txt' },
      file,
      expect.objectContaining({
        signal: expect.any(AbortSignal),
        onProgress: expect.any(Function),
      }),
    ),
  );
});

it('requires explicit confirmation to prepare Windows file access and retries a failed preparation', async () => {
  let listCalls = 0;
  let prepareCalls = 0;
  let finishPreparation:
    ((value: { prepared: boolean; restarted: boolean }) => void) | undefined;
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.files.list') {
      listCalls++;
      if (listCalls === 1)
        throw new Error('Prepare file access to browse this container');
      return { entries: [] };
    }
    if (method === 'container.files.prepare') {
      prepareCalls++;
      if (prepareCalls === 1)
        throw new Error('Could not restart the container');
      return await new Promise((resolve) => {
        finishPreparation = resolve;
      });
    }
    throw new Error(`Unexpected ${method}`);
  });
  const onChanged = vi.fn();
  const user = userEvent.setup();
  render(
    <ContainerDetail
      container={{ ...container, engine: 'windows' }}
      canOperate
      onClose={vi.fn()}
      onChanged={onChanged}
    />,
  );
  await user.click(screen.getByRole('button', { name: 'Files' }));
  expect(
    await screen.findByText('Prepare file access to browse this container'),
  ).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Prepare file access' }));
  let dialog = screen.getByRole('dialog', { name: 'Prepare file access?' });
  expect(dialog).toHaveTextContent('installs the UCD file utility');
  expect(dialog).toHaveTextContent('briefly stopped and restarted');
  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
  expect(prepareCalls).toBe(0);
  await user.click(screen.getByRole('button', { name: 'Prepare file access' }));
  dialog = screen.getByRole('dialog', { name: 'Prepare file access?' });
  await user.click(
    within(dialog).getByRole('button', { name: 'Prepare file access' }),
  );
  expect(
    await within(dialog).findByText('Could not restart the container'),
  ).toBeInTheDocument();
  expect(onChanged).toHaveBeenCalledTimes(1);
  expect(request).toHaveBeenCalledWith('container.files.prepare', 'windows', {
    id: 'abc123',
    confirmed: true,
  });
  await user.click(
    within(dialog).getByRole('button', { name: 'Prepare file access' }),
  );
  expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled();
  await user.click(within(dialog).getByRole('button', { name: 'Close' }));
  expect(
    screen.getByRole('dialog', { name: 'Prepare file access?' }),
  ).toBeInTheDocument();
  finishPreparation?.({ prepared: true, restarted: true });
  expect(
    await screen.findByText(
      'File access is prepared and the container was restarted.',
    ),
  ).toBeInTheDocument();
  expect(
    await screen.findByText('This directory is empty.'),
  ).toBeInTheDocument();
  expect(prepareCalls).toBe(2);
  expect(listCalls).toBe(2);
  expect(onChanged).toHaveBeenCalledTimes(2);
});

it('does not offer Windows preparation without operator permission', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  render(
    <ContainerDetail
      container={{ ...container, engine: 'windows' }}
      canOperate={false}
      onClose={vi.fn()}
      onChanged={vi.fn()}
    />,
  );
  await user.click(screen.getByRole('button', { name: 'Files' }));
  expect(
    screen.getByText(/Container Operators membership is required/),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole('button', { name: 'Prepare file access' }),
  ).not.toBeInTheDocument();
  expect(
    request.mock.calls.some(([method]) => method === 'container.files.prepare'),
  ).toBe(false);
});

it('writes final terminal output before reporting exit and closes the session', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.exec.open') return { sessionID: 'finished-exec' };
    if (method === 'container.exec.read')
      return { data: btoa('goodbye'), eof: true, exitCode: 3 };
    if (method === 'container.exec.close') return null;
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Exec' }));
  await user.click(screen.getByRole('button', { name: 'Connect' }));
  expect(await screen.findByText('Exited with code 3')).toBeInTheDocument();
  expect(new TextDecoder().decode(terminal.output[0])).toBe('goodbye');
  expect(request).toHaveBeenCalledWith('container.exec.close', 'linux', {
    sessionID: 'finished-exec',
  });
});

it('downloads large files and directory archives through the shared transfer path', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.files.list')
      return {
        entries: [
          {
            name: 'large.bin',
            path: '/large.bin',
            type: 'file',
            size: 2 * 1024 ** 3,
            modified: '',
          },
          {
            name: 'data',
            path: '/data',
            type: 'directory',
            size: 0,
            modified: '',
          },
        ],
      };
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Files' }));
  const file = await screen.findByRole('button', { name: 'large.bin' });
  await user.click(file);
  expect(screen.getByRole('alert')).toHaveTextContent(
    'Text editing supports files up to 8 MiB',
  );
  expect(
    request.mock.calls.some(([method]) => method === 'container.files.read'),
  ).toBe(false);
  await user.click(
    within(file.closest('tr')!).getByRole('button', { name: 'Download' }),
  );
  await waitFor(() =>
    expect(downloadTransfer).toHaveBeenCalledWith(
      'linux',
      'container.files.download',
      { id: 'abc123', path: '/large.bin' },
      'large.bin',
    ),
  );
  const directory = screen.getByRole('button', { name: 'data' });
  await user.click(
    within(directory.closest('tr')!).getByRole('button', {
      name: 'Download archive',
    }),
  );
  await waitFor(() =>
    expect(downloadTransfer).toHaveBeenCalledWith(
      'linux',
      'container.files.archive',
      { id: 'abc123', path: '/data' },
      'data.tar',
    ),
  );
  vi.mocked(downloadTransfer).mockRejectedValueOnce(
    new Error('Save dialog failed'),
  );
  await user.click(
    within(file.closest('tr')!).getByRole('button', { name: 'Download' }),
  );
  expect(await screen.findByText('Save dialog failed')).toBeInTheDocument();
});

it('shows upload progress, permits cancellation and retains the file for retry', async () => {
  request.mockImplementation(async (method) => {
    if (method === 'container.logs') return initialLogs;
    if (method === 'container.files.list') return { entries: [] };
    throw new Error(`Unexpected ${method}`);
  });
  let signal: AbortSignal | undefined;
  vi.mocked(uploadTransfer).mockImplementationOnce(
    async (_engine, _operation, _params, file, options) => {
      signal = options?.signal;
      options?.onProgress?.(5, file.size);
      return await new Promise((_resolve, reject) =>
        signal?.addEventListener(
          'abort',
          () => reject(new Error('Upload canceled')),
          { once: true },
        ),
      );
    },
  );
  const user = userEvent.setup();
  renderDetail();
  await user.click(screen.getByRole('button', { name: 'Files' }));
  await screen.findByText('This directory is empty.');
  const file = new File(['1234567890'], 'data.txt');
  await user.upload(screen.getByLabelText('Upload file'), file);
  const dialog = await screen.findByRole('dialog', { name: 'Upload file?' });
  await user.click(within(dialog).getByRole('button', { name: 'Upload file' }));
  expect(
    await within(dialog).findByText('Uploading 5 B of 10 B'),
  ).toBeInTheDocument();
  await user.click(
    within(dialog).getByRole('button', { name: 'Cancel upload' }),
  );
  expect(
    await within(dialog).findByText('Upload canceled'),
  ).toBeInTheDocument();
  expect(signal?.aborted).toBe(true);
  await user.click(within(dialog).getByRole('button', { name: 'Upload file' }));
  await waitFor(() =>
    expect(
      screen.queryByRole('dialog', { name: 'Upload file?' }),
    ).not.toBeInTheDocument(),
  );
  expect(vi.mocked(uploadTransfer).mock.calls).toHaveLength(2);
  expect(vi.mocked(uploadTransfer).mock.calls[1]?.[3]).toBe(file);
});
