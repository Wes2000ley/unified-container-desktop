type Params = Record<string, unknown>;
import { savePreferences } from '../../src/preferences';
type TestOptions = {
  role?: string;
  failOnce?: Record<string, string>;
  cancelDownload?: boolean;
  unpreparedWindowsFiles?: boolean;
  linuxOnline?: boolean;
};
type Entry = {
  name: string;
  path: string;
  type: string;
  size: number;
  modified: string;
};
type StoredFile = { type: 'file' | 'directory'; data: string };
type Session = { chunks: string[]; index: number };
type TransferSession = {
  engine: string;
  operation: string;
  params: Params;
  chunks: string[];
  data: string;
  offset: number;
};
type FixtureProject = {
  id: string;
  name: string;
  parts: {
    engine: string;
    composeFile: string;
    directory: string;
    name: string;
  }[];
  startupOrder?: string[];
  discovered?: boolean;
};
let projects: FixtureProject[];
let files: Record<string, string>;
let volumes: {
  name: string;
  engine: string;
  driver: string;
  mountpoint: string;
  usageCount: number;
  size: number;
}[];
export let requests: { method: string; engine: string; params: Params }[];
let eventEpoch: string;
let events: { sequence: number; time: string; type: string; message: string }[];
let failedRead = '';
let options: TestOptions;
let containers: {
  id: string;
  name: string;
  engine: string;
  image: string;
  state: string;
  status: string;
  project?: string;
  ports: {
    hostIP: string;
    published: number;
    target: number;
    protocol: string;
  }[];
  cpuPercent: number;
  memoryBytes: number;
  metricsAvailable: boolean;
}[];
let images: {
  id: string;
  engine: string;
  tags: string[];
  size: number;
  created: number;
  containers: number;
}[];
let networks: {
  Id: string;
  Name: string;
  Driver: string;
  Scope: string;
  Internal: boolean;
  Attachable: boolean;
  engine: string;
}[];
let logins: { server: string; username: string }[];
let desktopPreferences = {
  startAtLogin: false,
  startMinimized: false,
  closeToTray: true,
};
let containerFiles: Record<string, StoredFile>;
let unpreparedWindowsFiles: Set<string>;
let volumeFiles: Record<string, Record<string, StoredFile>>;
let sessions: Record<string, Session>;
let transfers: Record<string, TransferSession>;
let sessionNumber: number;

function textData(value: string): string {
  const bytes = new TextEncoder().encode(value);
  return btoa(String.fromCharCode(...bytes));
}

function archiveData(files: Record<string, StoredFile>): string {
  const encoder = new TextEncoder();
  const items = Object.entries(files);
  const length = items.reduce(
    (total, [, file]) =>
      total + 512 + Math.ceil(atob(file.data).length / 512) * 512,
    1024,
  );
  const archive = new Uint8Array(length);
  const put = (offset: number, value: string) =>
    archive.set(encoder.encode(value), offset);
  const octal = (value: number, width: number) =>
    value.toString(8).padStart(width - 1, '0') + '\0';
  let offset = 0;
  for (const [name, file] of items) {
    const data = Uint8Array.from(atob(file.data), (character) =>
      character.charCodeAt(0),
    );
    put(offset, name);
    put(offset + 100, octal(0o644, 8));
    put(offset + 108, octal(0, 8));
    put(offset + 116, octal(0, 8));
    put(offset + 124, octal(data.length, 12));
    put(offset + 136, octal(0, 12));
    put(offset + 148, '        ');
    put(offset + 156, file.type === 'directory' ? '5' : '0');
    put(offset + 257, 'ustar\0');
    put(offset + 263, '00');
    put(
      offset + 148,
      octal(
        archive
          .slice(offset, offset + 512)
          .reduce((sum, byte) => sum + byte, 0),
        8,
      ),
    );
    archive.set(data, offset + 512);
    offset += 512 + Math.ceil(data.length / 512) * 512;
  }
  let binary = '';
  for (let index = 0; index < archive.length; index += 0x8000)
    binary += String.fromCharCode(...archive.subarray(index, index + 0x8000));
  return btoa(binary);
}

