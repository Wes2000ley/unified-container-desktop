import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Projects } from '../src/projects';

const { request } = vi.hoisted(() => ({ request: vi.fn() }));
vi.mock('../src/api', async (original) => ({
  ...(await original<typeof import('../src/api')>()),
  call: request,
}));

const project = {
  id: 'demo',
  name: 'Demo',
  parts: [
    {
      engine: 'linux',
      name: 'demo-linux',
      directory: '/projects/demo-linux',
      composeFile: '/projects/demo-linux/compose.yaml',
    },
  ],
};

beforeEach(() => {
  request.mockReset();
});

it('shows project-scoped build output while the selected service is building', async () => {
  let finish!: (result: unknown) => void;
  let outputReady = false;
  request.mockImplementation(async (method: string) => {
    if (method === 'project.list') return [project];
    if (method === 'system.events')
      return {
        epoch: 'one',
        events: outputReady
          ? [
              {
                sequence: 1,
                engine: 'linux',
                project: 'other',
                type: 'compose.output',
                message: 'other output',
              },
              {
                sequence: 2,
                engine: 'linux',
                project: 'demo-linux',
                type: 'compose.output',
                message: 'building web\n',
              },
            ]
          : [],
      };
    if (method === 'project.action')
      return new Promise((resolve) => {
        finish = resolve;
      });
    throw new Error(`Unexpected ${method}`);
  });
  const user = userEvent.setup();
  render(<Projects revision={0} canOperate />);
  await screen.findByRole('heading', { name: 'Demo' });
  await user.type(screen.getByLabelText('Services for Demo'), 'web');
  await user.click(screen.getByRole('button', { name: 'Build' }));
  const dialog = await screen.findByRole('dialog', { name: 'Demo · Build' });
  outputReady = true;
  expect(await within(dialog).findByText(/building web/)).toBeInTheDocument();
  expect(within(dialog).queryByText(/other output/)).not.toBeInTheDocument();
  expect(request).toHaveBeenCalledWith(
    'project.action',
    '',
    expect.objectContaining({ id: 'demo', action: 'build', services: ['web'] }),
  );
  finish({ projectID: 'demo', success: true, parts: [{ engine: 'linux' }] });
  expect(
    await within(dialog).findByText(/Operation finished/),
  ).toBeInTheDocument();
});

it('follows, searches, exports and closes a project log stream', async () => {
  let readCount = 0;
  let pendingSignal: AbortSignal | undefined;
  request.mockImplementation(
    async (
      method: string,
      _engine: string,
      _params: unknown,
      options?: { signal?: AbortSignal },
    ) => {
      if (method === 'project.list') return [project];
      if (method === 'project.logs.open')
        return {
          projectID: 'demo',
          success: true,
          parts: [{ engine: 'linux', result: { sessionID: 'logs-1' } }],
        };
      if (method === 'project.logs.read') {
        if (readCount++ === 0)
          return {
            data: btoa('web | ready\nweb | error occurred\n'),
            eof: false,
          };
        pendingSignal = options?.signal;
        return new Promise((_resolve, reject) =>
          options?.signal?.addEventListener('abort', () =>
            reject(new Error('aborted')),
          ),
        );
      }
      if (method === 'project.logs.close') return null;
      throw new Error(`Unexpected ${method}`);
    },
  );
  const createURL = vi
    .spyOn(URL, 'createObjectURL')
    .mockReturnValue('blob:test');
  const revokeURL = vi
    .spyOn(URL, 'revokeObjectURL')
    .mockImplementation(() => undefined);
  const download = vi
    .spyOn(HTMLAnchorElement.prototype, 'click')
    .mockImplementation(() => undefined);
  const user = userEvent.setup();
  render(<Projects revision={0} canOperate />);
  await screen.findByRole('heading', { name: 'Demo' });
  await user.type(screen.getByLabelText('Services for Demo'), 'web');
  await user.click(screen.getByRole('button', { name: 'Logs' }));
  const dialog = await screen.findByRole('dialog', { name: 'Demo · Logs' });
  expect(await within(dialog).findByText(/error occurred/)).toBeInTheDocument();
  await user.type(
    within(dialog).getByLabelText('Search project logs'),
    'error',
  );
  expect(within(dialog).getByLabelText('linux project logs')).toHaveTextContent(
    'error occurred',
  );
  expect(
    within(dialog).getByLabelText('linux project logs'),
  ).not.toHaveTextContent('ready');
  await user.click(within(dialog).getByRole('button', { name: 'Export logs' }));
  expect(createURL).toHaveBeenCalled();
  expect(download).toHaveBeenCalled();
  await waitFor(() => expect(revokeURL).toHaveBeenCalledWith('blob:test'));
  expect(request).toHaveBeenCalledWith('project.logs.open', '', {
    id: 'demo',
    engine: 'linux',
    services: ['web'],
  });
  await user.click(
    within(dialog).getAllByRole('button', { name: 'Close' })[0]!,
  );
  await waitFor(() =>
    expect(request).toHaveBeenCalledWith('project.logs.close', 'linux', {
      sessionID: 'logs-1',
    }),
  );
  expect(pendingSignal?.aborted).toBe(true);
  createURL.mockRestore();
  revokeURL.mockRestore();
  download.mockRestore();
});
