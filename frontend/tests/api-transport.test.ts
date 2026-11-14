import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { call } from '../src/api';

const nativeCall = vi.hoisted(() => vi.fn());
vi.mock('@wailsio/runtime', () => ({ Call: { ByName: nativeCall } }));
const launchToken = 'a'.repeat(43);

function webMode() {
  const meta = document.createElement('meta');
  meta.name = 'ucd-transport';
  meta.content = 'web';
  document.head.append(meta);
}

describe('API transport', () => {
  beforeEach(() => {
    sessionStorage.clear();
    history.replaceState(null, '', '/');
    nativeCall.mockReset();
  });
  afterEach(() => {
    document
      .querySelectorAll('meta[name="ucd-transport"]')
      .forEach((meta) => meta.remove());
    vi.unstubAllGlobals();
  });

  it('sends the launch token and request to the browser endpoint, then reuses it after refresh', async () => {
    webMode();
    history.replaceState(null, '', `/?view=containers#${launchToken}`);
    const fetchMock = vi.fn().mockImplementation(
      async () =>
        new Response(JSON.stringify({ result: { online: true } }), {
          status: 200,
        }),
    );
    vi.stubGlobal('fetch', fetchMock);
    await expect(
      call('engine.status', 'linux', { detail: true }),
    ).resolves.toEqual({ online: true });
    expect(window.location.hash).toBe('');
    expect(window.location.search).toBe('?view=containers');
    expect(sessionStorage.getItem('ucd-web-token')).toBe(launchToken);
    expect(fetchMock).toHaveBeenCalledWith('/v1/call', {
      method: 'POST',
      signal: undefined,
      headers: {
        Authorization: `Bearer ${launchToken}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({
        method: 'engine.status',
        engine: 'linux',
        params: { detail: true },
      }),
    });
    await expect(call('engine.status')).resolves.toEqual({ online: true });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(nativeCall).not.toHaveBeenCalled();
  });

  it('uses the browser transport when the shared bundle also declares desktop', async () => {
    const desktop = document.createElement('meta');
    desktop.name = 'ucd-transport';
    desktop.content = 'desktop';
    document.head.append(desktop);
    webMode();
    sessionStorage.setItem('ucd-web-token', launchToken);
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(JSON.stringify({ result: 'ok' })));
    vi.stubGlobal('fetch', fetchMock);
    await expect(call('system.settings')).resolves.toBe('ok');
    expect(nativeCall).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('preserves the token and ordinary anchor when navigating to main content', async () => {
    webMode();
    sessionStorage.setItem('ucd-web-token', launchToken);
    history.replaceState(null, '', '/#main');
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response('{}', { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(call('engine.list')).resolves.toBeNull();
    expect(window.location.hash).toBe('#main');
    expect(sessionStorage.getItem('ucd-web-token')).toBe(launchToken);
    expect(fetchMock.mock.calls[0]?.[1].headers.Authorization).toBe(
      `Bearer ${launchToken}`,
    );
  });

  it('reports a missing token without calling either transport', async () => {
    webMode();
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    await expect(call('engine.list')).rejects.toThrow(
      'Restart ucd web and open the launch URL',
    );
    expect(fetchMock).not.toHaveBeenCalled();
    expect(nativeCall).not.toHaveBeenCalled();
  });

  it('reports backend errors and HTTP failures', async () => {
    webMode();
    sessionStorage.setItem('ucd-web-token', launchToken);
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error: { code: 'denied', message: 'Access denied by controller' },
          }),
          { status: 403 },
        ),
      )
      .mockResolvedValueOnce(new Response('{}', { status: 503 }))
      .mockResolvedValueOnce(new Response('not JSON', { status: 502 }))
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error: {
              code: 'unauthenticated',
              message: 'valid bearer token required',
            },
          }),
          { status: 401 },
        ),
      );
    vi.stubGlobal('fetch', fetchMock);
    await expect(call('engine.list')).rejects.toThrow(
      'Access denied by controller',
    );
    await expect(call('engine.list')).rejects.toThrow('HTTP 503');
    await expect(call('engine.list')).rejects.toThrow(
      'Invalid JSON response from engine.list (HTTP 502)',
    );
    await expect(call('engine.list')).rejects.toThrow(
      'valid bearer token required. Restart ucd web and open the launch URL.',
    );
  });

  it('uses the native binding when the web marker is absent', async () => {
    nativeCall.mockResolvedValue('{"value":42}');
    await expect(call('test.method', 'windows', { x: 1 })).resolves.toEqual({
      value: 42,
    });
    expect(nativeCall).toHaveBeenCalledWith(
      'main.Bridge.Call',
      'test.method',
      'windows',
      '{"x":1}',
    );
  });

  it('passes browser cancellation to fetch', async () => {
    webMode();
    sessionStorage.setItem('ucd-web-token', launchToken);
    const controller = new AbortController();
    const fetchMock = vi.fn(
      (_url: string, options: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          options.signal?.addEventListener(
            'abort',
            () => reject(options.signal?.reason),
            { once: true },
          );
        }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const pending = call(
      'transfer.read',
      'linux',
      { sessionID: 'transfer' },
      { signal: controller.signal },
    );
    const rejected = expect(pending).rejects.toThrow('cancel transfer');
    controller.abort(new Error('cancel transfer'));
    await rejected;
    expect(fetchMock.mock.calls[0]?.[1].signal).toBe(controller.signal);
  });

  it('cancels the native request and detaches cancellation after completion', async () => {
    const controller = new AbortController();
    let rejectRequest: (reason: unknown) => void = () => {};
    const cancel = vi.fn(async () => {
      rejectRequest(new Error('native request canceled'));
    });
    const nativePending = Object.assign(
      new Promise<string>((_resolve, reject) => {
        rejectRequest = reject;
      }),
      { cancel },
    );
    nativeCall.mockReturnValue(nativePending);
    const pending = call(
      'transfer.read',
      'windows',
      {},
      { signal: controller.signal },
    );
    const rejected = expect(pending).rejects.toThrow('native request canceled');
    await vi.waitFor(() => expect(nativeCall).toHaveBeenCalledOnce());
    controller.abort();
    await rejected;
    expect(cancel).toHaveBeenCalledOnce();

    const completedController = new AbortController();
    const completedCancel = vi.fn();
    nativeCall.mockReturnValue(
      Object.assign(Promise.resolve('{}'), { cancel: completedCancel }),
    );
    await call('engine.list', '', {}, { signal: completedController.signal });
    completedController.abort();
    expect(completedCancel).not.toHaveBeenCalled();
  });

  it('does not start an already canceled request', async () => {
    const controller = new AbortController();
    controller.abort(new Error('already canceled'));
    await expect(
      call('image.build', 'linux', {}, { signal: controller.signal }),
    ).rejects.toThrow('already canceled');
    expect(nativeCall).not.toHaveBeenCalled();
  });

  it('retains native response validation and errors', async () => {
    nativeCall
      .mockResolvedValueOnce(42)
      .mockResolvedValueOnce('invalid')
      .mockRejectedValueOnce(new Error('controller unavailable'));
    await expect(call('test')).rejects.toThrow('expected JSON text');
    await expect(call('test')).rejects.toThrow('Invalid JSON response');
    await expect(call('test')).rejects.toThrow('controller unavailable');
  });
});
