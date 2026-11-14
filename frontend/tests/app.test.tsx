import { beforeEach, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { App } from '../src/app/app';
import {
  failNextProjectRead,
  resetFake,
  requests,
  setFakeEvents,
  setFakeRole,
} from './fixtures/fake-runtime';
vi.mock('@wailsio/runtime', () => import('./fixtures/fake-runtime'));
beforeEach(() => {
  vi.restoreAllMocks();
  resetFake();
});

it('keeps project input after a backend error and retries with the entered YAML', async () => {
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Projects' }));
  await screen.findByText('Web stack');
  await user.click(screen.getByRole('button', { name: /New project/ }));
  const dialog = screen.getByRole('dialog', { name: 'Create project' });
  await user.type(within(dialog).getByLabelText('Project name'), 'broken');
  await user.type(
    within(dialog).getByLabelText('Compose YAML'),
    'services: test',
  );
  await user.click(
    within(dialog).getByRole('button', { name: 'Create project' }),
  );
  expect(
    await within(dialog).findByText(/Compose validation failed/),
  ).toBeInTheDocument();
  expect(within(dialog).getByLabelText('Compose YAML')).toHaveValue(
    'services: test',
  );
  await user.clear(within(dialog).getByLabelText('Project name'));
  await user.type(within(dialog).getByLabelText('Project name'), 'new-app');
  await user.click(
    within(dialog).getByRole('button', { name: 'Create project' }),
  );
  expect(
    await screen.findByRole('heading', { name: 'new-app' }),
  ).toBeInTheDocument();
  expect(
    requests.filter((r) => r.method === 'project.import').at(-1)?.params,
  ).toMatchObject({
    name: 'new-app',
    engine: 'windows',
    yaml: 'services: test',
  });
});

it('requires explicit confirmation before removing a volume', async () => {
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Volumes' }));
  await screen.findByText('app-data');
  await user.click(screen.getByRole('button', { name: 'Remove' }));
  const dialog = screen.getByRole('dialog', {
    name: 'Remove volume app-data?',
  });
  expect(requests.some((r) => r.method === 'volume.remove')).toBe(false);
  await user.click(
    within(dialog).getByRole('button', { name: 'Remove volume' }),
  );
  expect(await screen.findByText('app-data removed.')).toBeInTheDocument();
  expect(requests.find((r) => r.method === 'volume.remove')).toMatchObject({
    engine: 'windows',
    params: { name: 'app-data' },
  });
});

it('offers an import copy for discovered external projects instead of Compose actions', async () => {
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Projects' }));
  const project = (
    await screen.findByRole('heading', { name: 'legacy' })
  ).closest('article');
  expect(project).not.toBeNull();
  expect(
    within(project!).queryByRole('button', { name: 'Deploy' }),
  ).not.toBeInTheDocument();
  await user.click(
    within(project!).getByRole('button', { name: 'Import Compose' }),
  );
  expect(
    screen.getByRole('dialog', { name: 'Import Compose · legacy' }),
  ).toHaveTextContent('Existing external containers remain separate');
});

it('retries an unavailable Compose file without losing edits to the other engine', async () => {
  failNextProjectRead('linux');
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Projects' }));
  await screen.findByRole('heading', { name: 'Web stack' });
  await user.click(screen.getByRole('button', { name: 'Edit YAML' }));
  const dialog = screen.getByRole('dialog', {
    name: 'Edit Compose · Web stack',
  });
  expect(
    await within(dialog).findByText('linux: engine unavailable'),
  ).toBeInTheDocument();
  const windows = within(dialog).getByLabelText('YAML for windows');
  await user.clear(windows);
  await user.type(windows, 'draft text');
  await user.click(
    within(dialog).getByRole('button', { name: 'Retry unavailable files' }),
  );
  await user.click(
    await within(dialog).findByRole('button', { name: 'linux' }),
  );
  expect(within(dialog).getByLabelText('YAML for linux')).toHaveValue(
    'services:\n  api:\n    image: example/api:latest',
  );
  await user.click(within(dialog).getByRole('button', { name: 'windows' }));
  expect(within(dialog).getByLabelText('YAML for windows')).toHaveValue(
    'draft text',
  );
});

it('reads new events after the controller restarts with a new epoch', async () => {
  const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
  setFakeEvents('epoch-a', [
    {
      sequence: 7,
      time: '2026-01-01T00:00:00Z',
      type: 'test',
      message: 'before restart',
    },
  ]);
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Settings' }));
  expect(await screen.findByText('before restart')).toBeInTheDocument();
  setFakeEvents('epoch-b', [
    {
      sequence: 1,
      time: '2026-01-01T00:01:00Z',
      type: 'test',
      message: 'after restart',
    },
  ]);
  window.dispatchEvent(new Event('focus'));
  expect(await screen.findByText('after restart')).toBeInTheDocument();
  expect(screen.queryByText('before restart')).not.toBeInTheDocument();
  expect(
    requests.filter((r) => r.method === 'system.events').at(-1)?.params,
  ).toMatchObject({ after: 7, epoch: 'epoch-a' });
  focus.mockRestore();
});

it('paginates Recent events and lets the operator choose a larger page', async () => {
  const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
  setFakeEvents(
    'events',
    Array.from({ length: 23 }, (_, index) => ({
      sequence: index + 1,
      time: `2026-01-01T00:${String(index).padStart(2, '0')}:00Z`,
      type: 'operation',
      message: `event ${index + 1}`,
    })),
  );
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Settings' }));
  expect(await screen.findByText('Showing 1–10 of 23')).toBeInTheDocument();
  expect(screen.getByText('event 23')).toBeInTheDocument();
  expect(screen.queryByText('event 13')).not.toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: 'Next' }));
  expect(screen.getByText('Showing 11–20 of 23')).toBeInTheDocument();
  expect(screen.getByText('event 13')).toBeInTheDocument();
  expect(screen.queryByText('event 23')).not.toBeInTheDocument();
  await user.selectOptions(screen.getByLabelText('Rows'), '25');
  expect(screen.getByText('Showing 1–23 of 23')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();
  focus.mockRestore();
});

