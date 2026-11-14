import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import {
  downloadTransfer,
  uploadStreamTransfer,
  uploadTransfer,
} from '../src/transfers';

const bridge = vi.hoisted(() => vi.fn());
vi.mock('@wailsio/runtime', () => ({ Call: { ByName: bridge } }));

function testFile(size: number): File {
  const bytes = new Uint8Array(size).fill(97);
  const file = new File([bytes], 'archive.tar');
  Object.defineProperty(file, 'slice', {
    value: (start: number, end: number) => ({
      arrayBuffer: async () => bytes.slice(start, end).buffer,
    }),
  });
  return file;
}
function methodCalls() {
  return bridge.mock.calls
    .filter(([name]) => name === 'main.Bridge.Call')
    .map(([, method, engine, raw]) => ({
      method,
      engine,
      params: JSON.parse(raw as string) as Record<string, unknown>,
    }));
}
beforeEach(() => {
  bridge.mockReset();
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  sessionStorage.clear();
  document.querySelector('meta[name="ucd-transport"]')?.remove();
});

it('uploads bounded chunks, finishes, and closes the session', async () => {
  bridge.mockImplementation(async (_name: string, method: string) =>
    JSON.stringify(
      method === 'transfer.open'
        ? { sessionID: 'session', name: 'archive.tar' }
        : method === 'transfer.finish'
          ? { output: 'loaded' }
          : null,
    ),
  );
  const progress: number[] = [];
  const result = await uploadTransfer(
    'linux',
    'image.load',
    {},
    testFile(524289),
    { onProgress: (sent) => progress.push(sent) },
  );
  expect(result).toEqual({ output: 'loaded' });
  const calls = methodCalls();
  expect(calls[0]).toMatchObject({
    method: 'transfer.open',
    engine: 'linux',
    params: { operation: 'image.load', size: 524289 },
  });
  expect(
    calls
      .filter((entry) => entry.method === 'transfer.write')
      .map((entry) => atob(entry.params.data as string).length),
  ).toEqual([262144, 262144, 1]);
  expect(progress).toEqual([0, 262144, 524288, 524289]);
  expect(calls.slice(-2).map((entry) => entry.method)).toEqual([
    'transfer.finish',
    'transfer.close',
  ]);
});

it('closes a failed or canceled upload and preserves the error', async () => {
  bridge.mockImplementation(async (_name: string, method: string) => {
    if (method === 'transfer.open')
      return JSON.stringify({ sessionID: 'session', name: 'archive.tar' });
    if (method === 'transfer.write') throw new Error('write failed');
    return 'null';
  });
  await expect(
    uploadTransfer(
      'windows',
      'volume.restore',
      { name: 'data', confirmed: true },
      testFile(4),
    ),
  ).rejects.toThrow('write failed');
  expect(methodCalls().map((entry) => entry.method)).toEqual([
    'transfer.open',
    'transfer.write',
    'transfer.close',
  ]);
  bridge.mockClear();
  const controller = new AbortController();
  await expect(
    uploadTransfer(
      'windows',
      'volume.restore',
      { name: 'data', confirmed: true },
      testFile(4),
      { signal: controller.signal, onProgress: () => controller.abort() },
    ),
  ).rejects.toMatchObject({ name: 'AbortError' });
  expect(methodCalls().map((entry) => entry.method)).toEqual([
    'transfer.open',
    'transfer.close',
  ]);
});

