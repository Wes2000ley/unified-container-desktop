export type Engine = 'windows' | 'linux';
export interface EngineStatus {
  id: Engine;
  os: string;
  online: boolean;
  version?: string;
  containers: number;
  running: number;
  stopped: number;
  cpus: number;
  memoryBytes: number;
  error?: string;
  resourceSaver?: boolean;
  activeOperations?: number;
}
export interface Port {
  hostIP: string;
  published: number;
  target: number;
  protocol: string;
}
export interface Container {
  id: string;
  name: string;
  engine: Engine;
  image: string;
  state: string;
  status: string;
  project?: string;
  service?: string;
  ports: Port[];
  cpuPercent: number;
  memoryBytes: number;
  metricsAvailable: boolean;

  memoryLimitBytes?: number;
  networkRxBytes?: number;
  networkTxBytes?: number;
  blockReadBytes?: number;
  blockWriteBytes?: number;
  pids?: number;
  sampleTime?: string;
}
export interface Image {
  id: string;
  engine: Engine;
  tags: string[];
  size: number;
  created: number;
  containers: number;
}
export interface Volume {
  name: string;
  engine: Engine;
  driver: string;
  mountpoint: string;
  labels?: Record<string, string>;
  size: number;
  usageCount: number;
}
export interface ProjectPart {
  engine: Engine;
  composeFile: string;
  directory: string;
  name: string;
}
export interface Project {
  id: string;
  name: string;
  parts: ProjectPart[];
  startupOrder?: string[];
  discovered?: boolean;
}
export interface ProjectFile {
  engine: Engine;
  yaml: string;
}
export interface Principal {
  sid: string;
  name: string;
  role: string;
}
export interface WSLSettings {
  distro: string;
  serviceAccount: string;
  cpus: number;
  memoryMB: number;
  diskGB: number;
  profileDirectory: string;
  installDirectory: string;
}
export interface Settings {
  version: number;
  dataRoot: string;
  windowsEndpoint: string;
  helperURL: string;
  caCert: string;
  clientCert: string;
  clientKey: string;
  wsl: WSLSettings;
  projects: Project[];
  dockerProxy: ProxySettings;
  containersProxy: ProxySettings;
  network: NetworkSettings;
  resourceSaver: ResourceSaverSettings;
  registryMirrors?: string[];
}
export interface ProxySettings {
  http?: string;
  https?: string;
  socks5?: string;
  noProxy?: string;
}
export type DefaultNetworkingMode = 'dual-stack' | 'ipv4only' | 'ipv6only';
export type DNSInhibition = 'auto' | 'none' | 'ipv4' | 'ipv6';
export interface NetworkSettings {
  dockerSubnet?: string;
  defaultNetworkingMode: DefaultNetworkingMode;
  dnsInhibition: DNSInhibition;
  defaultPortBinding: 'local' | 'all';
  hostNetworkingEnabled: boolean;
}
export interface ResourceSaverSettings {
  enabled: boolean;
  idleSeconds: number;
}
export interface WSLStatus {
  distro: string;
  running: boolean;
  cpus: number;
  memoryMB: number;
  diskGB: number;
  memoryUsedMB: number;
  diskUsedBytes: number;
  restartRequired: boolean;
  error?: string;
}
export interface Share {
  name: string;
  windowsPath: string;
  linuxPath: string;
}
export interface Event {
  sequence: number;
  time: string;
  engine?: string;
  project?: string;
  type: string;
  message: string;
}
export interface EventBatch {
  epoch: string;
  events: Event[];
}
export interface ListResult<T> {
  items: T[];
  errors: { engine: string; message: string }[];
}
export interface PartResult {
  engine: string;
  result?: unknown;
  error?: string;
}
export interface OperationResult {
  projectID: string;
  parts: PartResult[];
}

export function isWebTransport(): boolean {
  return Array.from(
    document.querySelectorAll('meta[name="ucd-transport"]'),
  ).some((meta) => meta.getAttribute('content') === 'web');
}

export async function call<T>(
  method: string,
  engine = '',
  params: unknown = {},
  options: { signal?: AbortSignal } = {},
): Promise<T> {
  options.signal?.throwIfAborted();
  if (isWebTransport()) {
    const fragment = window.location.hash.slice(1);
    if (/^[A-Za-z0-9_-]{43}$/.test(fragment)) {
      sessionStorage.setItem('ucd-web-token', fragment);
      history.replaceState(
        null,
        '',
        window.location.pathname + window.location.search,
      );
    }
    const token = sessionStorage.getItem('ucd-web-token');
    if (!token)
      throw new Error(
        'Web session token is missing. Restart ucd web and open the launch URL.',
      );

    const response = await fetch('/v1/call', {
      method: 'POST',
      signal: options.signal,
      headers: {
        Authorization: `Bearer ${token}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ method, engine, params }),
    });
    let envelope: { result?: T; error?: { code: string; message: string } };
    try {
      envelope = (await response.json()) as typeof envelope;
    } catch {
      throw new Error(
        `Invalid JSON response from ${method} (HTTP ${response.status})`,
      );
    }
    if (!envelope || typeof envelope !== 'object' || Array.isArray(envelope))
      throw new Error(`Invalid response from ${method}`);
    if (envelope.error) {
      if (typeof envelope.error.message !== 'string' || !envelope.error.message)
        throw new Error(
          `Invalid error response from ${method} (HTTP ${response.status})`,
        );
      throw new Error(
        response.status === 401
          ? `${envelope.error.message}. Restart ucd web and open the launch URL.`
          : envelope.error.message,
      );
    }
    if (!response.ok)
      throw new Error(`Request ${method} failed (HTTP ${response.status})`);
    return (envelope.result ?? null) as T;
  }

  const { Call } = await import('@wailsio/runtime');
  const pending = Call.ByName(
    'main.Bridge.Call',
    method,
    engine,
    JSON.stringify(params),
  );
  const abort = () => {
    void pending.cancel();
  };
  options.signal?.addEventListener('abort', abort, { once: true });
  let raw: unknown;
  try {
    if (options.signal?.aborted) abort();
    raw = await pending;
  } finally {
    options.signal?.removeEventListener('abort', abort);
  }
  if (typeof raw !== 'string')
    throw new Error(`Invalid response from ${method}: expected JSON text`);
  try {
    return JSON.parse(raw) as T;
  } catch {
    throw new Error(`Invalid JSON response from ${method}`);
  }
}

export function errorText(error: unknown): string {
  if (error instanceof Error) return error.message;
  if (typeof error === 'string') return error;
  return JSON.stringify(error) || 'An unknown error occurred';
}

export function bytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) return '—';
  if (value < 1024) return `${value} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let amount = value / 1024;
  let index = 0;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index++;
  }
  return `${amount.toFixed(amount >= 10 ? 0 : 1)} ${units[index]}`;
}

export function operationErrors(result: OperationResult): string[] {
  return (result.parts || [])
    .filter((part) => part.error)
    .map((part) => `${part.engine}: ${part.error}`);
}