it('saves refresh and log defaults as local interface preferences', async () => {
  const interval = vi.spyOn(window, 'setInterval');
  const user = userEvent.setup();
  render(<App />);
  await screen.findByText('Controller connected');
  await user.click(screen.getByRole('button', { name: 'Settings' }));
  await user.selectOptions(screen.getByLabelText('Refresh data every'), '15');
  await user.selectOptions(screen.getByLabelText('Default log lines'), '1000');
  await user.click(
    screen.getByLabelText('Include timestamps in container logs by default'),
  );
  await user.click(screen.getByRole('button', { name: 'Save preferences' }));
  expect(
    await screen.findByText('Preferences saved on this device.'),
  ).toBeInTheDocument();
  expect(
    JSON.parse(localStorage.getItem('ucd.ui-preferences.v1') || '{}'),
  ).toEqual({ refreshSeconds: 15, logTail: 1000, logTimestamps: true });
  await waitFor(() =>
    expect(interval.mock.calls.map(([, delay]) => delay)).toContain(15000),
  );
  interval.mockRestore();
});

it('keeps a preference active in the current session when browser storage rejects the save', async () => {
  const write = vi
    .spyOn(Storage.prototype, 'setItem')
    .mockImplementation(() => {
      throw new Error('storage blocked');
    });
  try {
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText('Controller connected');
    await user.click(screen.getByRole('button', { name: 'Settings' }));
    await user.selectOptions(screen.getByLabelText('Refresh data every'), '10');
    await user.click(screen.getByRole('button', { name: 'Save preferences' }));
    expect(
      await screen.findByText(
        'Preferences applied for this session. Browser storage is unavailable.',
      ),
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Refresh data every')).toHaveValue('10');
    expect(
      JSON.parse(localStorage.getItem('ucd.ui-preferences.v1') || '{}'),
    ).toEqual({ refreshSeconds: 5, logTail: 300, logTimestamps: false });
  } finally {
    write.mockRestore();
  }
});

it('updates Windows startup and tray settings through the desktop bridge', async () => {
  vi.stubEnv('DEV', false);
  const transport = document.createElement('meta');
  transport.name = 'ucd-transport';
  transport.content = 'desktop';
  document.head.append(transport);
  try {
    const user = userEvent.setup();
    render(<App />);
    await screen.findByText('Controller connected');
    await user.click(screen.getByRole('button', { name: 'Settings' }));
    const startAtLogin = await screen.findByLabelText(
      'Start Unified Container Desktop when I sign in to Windows',
    );
    const startMinimized = screen.getByLabelText(
      'Start minimized to the notification area',
    );
    expect(startMinimized).toBeDisabled();
    await user.click(startAtLogin);
    await user.click(startMinimized);
    await user.click(
      screen.getByRole('button', { name: 'Save desktop preferences' }),
    );
    expect(
      await screen.findByText('Desktop preferences saved.'),
    ).toBeInTheDocument();
    expect(
      requests.find((request) => request.method === 'desktop.preferences.set')
        ?.params,
    ).toEqual({ startAtLogin: true, startMinimized: true, closeToTray: true });
  } finally {
    transport.remove();
    vi.unstubAllEnvs();
  }
});

it('refreshes access in the open app without asking the user to sign in again', async () => {
  const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
  setFakeRole('viewer');
  render(<App />);
  expect(
    await screen.findByText(
      /Access updates automatically while this app is open/,
    ),
  ).toBeInTheDocument();
  setFakeRole('operator');
  window.dispatchEvent(new Event('focus'));
  await waitFor(() => expect(screen.getByText('operator')).toBeInTheDocument());
  expect(screen.queryByText(/sign out and back in/i)).not.toBeInTheDocument();
  focus.mockRestore();
});

it('refreshes engine and container inventories when the event batch is empty', async () => {
  const focus = vi.spyOn(document, 'hasFocus').mockReturnValue(true);
  setFakeEvents('quiet-controller', []);
  render(<App />);
  await screen.findByText('Controller connected');
  await waitFor(() => {
    expect(requests.some((request) => request.method === 'system.events')).toBe(
      true,
    );
    expect(
      requests.filter((request) => request.method === 'engine.list').length,
    ).toBeGreaterThan(1);
  });
  const engineReads = requests.filter(
    (request) => request.method === 'engine.list',
  ).length;
  const containerReads = requests.filter(
    (request) => request.method === 'container.list',
  ).length;
  window.dispatchEvent(new Event('focus'));
  await waitFor(() => {
    expect(
      requests.filter((request) => request.method === 'engine.list').length,
    ).toBeGreaterThan(engineReads);
    expect(
      requests.filter((request) => request.method === 'container.list').length,
    ).toBeGreaterThan(containerReads);
  });
  focus.mockRestore();
});
