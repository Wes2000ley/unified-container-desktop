import { call, errorText, isWebTransport } from './api';
import type { Engine } from './api';

const chunkSize = 256 * 1024;
type OpenResult = { sessionID: string; name: string; size?: number };
type TransferOptions = {
  signal?: AbortSignal;
  onProgress?: (sent: number, total: number) => void;
};

function base64(bytes: Uint8Array): string {
  let binary = '';
  for (let offset = 0; offset < bytes.length; offset += 0x8000)
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  return btoa(binary);
}

async function closeAfterFailure(
  engine: Engine,
  sessionID: string,
  original: unknown,
) {
  try {
    await call('transfer.close', engine, { sessionID });
  } catch (error) {
    throw new AggregateError(
      [original, error],
      `${errorText(original)}; closing the transfer also failed: ${errorText(error)}`,
      { cause: error },
    );
  }
}

export async function uploadTransfer(
  engine: Engine,
  operation: string,
  params: Record<string, unknown>,
  file: File,
  options: TransferOptions = {},
): Promise<unknown> {
  let offset = 0;
  const stream = new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (offset >= file.size) {
        controller.close();
        return;
      }
      const bytes = new Uint8Array(
        await file.slice(offset, offset + chunkSize).arrayBuffer(),
      );
      offset += bytes.length;
      controller.enqueue(bytes);
    },
  });
  return uploadStreamTransfer(
    engine,
    operation,
    params,
    stream,
    file.size,
    options,
  );
}

export async function uploadStreamTransfer(
  engine: Engine,
  operation: string,
  params: Record<string, unknown>,
  stream: ReadableStream<Uint8Array>,
  total?: number,
  options: TransferOptions = {},
): Promise<unknown> {
  let session: OpenResult;
  try {
    options.signal?.throwIfAborted();
    session = await call<OpenResult>(
      'transfer.open',
      engine,
      { ...params, operation, ...(total === undefined ? {} : { size: total }) },
      { signal: options.signal },
    );
  } catch (error) {
    await stream.cancel(error).catch(() => undefined);
    throw error;
  }
  const reader = stream.getReader();
  const cancelRead = () => {
    void reader.cancel(options.signal?.reason).catch(() => undefined);
  };
  options.signal?.addEventListener('abort', cancelRead, { once: true });
  let result: unknown;
  try {
    const buffer = new Uint8Array(chunkSize);
    let buffered = 0;
    let sent = 0;
    const flush = async () => {
      if (!buffered) return;
      options.signal?.throwIfAborted();
      await call(
        'transfer.write',
        engine,
        {
          sessionID: session.sessionID,
          data: base64(buffer.subarray(0, buffered)),
        },
        { signal: options.signal },
      );
      sent += buffered;
      buffered = 0;
      options.onProgress?.(sent, total ?? 0);
    };
    options.onProgress?.(0, total ?? 0);
    while (true) {
      options.signal?.throwIfAborted();
      const { done, value } = await reader.read();
      options.signal?.throwIfAborted();
      if (done) break;
      for (let offset = 0; offset < value.length;) {
        const length = Math.min(chunkSize - buffered, value.length - offset);
        buffer.set(value.subarray(offset, offset + length), buffered);
        buffered += length;
        offset += length;
        if (buffered === chunkSize) await flush();
      }
    }
    await flush();
    result = await call(
      'transfer.finish',
      engine,
      { sessionID: session.sessionID },
      { signal: options.signal },
    );
  } catch (error) {
    await reader.cancel(error).catch(() => undefined);
    await closeAfterFailure(engine, session.sessionID, error);
    throw error;
  } finally {
    options.signal?.removeEventListener('abort', cancelRead);
    reader.releaseLock();
  }
  await call('transfer.close', engine, { sessionID: session.sessionID });
  return result;
}

export async function downloadTransfer(
  engine: Engine,
  operation: string,
  params: Record<string, unknown>,
  filename: string,
): Promise<boolean> {
  const session = await call<OpenResult>('transfer.open', engine, {
    ...params,
    operation,
  });
  let ownedByDownload = false;
  try {
    const name = filename || session.name;
    if (isWebTransport()) {
      const token = sessionStorage.getItem('ucd-web-token');
      if (!token)
        throw new Error(
          'Web session token is missing. Restart ucd web and open the launch URL.',
        );
      const response = await fetch('/v1/download', {
        method: 'POST',
        headers: {
          Authorization: `Bearer ${token}`,
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ engine, sessionID: session.sessionID, name }),
      });
      if (!response.ok)
        throw new Error(
          `Preparing the download failed (HTTP ${response.status}).`,
        );
      const result: unknown = await response.json();
      if (
        !result ||
        typeof result !== 'object' ||
        !('url' in result) ||
        typeof result.url !== 'string' ||
        !/^\/v1\/download\/[A-Za-z0-9_-]+$/.test(result.url)
      )
        throw new Error('Invalid download URL from the controller.');
      const link = document.createElement('a');
      link.href = result.url;
      link.download = name;
      link.click();
      ownedByDownload = true;
      return true;
    }
    const { Call } = await import('@wailsio/runtime');
    const pending = Call.ByName(
      'main.Bridge.Download',
      engine,
      session.sessionID,
      name,
    );
    ownedByDownload = true;
    let selected: unknown;
    try {
      selected = await pending;
    } catch (error) {
      try {
        await call('transfer.close', engine, { sessionID: session.sessionID });
      } catch {
        /* The bridge may already have closed it. */
      }
      throw error;
    }
    if (typeof selected !== 'string')
      throw new Error('Invalid response from the save dialog.');
    return selected !== '';
  } catch (error) {
    if (!ownedByDownload)
      await closeAfterFailure(engine, session.sessionID, error);
    throw error;
  }
}