it('streams a context larger than 8 MiB through bounded chunks', async () => {
  bridge.mockImplementation(async (_name: string, method: string) =>
    JSON.stringify(
      method === 'transfer.open'
        ? { sessionID: 'session', name: 'context.tar' }
        : null,
    ),
  );
  const size = 8 * 1024 * 1024 + 17;
  let remaining = size;
  const stream = new ReadableStream<Uint8Array>({
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
  const progress: number[] = [];
  await uploadStreamTransfer(
    'linux',
    'image.build',
    { dockerfile: 'FROM scratch' },
    stream,
    undefined,
    {
      onProgress: (sent) => progress.push(sent),
    },
  );
  const calls = methodCalls();
  expect(calls[0]).toMatchObject({
    method: 'transfer.open',
    params: { operation: 'image.build' },
  });
  expect(calls[0]!.params).not.toHaveProperty('size');
  const chunks = calls
    .filter((entry) => entry.method === 'transfer.write')
    .map((entry) => atob(entry.params.data as string).length);
  expect(Math.max(...chunks)).toBeLessThanOrEqual(256 * 1024);
  expect(chunks.reduce((sum, chunk) => sum + chunk, 0)).toBe(size);
  expect(progress.at(-1)).toBe(size);
  expect(calls.slice(-2).map((entry) => entry.method)).toEqual([
    'transfer.finish',
    'transfer.close',
  ]);
}, 15000);

it('cancels a pending source read and closes its transfer session', async () => {
  bridge.mockImplementation(async (_name: string, method: string) =>
    JSON.stringify(
      method === 'transfer.open'
        ? { sessionID: 'session', name: 'context.tar' }
        : null,
    ),
  );
  const canceled = vi.fn();
  const stream = new ReadableStream<Uint8Array>({
    pull: () => new Promise(() => {}),
    cancel: canceled,
  });
  const controller = new AbortController();
  const upload = uploadStreamTransfer(
    'linux',
    'image.build',
    {},
    stream,
    undefined,
    { signal: controller.signal },
  );
  await vi.waitFor(() =>
    expect(methodCalls()[0]?.method).toBe('transfer.open'),
  );
  controller.abort();
  await expect(upload).rejects.toMatchObject({ name: 'AbortError' });
  expect(canceled).toHaveBeenCalledOnce();
  expect(methodCalls().map((entry) => entry.method)).toEqual([
    'transfer.open',
    'transfer.close',
  ]);
});

it('treats an empty native save path as cancellation', async () => {
  bridge.mockImplementation(async (name: string) =>
    name === 'main.Bridge.Download'
      ? ''
      : JSON.stringify({ sessionID: 'session', name: 'images.tar' }),
  );
  expect(
    await downloadTransfer(
      'linux',
      'image.save',
      { references: ['app:1'] },
      'images.tar',
    ),
  ).toBe(false);
  expect(
    bridge.mock.calls.find(([name]) => name === 'main.Bridge.Download'),
  ).toEqual(['main.Bridge.Download', 'linux', 'session', 'images.tar']);
  expect(methodCalls().map((entry) => entry.method)).toEqual(['transfer.open']);
});

it('prepares an authenticated one-use browser download without buffering bytes', async () => {
  const meta = document.createElement('meta');
  meta.name = 'ucd-transport';
  meta.content = 'web';
  document.head.append(meta);
  const token = 'a'.repeat(43);
  sessionStorage.setItem('ucd-web-token', token);
  const fetchMock = vi.fn(async (url: string) => ({
    ok: true,
    status: 200,
    json: async () =>
      url === '/v1/call'
        ? { result: { sessionID: 'session', name: 'data.tar' } }
        : { url: '/v1/download/nonce' },
  }));
  vi.stubGlobal('fetch', fetchMock);
  const click = vi
    .spyOn(HTMLAnchorElement.prototype, 'click')
    .mockImplementation(() => {});
  expect(
    await downloadTransfer(
      'windows',
      'volume.backup',
      { name: 'data' },
      'data.tar',
    ),
  ).toBe(true);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  const request = fetchMock.mock.calls[1] as unknown as [string, RequestInit];
  expect(request[0]).toBe('/v1/download');
  expect(request[1].headers).toMatchObject({
    Authorization: `Bearer ${token}`,
  });
  expect(JSON.parse(request[1].body as string)).toEqual({
    engine: 'windows',
    sessionID: 'session',
    name: 'data.tar',
  });
  expect(click).toHaveBeenCalledOnce();
});