function restoredFiles(encoded: string): Record<string, StoredFile> {
  const bytes = Uint8Array.from(atob(encoded), (character) =>
    character.charCodeAt(0),
  );
  if (bytes.length < 1024 || bytes.length % 512 !== 0)
    throw new Error('invalid backup archive');
  const decoder = new TextDecoder();
  const files: Record<string, StoredFile> = {};
  for (let offset = 0; offset < bytes.length;) {
    const name =
      decoder.decode(bytes.subarray(offset, offset + 100)).split('\0')[0] || '';
    if (!name) break;
    const size = Number.parseInt(
      decoder
        .decode(bytes.subarray(offset + 124, offset + 136))
        .split('\0')[0] || '0',
      8,
    );
    if (!Number.isFinite(size) || offset + 512 + size > bytes.length)
      throw new Error('invalid backup archive');
    const contents = bytes.subarray(offset + 512, offset + 512 + size);
    let binary = '';
    for (let index = 0; index < contents.length; index += 0x8000)
      binary += String.fromCharCode(
        ...contents.subarray(index, index + 0x8000),
      );
    files[name] = {
      type: bytes[offset + 156] === 53 ? 'directory' : 'file',
      data: btoa(binary),
    };
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  return files;
}

function normalized(path: string): string {
  return path.replaceAll('\\', '/').replace(/\/$/, '') || '/';
}

function volumeContent(
  engine: string,
  name: unknown,
): Record<string, StoredFile> {
  const files = volumeFiles[`${engine}:${String(name)}`];
  if (!files) throw new Error('volume not found');
  return files;
}

function addImage(engine: string, tags: string[]) {
  images.push({
    id: `sha256:fixture${images.length + 1}`,
    engine,
    tags,
    size: 1024,
    created: 0,
    containers: 0,
  });
}

function entries(
  files: Record<string, StoredFile>,
  directory: string,
): Entry[] {
  const parent = normalized(directory);
  return Object.entries(files)
    .filter(([path]) => {
      const slash = path.lastIndexOf('/');
      return (slash < 0 ? '.' : path.slice(0, slash) || '/') === parent;
    })
    .map(([path, file]) => ({
      name: path.slice(path.lastIndexOf('/') + 1),
      path,
      type: file.type,
      size: file.type === 'file' ? atob(file.data).length : 0,
      modified: '2026-11-13T00:00:00Z',
    }));
}
export function failNextProjectRead(engine: string) {
  failedRead = engine;
}
export function setFakeEvents(epoch: string, next: typeof events) {
  eventEpoch = epoch;
  events = next;
}
export function setFakeRole(role: string) {
  options.role = role;
}
export function resetFake() {
  options =
    typeof window === 'undefined'
      ? {}
      : (window as unknown as { __ucdTestOptions?: TestOptions })
          .__ucdTestOptions || {};
  options = { ...options, failOnce: { ...options.failOnce } };
  projects = [
    {
      id: 'web-stack',
      name: 'Web stack',
      parts: [
        {
          engine: 'windows',
          composeFile:
            'C:\\ProgramData\\UnifiedContainerDesktop\\projects\\web-stack-windows\\compose.yaml',
          directory: '',
          name: 'web-stack-windows',
        },
        {
          engine: 'linux',
          composeFile: '/var/lib/ucd/projects/web-stack-linux/compose.yaml',
          directory: '',
          name: 'web-stack-linux',
        },
      ],
    },
    {
      id: 'discovered-linux-legacy',
      name: 'legacy',
      discovered: true,
      parts: [
        {
          engine: 'linux',
          composeFile: '/external/compose.yaml',
          directory: '/external',
          name: 'legacy',
        },
      ],
    },
  ];
  files = {
    'web-stack:windows': 'services:\n  web:\n    image: example/web:latest',
    'web-stack:linux': 'services:\n  api:\n    image: example/api:latest',
  };
  volumes = [
    {
      name: 'app-data',
      engine: 'windows',
      driver: 'local',
      mountpoint: 'C:\\ProgramData\\Docker\\volumes\\app-data',
      usageCount: 0,
      size: 1024,
    },
  ];
  containers = [
    {
      id: 'abc123',
      name: 'web-service',
      engine: 'windows',
      image: 'example/web:latest',
      state: 'running',
      status: 'Up 2 hours',
      project: 'web-stack',
      ports: [
        { hostIP: '0.0.0.0', published: 8080, target: 80, protocol: 'tcp' },
      ],
      cpuPercent: 1.2,
      memoryBytes: 104857600,
      metricsAvailable: true,
    },
  ];
  if (options.linuxOnline)
    containers.push({
      id: 'linux123',
      name: 'api-service',
      engine: 'linux',
      image: 'example/api:latest',
      state: 'running',
      status: 'Up 2 hours',
      ports: [],
      cpuPercent: 0,
      memoryBytes: 0,
      metricsAvailable: false,
    });
  images = [
    {
      id: 'sha256:12345678901234',
      engine: 'windows',
      tags: ['example/web:latest'],
      size: 104857600,
      created: 0,
      containers: 0,
    },
  ];
  networks = [
    {
      Id: 'net1',
      Name: 'app-net',
      Driver: 'nat',
      Scope: 'local',
      Internal: false,
      Attachable: true,
      engine: 'windows',
    },
  ];
  logins = [];
  desktopPreferences = {
    startAtLogin: false,
    startMinimized: false,
    closeToTray: true,
  };
  savePreferences({ refreshSeconds: 5, logTail: 300, logTimestamps: false });
  containerFiles = {
    'C:/config.txt': { type: 'file', data: textData('old config\n') },
  };
  unpreparedWindowsFiles = new Set(
    options.unpreparedWindowsFiles ? ['abc123'] : [],
  );
  volumeFiles = {
    'windows:app-data': {
      'notes.txt': { type: 'file', data: textData('volume notes\n') },
    },
  };
  sessions = {};
  transfers = {};
  sessionNumber = 0;
  requests = [];
  if (typeof window !== 'undefined')
    Object.assign(window, { __ucdTestRequests: requests });
  eventEpoch = 'epoch-a';
  events = [];
  failedRead = '';
}
resetFake();
const wsl = {
  distro: 'UCD-Linux',
  serviceAccount: 'UCDService',
  cpus: 4,
  memoryMB: 8192,
  diskGB: 80,
  profileDirectory: 'C:\\ProgramData\\UnifiedContainerDesktop\\profile',
  installDirectory: 'C:\\ProgramData\\UnifiedContainerDesktop\\wsl',
};
const settings = {
  version: 1,
  dataRoot: 'C:\\ProgramData\\UnifiedContainerDesktop',
  windowsEndpoint: '',
  helperURL: '',
  caCert: '',
  clientCert: '',
  clientKey: '',
  wsl,
  projects: [],
  dockerProxy: { http: '', https: '', socks5: '', noProxy: '' },
  containersProxy: { http: '', https: '', socks5: '', noProxy: '' },
  network: {
    dockerSubnet: '',
    defaultNetworkingMode: 'ipv4only',
    dnsInhibition: 'auto',
    defaultPortBinding: 'local',
    hostNetworkingEnabled: false,
  },
  resourceSaver: { enabled: false, idleSeconds: 300 },
  registryMirrors: [],
};
function result(value: unknown) {
  return JSON.stringify(value);
}
function cancellable<T>(
  operation: () => Promise<T>,
): Promise<T> & { cancel: () => Promise<void> } {
  let rejectPending: (reason: Error) => void = () => undefined;
  let settled = false;
  const promise = new Promise<T>((resolve, reject) => {
    rejectPending = reject;
    void operation().then(
      (value) => {
        settled = true;
        resolve(value);
      },
      (error: unknown) => {
        settled = true;
        reject(error);
      },
    );
  });
  return Object.assign(promise, {
    cancel: async () => {
      if (!settled) {
        settled = true;
        const error = new Error('The operation was cancelled');
        error.name = 'CancelError';
        // Wails suppresses an unhandled cancellation while delivering it to callers.
        void promise.catch(() => undefined);
        rejectPending(error);
      }
    },
  });
}

export const Call = {
  ByName: (
    _binding: string,
    method: string,
    engine: string,
    raw: string,
  ): Promise<string> & { cancel: () => Promise<void> } =>
    cancellable(async () => {
      if (_binding === 'main.Bridge.GetDesktopPreferences') {
        requests.push({
          method: 'desktop.preferences.get',
          engine: '',
          params: {},
        });
        return desktopPreferences as unknown as string;
      }
      if (_binding === 'main.Bridge.SetDesktopPreferences') {
        desktopPreferences = method as unknown as typeof desktopPreferences;
        requests.push({
          method: 'desktop.preferences.set',
          engine: '',
          params: desktopPreferences,
        });
        return null as unknown as string;
      }
      if (_binding === 'main.Bridge.Download') {
        const downloadEngine = method;
        const sessionID = engine;
        const name = raw;
        requests.push({
          method: 'bridge.download',
          engine: downloadEngine,
          params: { sessionID, name },
        });
        const transfer = transfers[sessionID];
        if (!transfer || transfer.engine !== downloadEngine)
          throw new Error('transfer not found');
        delete transfers[sessionID];
        if (options.cancelDownload) return '';
        const bytes = Uint8Array.from(atob(transfer.data), (character) =>
          character.charCodeAt(0),
        );
        const url = URL.createObjectURL(new Blob([bytes]));
        const link = document.createElement('a');
        link.href = url;
        link.download = name;
        link.click();
        URL.revokeObjectURL(url);
        return `C:\\test-downloads\\${name}`;
      }
      const params = JSON.parse(raw) as Params;
      requests.push({ method, engine, params });
      const failure = options.failOnce?.[method];
      if (failure) {
        delete options.failOnce?.[method];
        throw new Error(failure);
      }
      if (
        engine === 'windows' &&
        unpreparedWindowsFiles.has(String(params.id)) &&
        ((method.startsWith('container.files.') &&
          method !== 'container.files.prepare') ||
          (method === 'transfer.open' &&
            String(params.operation).startsWith('container.files.')))
      ) {
        throw new Error(
          'start container file utility (use Prepare Files to install it; preparation may restart the container): executable file not found',
        );
      }
      switch (method) {
        case 'transfer.open': {
          const operation = String(params.operation);
          const sessionID = `transfer-${++sessionNumber}`;
          let data = '';
          let name = '';
          if (
            operation === 'container.files.download' ||
            operation === 'container.files.archive'
          ) {
            const filePath = normalized(String(params.path));
            const file = containerFiles[filePath];
            if (!file) throw new Error('file not found');
            data = operation.endsWith('.archive')
              ? archiveData({ [filePath.split('/').at(-1)!]: file })
              : file.data;
            name = `${filePath.split('/').at(-1)}${operation.endsWith('.archive') ? '.tar' : ''}`;
          } else if (operation === 'volume.backup') {
            data = archiveData(volumeContent(engine, params.name));
            name = `${params.name}.tar`;
          } else if (
            operation === 'volume.files.download' ||
            operation === 'volume.files.archive'
          ) {
            const filePath = normalized(String(params.path));
            const file = volumeContent(engine, params.name)[filePath];
            if (!file) throw new Error('file not found');
            data = operation.endsWith('.archive')
              ? archiveData({ [filePath]: file })
              : file.data;
            name = `${filePath.split('/').at(-1)}${operation.endsWith('.archive') ? '.tar' : ''}`;
          } else if (operation === 'image.save') {
            data = archiveData({
              'manifest.json': {
                type: 'file',
                data: textData(
                  JSON.stringify([
                    {
                      Config: 'config.json',
                      RepoTags: params.references || [],
                      Layers: [],
                    },
                  ]),
                ),
              },
              'config.json': {
                type: 'file',
                data: textData(
                  JSON.stringify({
                    architecture: 'amd64',
                    os: engine,
                    rootfs: { type: 'layers', diff_ids: [] },
                  }),
                ),
              },
            });
            name = 'images.tar';
          } else if (operation === 'container.export') {
            data = archiveData({
              'config.txt': containerFiles['C:/config.txt'] || {
                type: 'file',
                data: '',
              },
            });
            name = 'container.tar';
          } else if (
            ![
              'container.files.upload',
              'volume.files.upload',
              'volume.restore',
              'image.load',
              'image.import',
              'image.build',
            ].includes(operation)
          ) {
            throw new Error(`Unexpected test transfer: ${operation}`);
          }
          if (operation === 'volume.restore' && !params.confirmed)
            throw new Error('confirmation required');
          transfers[sessionID] = {
            engine,
            operation,
            params,
            chunks: [],
            data,
            offset: 0,
          };
          return result({
            sessionID,
            name,
            ...(data ? { size: atob(data).length } : {}),
          });
        }
        case 'transfer.write': {
          const transfer = transfers[String(params.sessionID)];
          if (!transfer || transfer.engine !== engine)
            throw new Error('transfer not found');
          const chunk = atob(String(params.data));
          if (chunk.length > 256 * 1024)
            throw new Error('chunk exceeds 262144 bytes');
          transfer.chunks.push(chunk);
          return result(null);
        }
        case 'transfer.read': {
          const transfer = transfers[String(params.sessionID)];
          if (!transfer || transfer.engine !== engine)
            throw new Error('transfer not found');
          const data = atob(transfer.data);
          const chunk = data.slice(
            transfer.offset,
            transfer.offset + Number(params.maxBytes || 65536),
          );
          transfer.offset += chunk.length;
          return result({
            data: btoa(chunk),
            eof: transfer.offset >= data.length,
          });
        }
        case 'transfer.finish': {
          const transfer = transfers[String(params.sessionID)];
          if (!transfer || transfer.engine !== engine)
            throw new Error('transfer not found');
          const failure = options.failOnce?.[transfer.operation];
          if (failure) {
            delete options.failOnce?.[transfer.operation];
            throw new Error(failure);
          }
          const binary = transfer.chunks.join('');
          if (
            transfer.params.size !== undefined &&
            binary.length !== Number(transfer.params.size)
          )
            throw new Error('upload size did not match');
          const data = btoa(binary);
          if (transfer.operation === 'container.files.upload')
            containerFiles[normalized(String(transfer.params.path))] = {
              type: 'file',
              data,
            };
          else if (transfer.operation === 'volume.files.upload')
            volumeContent(engine, transfer.params.name)[
              normalized(String(transfer.params.path))
            ] = { type: 'file', data };
          else if (transfer.operation === 'volume.restore')
            volumeFiles[`${engine}:${String(transfer.params.name)}`] =
              restoredFiles(data);
          else if (transfer.operation === 'image.import') {
            if (!transfer.params.reference)
              throw new Error('target image reference is required');
            restoredFiles(data);
            addImage(engine, [String(transfer.params.reference)]);
          } else if (transfer.operation === 'image.load') {
            const manifestFile = restoredFiles(data)['manifest.json'];
            if (!manifestFile) throw new Error('invalid image archive');
            const manifest: unknown = JSON.parse(atob(manifestFile.data));
            if (!Array.isArray(manifest) || manifest.length === 0)
              throw new Error('invalid image archive');
            const tags: unknown = (manifest[0] as { RepoTags?: unknown })
              .RepoTags;
            addImage(engine, Array.isArray(tags) ? tags.map(String) : []);
          } else if (transfer.operation === 'image.build') {
            const context = restoredFiles(data);
            if (
              !context[String(transfer.params.dockerfilePath || 'Dockerfile')]
            )
              throw new Error('Dockerfile not found in build context');
            addImage(
              engine,
              Array.isArray(transfer.params.tags)
                ? transfer.params.tags.map(String)
                : [],
            );
          }
          return result(
            transfer.operation.startsWith('image.')
              ? { output: `${transfer.operation} completed\n` }
              : null,
          );
        }
        case 'transfer.close':
          delete transfers[String(params.sessionID)];
          return result(null);
        case 'system.settings':
          return result({
            settings,
            principal: {
              sid: 'S-1',
              name: 'Local Admin',
              role: options.role || 'administrator',
            },
          });
        case 'system.events':
          return result({
            epoch: eventEpoch,
            events:
              params.epoch === eventEpoch
                ? events.filter(
                    (event) => event.sequence > Number(params.after),
                  )
                : events,
          });
        case 'system.diagnostics':
          return result({ version: '0.1.0', engines: [] });
        case 'system.configure':
          return result({ restartRequired: true });
        case 'engine.list':
          return result([
            {
              id: 'windows',
              os: 'windows',
              online: true,
              version: '27.0',
              containers: 2,
              running: 1,
              stopped: 1,
              cpus: 8,
              memoryBytes: 17179869184,
            },
            {
              id: 'linux',
              os: 'linux',
              online: !!options.linuxOnline,
              version: options.linuxOnline ? '27.0' : '',
              containers: options.linuxOnline ? 1 : 0,
              running: options.linuxOnline ? 1 : 0,
              stopped: 0,
              cpus: options.linuxOnline ? 4 : 0,
              memoryBytes: options.linuxOnline ? 8589934592 : 0,
              ...(options.linuxOnline
                ? {}
                : { error: 'Linux engine is unavailable' }),
            },
          ]);
        case 'engine.status':
          return result({
            id: engine,
            os: engine,
            online: engine === 'windows' || !!options.linuxOnline,
            containers: 0,
            running: 0,
            stopped: 0,
            cpus: engine === 'linux' ? 4 : 8,
            memoryBytes: 0,
            resourceSaver: false,
            activeOperations: 0,
          });
        case 'engine.start':
        case 'engine.stop':
          return result(null);
        case 'container.list':
          return result({
            items: containers,
            errors: options.linuxOnline
              ? []
              : [{ engine: 'linux', message: 'Linux engine is unavailable' }],
          });
        case 'container.action':
          if (params.action === 'remove')
            containers = containers.filter((item) => item.id !== params.id);
          else {
            const container = containers.find(
              (item) => item.id === params.id && item.engine === engine,
            );
            if (!container) throw new Error('container not found');
            if (
              params.action === 'start' ||
              params.action === 'restart' ||
              params.action === 'unpause'
            ) {
              container.state = 'running';
              container.status = 'Up';
            } else if (params.action === 'stop' || params.action === 'kill') {
              container.state = 'exited';
              container.status = 'Exited';
            } else if (params.action === 'pause') {
              container.state = 'paused';
              container.status = 'Paused';
            }
          }
          return result(null);
        case 'container.logs':
          return result({ text: 'Application started\n' });
        case 'container.logs.open':
        case 'container.exec.open': {
          const sessionID = `session-${++sessionNumber}`;
          sessions[sessionID] = {
            index: 0,
            chunks:
              method === 'container.logs.open'
                ? [
                    textData('first line\n'),
                    textData('burst 1\nburst 2\n'),
                    textData('burst 3\n'),
                  ]
                : [textData('PS C:\\> ')],
          };
          return result({ sessionID });
        }
        case 'container.logs.read':
        case 'container.exec.read':
        case 'project.logs.read': {
          const session = sessions[String(params.sessionID)];
          if (!session) throw new Error('session not found');
          const data = session.chunks[session.index++] || '';
          if (!data) await new Promise((resolve) => setTimeout(resolve, 150));
          return result({ data, eof: false });
        }
        case 'container.logs.close':
        case 'container.exec.close':
        case 'project.logs.close':
          delete sessions[String(params.sessionID)];
          return result(null);
        case 'container.exec.write':
        case 'container.exec.resize':
          if (!sessions[String(params.sessionID)])
            throw new Error('session not found');
          return result(null);
        case 'container.create':
        case 'container.run': {
          const config = params.config as { Image: string };
          const id = `created-${containers.length + 1}`;
          containers.push({
            id,
            name: String(params.name || id),
            engine,
            image: config.Image,
            state: method === 'container.run' ? 'running' : 'created',
            status: method === 'container.run' ? 'Up' : 'Created',
            ports: [],
            cpuPercent: 0,
            memoryBytes: 0,
            metricsAvailable: false,
          });
          return result({ id });
        }
        case 'container.rename': {
          const item = containers.find((value) => value.id === params.id);
          if (!item) throw new Error('container not found');
          item.name = String(params.name);
          return result(null);
        }
        case 'container.update':
          return result({ warnings: [] });
        case 'container.commit':
          return result({ id: 'sha256:committed' });
        case 'container.top':
          return result({
            Titles: ['PID', 'COMMAND'],
            Processes: [['1', 'server']],
          });
        case 'container.diff':
          return result([{ Path: '/config.txt', Kind: 0 }]);
        case 'container.export':
          return result({
            data: textData('container archive'),
            name: 'container.tar',
          });
        case 'container.stats':
          return result({
            metricsAvailable: true,
            cpuPercent: 1.2,
            memoryBytes: 104857600,
            memoryLimitBytes: 536870912,
            networkRxBytes: 2048,
            networkTxBytes: 1024,
            blockReadBytes: 512,
            blockWriteBytes: 256,
            pids: 3,
            sampleTime: '2026-11-13T00:00:00Z',
          });
        case 'container.inspect':
          return result({
            Id: 'abc123',
            Name: '/web-service',
            Config: { Image: 'example/web:latest', Env: ['MODE=test'] },
            State: { Status: 'running' },
            Mounts: [
              {
                Type: 'bind',
                Source: 'C:\\data',
                Destination: 'C:\\app',
                RW: false,
              },
            ],
          });
        case 'container.files.prepare': {
          if (engine !== 'windows')
            throw new Error(
              'file preparation is only required for Windows containers',
            );
          const container = containers.find(
            (item) => item.engine === engine && item.id === params.id,
          );
          if (!container) throw new Error('container not found');
          const restarted = container.state === 'running';
          if (restarted && params.confirmed !== true)
            throw new Error(
              'preparing files requires stopping and restarting this running container',
            );
          unpreparedWindowsFiles.delete(container.id);
          return result({ prepared: true, restarted });
        }
        case 'container.files.list':
          return result({
            entries: entries(containerFiles, String(params.path)),
          });
        case 'container.files.read':
        case 'container.files.download':
        case 'container.files.archive': {
          const filePath = normalized(String(params.path));
          const file = containerFiles[filePath];
          if (!file) throw new Error('file not found');
          return result({ data: file.data, name: filePath.split('/').at(-1) });
        }
        case 'container.files.write':
        case 'container.files.upload':
          containerFiles[normalized(String(params.path))] = {
            type: 'file',
            data: String(params.data),
          };
          return result(null);
        case 'container.files.mkdir':
          containerFiles[normalized(String(params.path))] = {
            type: 'directory',
            data: '',
          };
          return result(null);
        case 'container.files.remove':
          delete containerFiles[normalized(String(params.path))];
          return result(null);
        case 'image.list':
          return result({
            items: images,
            errors: options.linuxOnline
              ? []
              : [{ engine: 'linux', message: 'Linux engine is unavailable' }],
          });
        case 'image.pull':
          if (params.reference === 'broken:latest')
            throw new Error('registry authentication failed');
          addImage(engine, [String(params.reference)]);
          return result(null);
        case 'image.inspect':
          return result({
            Id: 'sha256:12345678901234',
            RepoTags: ['example/web:latest'],
            Architecture: 'amd64',
          });
        case 'image.history':
          return result([
            { Id: 'sha256:layer', CreatedBy: 'COPY app /app', Size: 1024 },
          ]);
        case 'image.tag': {
          const item = images.find(
            (value) =>
              value.engine === engine &&
              (value.id === params.reference ||
                value.tags.includes(String(params.reference))),
          );
          if (!item) throw new Error('image not found');
          item.tags.push(String(params.target));
          return result(null);
        }
        case 'image.push':
        case 'image.load':
        case 'image.import':
          return result({ output: `${method} completed\n` });
        case 'image.build':
          addImage(
            engine,
            Array.isArray(params.tags) ? params.tags.map(String) : [],
          );
          return result({ output: `${method} completed\n` });
        case 'image.save':
          return result({
            data: textData('image archive'),
            name: 'images.tar',
          });
        case 'image.remove':
          images = images.filter(
            (item) => item.id !== params.id || item.engine !== engine,
          );
          return result(null);
        case 'volume.list':
          return result({
            items: volumes,
            errors: options.linuxOnline
              ? []
              : [{ engine: 'linux', message: 'Linux engine is unavailable' }],
          });
        case 'volume.inspect': {
          const volume = volumes.find(
            (v) => v.name === params.name && v.engine === engine,
          );
          if (!volume) throw new Error('volume not found');
          return result({ volume, containers: [] });
        }
        case 'volume.create': {
          const volume = {
            name: String(params.name),
            engine,
            driver: String(params.driver || 'local'),
            mountpoint: '',
            usageCount: 0,
            size: 0,
          };
          volumes.push(volume);
          volumeFiles[`${engine}:${volume.name}`] = {};
          return result(volume);
        }
        case 'volume.remove':
          volumes = volumes.filter(
            (v) => v.name !== params.name || v.engine !== engine,
          );
          delete volumeFiles[`${engine}:${String(params.name)}`];
          return result(null);
        case 'volume.files.list':
          return result({
            entries: entries(
              volumeContent(engine, params.name),
              String(params.path || '.'),
            ),
          });
        case 'volume.files.read':
        case 'volume.files.download':
        case 'volume.files.archive': {
          const filePath = normalized(String(params.path));
          const file = volumeContent(engine, params.name)[filePath];
          if (!file) throw new Error('file not found');
          if (method === 'volume.files.archive')
            return result({
              data: archiveData({ [filePath]: file }),
              name: `${filePath.split('/').at(-1)}.tar`,
            });
          return result({ data: file.data, name: filePath.split('/').at(-1) });
        }
        case 'volume.files.write':
        case 'volume.files.upload':
          volumeContent(engine, params.name)[normalized(String(params.path))] =
            {
              type: 'file',
              data: String(params.data),
            };
          return result(null);
        case 'volume.files.mkdir':
          volumeContent(engine, params.name)[normalized(String(params.path))] =
            {
              type: 'directory',
              data: '',
            };
          return result(null);
        case 'volume.files.remove':
          delete volumeContent(engine, params.name)[
            normalized(String(params.path))
          ];
          return result(null);
        case 'volume.backup':
          return result({
            data: archiveData(volumeContent(engine, params.name)),
            name: `${params.name}.tar`,
          });
        case 'volume.restore':
          if (!params.confirmed) throw new Error('confirmation required');
          volumeContent(engine, params.name);
          volumeFiles[`${engine}:${String(params.name)}`] = restoredFiles(
            String(params.data),
          );
          return result(null);
        case 'volume.empty':
          if (!params.confirmed) throw new Error('confirmation required');
          volumeContent(engine, params.name);
          volumeFiles[`${engine}:${String(params.name)}`] = {};
          return result(null);
        case 'volume.clone': {
          const source = volumes.find(
            (volume) => volume.name === params.name && volume.engine === engine,
          );
          if (!source) throw new Error('volume not found');
          if (
            volumes.some(
              (volume) =>
                volume.name === params.target && volume.engine === engine,
            )
          )
            throw new Error('target volume already exists');
          const copy = { ...source, name: String(params.target) };
          volumes.push(copy);
          volumeFiles[`${engine}:${copy.name}`] = structuredClone(
            volumeContent(engine, params.name),
          );
          return result(copy);
        }
        case 'network.list':
          return result(
            networks.filter((network) => network.engine === engine),
          );
        case 'network.create': {
          const network = {
            Id: `network-${networks.length + 1}`,
            Name: String(params.name),
            Driver: String(params.driver || 'bridge'),
            Scope: 'local',
            Internal: !!params.internal,
            Attachable: !!params.attachable,
            ...(params.options
              ? { Options: params.options as Record<string, string> }
              : {}),
            engine,
          };
          networks.push(network);
          return result({ id: network.Id, warnings: '' });
        }
        case 'network.inspect':
          return result(
            networks.find(
              (network) =>
                network.engine === engine && network.Id === params.id,
            ),
          );
        case 'network.connect':
        case 'network.disconnect':
          return result(null);
        case 'network.remove':
          networks = networks.filter(
            (network) => network.engine !== engine || network.Id !== params.id,
          );
          return result(null);
        case 'registry.list':
          return result(logins);
        case 'registry.login':
          logins = [
            ...logins.filter((login) => login.server !== params.server),
            {
              server: String(params.server),
              username: String(params.username),
            },
          ];
          return result(null);
        case 'registry.logout':
          logins = logins.filter((login) => login.server !== params.server);
          return result(null);
        case 'storage.df': {
          const usage = {
            TotalCount: 3,
            ActiveCount: 1,
            TotalSize: 4096,
            Reclaimable: 2048,
          };
          return result({
            Containers: usage,
            Images: usage,
            Volumes: usage,
            BuildCache: usage,
          });
        }
        case 'storage.preview':
          return result({
            scope: params.scope,
            all: !!params.all,
            totalCount: 3,
            activeCount: 1,
            reclaimable: 2048,
            description:
              'Remove the selected unused resources. This is an aggregate estimate.',
          });
        case 'storage.prune':
          if (!params.confirmed) throw new Error('confirmation required');
          return result({ SpaceReclaimed: 2048 });
        case 'project.list':
          return result(projects);
        case 'project.logs.open': {
          const partEngine = String(params.engine);
          if (partEngine === 'linux')
            return result({
              projectID: params.id,
              success: false,
              parts: [{ engine: partEngine, error: 'engine unavailable' }],
            });
          const sessionID = `session-${++sessionNumber}`;
          sessions[sessionID] = {
            index: 0,
            chunks: [textData('web | ready\n'), textData('web | processing\n')],
          };
          return result({
            projectID: params.id,
            success: true,
            parts: [{ engine: partEngine, result: { sessionID } }],
          });
        }
        case 'project.import': {
          if (params.name === 'broken')
            throw new Error('Compose validation failed: invalid service');
          const name = String(params.name);
          const id = String(
            params.id || name.toLowerCase().replace(/[^a-z0-9]+/g, '-'),
          );
          let project = projects.find((p) => p.id === id);
          if (!project) {
            project = { id, name, parts: [] };
            projects.push(project);
          }
          const partEngine = String(params.engine);
          project.parts.push({
            engine: partEngine,
            composeFile: `${id}/${partEngine}/compose.yaml`,
            directory: '',
            name: `${id}-${partEngine}`,
          });
          files[`${id}:${partEngine}`] = String(params.yaml);
          return result(project);
        }
        case 'project.read':
          if (params.engine === failedRead) {
            failedRead = '';
            throw new Error('engine unavailable');
          }
          return result(
            projects
              .find((p) => p.id === params.id)
              ?.parts.filter(
                (part) => !params.engine || part.engine === params.engine,
              )
              .map((part) => ({
                engine: part.engine,
                yaml: files[`${params.id}:${part.engine}`],
              })) || [],
          );
        case 'project.write':
          files[`${params.id}:${params.engine}`] = String(params.yaml);
          return result(null);
        case 'project.action':
          return result({
            projectID: params.id,
            success: false,
            parts: [
              { engine: 'windows', result: { text: 'completed' } },
              { engine: 'linux', error: 'engine unavailable' },
            ],
          });
        case 'wsl.status':
          return result({
            distro: 'UCD-Linux',
            running: true,
            cpus: 4,
            memoryMB: 8192,
            diskGB: 80,
            memoryUsedMB: 1024,
            diskUsedBytes: 10737418240,
            restartRequired: false,
          });
        case 'wsl.configure':
          return result({ restartRequired: true });
        case 'operator.list':
          return result(['DOMAIN\\operator']);
        case 'operator.add':
        case 'operator.remove':
          return result(null);
        case 'share.list':
          return result([]);
        case 'share.create':
        case 'share.remove':
          return result(null);
        default:
          throw new Error(`Unexpected test call: ${method}`);
      }
    }),
};
