import { useCallback, useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { bytes, call, errorText } from './api';
import type { Container } from './api';
import { Badge, Confirm, Feedback, Modal, useOperation } from './ui';
import { downloadTransfer, uploadTransfer } from './transfers';
import { usePreferences } from './preferences';
import './container-detail.css';

type DetailProps = {
  container: Container;
  canOperate: boolean;
  onClose: () => void;
  onChanged: () => void;
};
type DetailTab = 'Logs' | 'Inspect' | 'Mounts' | 'Exec' | 'Files' | 'Stats';
const tabs: DetailTab[] = [
  'Logs',
  'Inspect',
  'Mounts',
  'Exec',
  'Files',
  'Stats',
];

export function ContainerDetail({
  container,
  canOperate,
  onClose,
  onChanged,
}: DetailProps) {
  const [tab, setTab] = useState<DetailTab>('Logs');
  const unsavedFile = useRef(false);
  const markUnsaved = useCallback((value: boolean) => {
    unsavedFile.current = value;
  }, []);
  const op = useOperation();
  const state = container.state.toLowerCase();
  const running = state === 'running';
  const paused = state === 'paused';
  function leaveDetail(): boolean {
    return (
      !unsavedFile.current || window.confirm('Discard unsaved file changes?')
    );
  }
  async function action(name: string) {
    const ok = await op.run(
      `${name} ${container.name}`,
      () =>
        call('container.action', container.engine, {
          id: container.id,
          action: name,
        }).then(() => undefined),
      `${container.name}: ${name} completed.`,
    );
    if (ok) onChanged();
  }
  return (
    <div className="container-detail">
      <button
        className="text-button detail-back"
        onClick={() => {
          if (leaveDetail()) onClose();
        }}
      >
        ← All containers
      </button>
      <header className="detail-heading">
        <div>
          <div className="eyebrow">
            CONTAINER · {container.engine.toUpperCase()}
          </div>
          <h1>{container.name}</h1>
          <p className="detail-identity">
            <span className="mono">{container.id.slice(0, 12)}</span> ·{' '}
            {container.image}
          </p>
        </div>
        <div className="detail-heading-actions">
          <Badge tone={running ? 'good' : paused ? 'warn' : 'neutral'}>
            {container.status || container.state}
          </Badge>
          <button
            className="button secondary"
            disabled={!canOperate || !!op.pending}
            onClick={() =>
              void action(running ? 'stop' : paused ? 'unpause' : 'start')
            }
          >
            {running ? 'Stop' : paused ? 'Unpause' : 'Start'}
          </button>
          <button
            className="button secondary"
            disabled={!canOperate || !!op.pending}
            onClick={() => void action('restart')}
          >
            Restart
          </button>
        </div>
      </header>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <nav className="detail-tabs" aria-label="Container detail sections">
        {tabs.map((name) => (
          <button
            key={name}
            className={tab === name ? 'selected' : ''}
            aria-pressed={tab === name}
            onClick={() => {
              if (name === tab || leaveDetail()) setTab(name);
            }}
          >
            {name}
          </button>
        ))}
      </nav>
      <section
        className="panel detail-panel"
        aria-label={`${tab} for ${container.name}`}
      >
        {(tab === 'Inspect' ||
          tab === 'Mounts' ||
          tab === 'Exec' ||
          tab === 'Files') &&
        !canOperate ? (
          <p className="detail-empty">
            Container Operators membership is required for {tab.toLowerCase()}.
          </p>
        ) : (
          <>
            {tab === 'Logs' && <Logs container={container} />}
            {tab === 'Inspect' && <Inspect container={container} />}
            {tab === 'Mounts' && <Mounts container={container} />}
            {tab === 'Exec' && <Exec container={container} />}
            {tab === 'Files' && (
              <Files
                container={container}
                onChanged={onChanged}
                onDirtyChange={markUnsaved}
              />
            )}
            {tab === 'Stats' && <Stats container={container} />}
          </>
        )}
      </section>
    </div>
  );
}

type LogOptions = {
  tail: number;
  timestamps: boolean;
  stream: 'both' | 'stdout' | 'stderr';
};
function Logs({ container }: { container: Container }) {
  const defaults = usePreferences();
  const [options, setOptions] = useState<LogOptions>(() => ({
    tail: defaults.logTail,
    timestamps: defaults.logTimestamps,
    stream: 'both',
  }));
  const [tail, setTail] = useState(String(defaults.logTail));
  const [follow, setFollow] = useState(false);
  const [search, setSearch] = useState('');
  const [regex, setRegex] = useState(false);
  const [text, setText] = useState<string | null>(null);
  const [error, setError] = useState('');
  const [copied, setCopied] = useState(false);
  const [truncated, setTruncated] = useState(false);
  const [streamStatus, setStreamStatus] = useState('');
  const [revision, setRevision] = useState(0);
  const output = useRef<HTMLPreElement>(null);
  useEffect(() => {
    if (follow && output.current)
      output.current.scrollTop = output.current.scrollHeight;
  }, [follow, text]);
  useEffect(() => {
    let active = true;
    let timer: number | undefined;
    let sessionID = '';
    const controller = new AbortController();
    const params = {
      id: container.id,
      tail: options.tail,
      timestamps: options.timestamps,
      stdout: options.stream !== 'stderr',
      stderr: options.stream !== 'stdout',
    };
    const decoder = new TextDecoder();
    function append(chunk: string) {
      if (!chunk) return;
      setText((previous) => {
        const combined = (previous || '') + chunk;
        return combined.length > 8_000_000
          ? combined.slice(-8_000_000)
          : combined;
      });
    }
    let characters = 0;
    async function read(id: string) {
      try {
        const result = await call<{ data: string; eof: boolean }>(
          'container.logs.read',
          container.engine,
          { sessionID: id },
          { signal: controller.signal },
        );
        if (!active) return;
        const chunk = decoder.decode(fromBase64(result.data || ''), {
          stream: !result.eof,
        });
        characters += chunk.length;
        if (characters > 8_000_000) setTruncated(true);
        append(chunk);
        if (result.eof) {
          sessionID = '';
          setStreamStatus('Log stream ended.');
          await call('container.logs.close', container.engine, {
            sessionID: id,
          });
          return;
        }
        timer = window.setTimeout(() => void read(id), result.data ? 0 : 150);
      } catch (reason) {
        if (active) {
          setError(errorText(reason));
          setStreamStatus('');
          if (sessionID) {
            const id = sessionID;
            sessionID = '';
            void call('container.logs.close', container.engine, {
              sessionID: id,
            }).catch(() => undefined);
          }
        }
      }
    }
    async function load() {
      try {
        if (follow) {
          const result = await call<{ sessionID: string }>(
            'container.logs.open',
            container.engine,
            params,
            { signal: controller.signal },
          );
          if (!active) {
            await call('container.logs.close', container.engine, {
              sessionID: result.sessionID,
            });
            return;
          }
          sessionID = result.sessionID;
          setText('');
          setError('');
          setTruncated(false);
          setStreamStatus('Following live output.');
          void read(sessionID);
        } else {
          const result = await call<{ text: string }>(
            'container.logs',
            container.engine,
            params,
            { signal: controller.signal },
          );
          if (active) {
            setText(result.text);
            setError('');
            setTruncated(false);
            setStreamStatus('');
          }
        }
      } catch (reason) {
        if (active) {
          setError(errorText(reason));
          setStreamStatus('');
        }
      }
    }
    void load();
    return () => {
      active = false;
      controller.abort();
      window.clearTimeout(timer);
      if (sessionID)
        void call('container.logs.close', container.engine, {
          sessionID,
        }).catch(() => undefined);
    };
  }, [container.engine, container.id, options, follow, revision]);
  let expression: RegExp | null = null;
  let regexError = '';
  if (search && regex) {
    try {
      expression = new RegExp(search, 'i');
    } catch {
      regexError = 'Enter a valid regular expression.';
    }
  }
  const filtered =
    text === null
      ? ''
      : text
          .split('\n')
          .filter((line) => {
            if (!search || regexError) return true;
            return expression
              ? expression.test(line)
              : line.toLowerCase().includes(search.toLowerCase());
          })
          .join('\n');
  function apply(event: FormEvent) {
    event.preventDefault();
    const value = Number(tail);
    if (Number.isInteger(value) && value >= 1 && value <= 100000)
      setOptions((previous) => ({ ...previous, tail: value }));
  }
  async function copy() {
    try {
      await navigator.clipboard.writeText(filtered);
      setCopied(true);
      setError('');
    } catch (reason) {
      setError(`Copy logs: ${errorText(reason)}`);
    }
  }
  return (
    <>
      <div className="detail-section-heading">
        <div>
          <h2>Logs</h2>
          <p>
            Show recent lines or follow the live output stream. TTY output may
            combine stdout and stderr.
          </p>
        </div>
      </div>
      <Feedback error={error} success={copied ? 'Logs copied.' : ''} />
      {streamStatus && (
        <p className="detail-stream-status" role="status">
          {streamStatus}
        </p>
      )}
      {truncated && (
        <p className="detail-field-error" role="status">
          The live view retains the latest 8 million characters. Earlier output
          is no longer in this view.
        </p>
      )}
      <form className="detail-toolbar" onSubmit={apply}>
        <label>
          Tail lines
          <input
            type="number"
            min="1"
            max="100000"
            required
            value={tail}
            onChange={(e) => setTail(e.target.value)}
          />
        </label>
        <label>
          Stream
          <select
            value={options.stream}
            onChange={(e) =>
              setOptions((value) => ({
                ...value,
                stream: e.target.value as LogOptions['stream'],
              }))
            }
          >
            <option value="both">stdout and stderr</option>
            <option value="stdout">stdout</option>
            <option value="stderr">stderr</option>
          </select>
        </label>
        <label className="detail-check">
          <input
            type="checkbox"
            checked={options.timestamps}
            onChange={(e) =>
              setOptions((value) => ({
                ...value,
                timestamps: e.target.checked,
              }))
            }
          />{' '}
          Timestamps
        </label>
        <label className="detail-check">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => {
              setFollow(e.target.checked);
              if (!e.target.checked) setStreamStatus('');
            }}
          />{' '}
          Follow
        </label>
        <button className="button secondary">Apply</button>
        <button
          type="button"
          className="button secondary"
          onClick={() => setRevision((value) => value + 1)}
        >
          Refresh
        </button>
      </form>
      <div className="detail-toolbar detail-log-filter">
        <label>
          Filter lines
          <input
            placeholder="Search logs"
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setCopied(false);
            }}
            aria-invalid={!!regexError}
          />
        </label>
        <label className="detail-check">
          <input
            type="checkbox"
            checked={regex}
            onChange={(e) => setRegex(e.target.checked)}
          />{' '}
          Regular expression
        </label>
        <div className="detail-spacer" />
        <button
          className="button secondary"
          disabled={text === null || !!regexError}
          onClick={() => void copy()}
        >
          Copy
        </button>
        <button
          className="button secondary"
          disabled={text === null || !!regexError}
          onClick={() =>
            downloadSmallFile(
              new TextEncoder().encode(filtered),
              `${container.name || 'container'}-logs.txt`,
              'text/plain',
            )
          }
        >
          Download
        </button>
      </div>
      {regexError && (
        <p className="detail-field-error" role="alert">
          {regexError}
        </p>
      )}
      {text === null ? (
        !error && (
          <p className="loading" role="status">
            Loading logs…
          </p>
        )
      ) : (
        <pre ref={output} className="detail-log-output" tabIndex={0}>
          {filtered || 'No log lines to display.'}
        </pre>
      )}
    </>
  );
}

type JsonObject = Record<string, unknown>;
function object(value: unknown): JsonObject {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? (value as JsonObject)
    : {};
}
function jsonValue(value: unknown): string {
  if (value === undefined || value === null) return '—';
  return typeof value === 'string' ? value : JSON.stringify(value, null, 2);
}
function useInspect(container: Container) {
  const [state, setState] = useState<{ data?: JsonObject; error: string }>({
    error: '',
  });
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let active = true;
    call<JsonObject>('container.inspect', container.engine, {
      id: container.id,
    }).then(
      (data) => {
        if (active) setState({ data, error: '' });
      },
      (reason) => {
        if (active) setState({ error: errorText(reason) });
      },
    );
    return () => {
      active = false;
    };
  }, [container.engine, container.id, revision]);
  return { ...state, refresh: () => setRevision((value) => value + 1) };
}
function Inspect({ container }: { container: Container }) {
  const inspect = useInspect(container);
  const [raw, setRaw] = useState(false);
  const data = inspect.data;
  const config = object(data?.Config);
  const host = object(data?.HostConfig);
  const network = object(data?.NetworkSettings);
  const sections: [string, [string, unknown][]][] = [
    [
      'Platform',
      [
        ['Platform', data?.Platform],
        ['Container ID', data?.Id],
        ['Name', data?.Name],
        ['Created', data?.Created],
      ],
    ],
    [
      'Command',
      [
        ['Entrypoint', config.Entrypoint],
        ['Command', config.Cmd],
        ['Working directory', config.WorkingDir],
        ['User', config.User],
      ],
    ],
    ['State', [['State', data?.State]]],
    [
      'Image',
      [
        ['Configured image', config.Image],
        ['Image ID', data?.Image],
      ],
    ],
    [
      'Port bindings',
      [
        ['Configured bindings', host.PortBindings],
        ['Published ports', network.Ports],
      ],
    ],
    [
      'Runtime',
      [
        ['Runtime', host.Runtime],
        ['Restart policy', host.RestartPolicy],
        ['Network mode', host.NetworkMode],
        ['Privileged', host.Privileged],
      ],
    ],
    ['Mounts', [['Mounts', data?.Mounts]]],
    ['Volumes', [['Declared volumes', config.Volumes]]],
    ['Environment', [['Environment', config.Env]]],
    ['Labels', [['Labels', config.Labels]]],
    ['Networks', [['Networks', network.Networks]]],
  ];
  return (
    <>
      <div className="detail-section-heading">
        <div>
          <h2>Inspect</h2>
          <p>Configuration and state returned by the container engine.</p>
        </div>
        <div className="detail-inline">
          <button className="button secondary" onClick={() => setRaw(!raw)}>
            {raw ? 'Structured view' : 'Raw JSON'}
          </button>
          <button className="button secondary" onClick={inspect.refresh}>
            Refresh
          </button>
        </div>
      </div>
      <Feedback error={inspect.error} />
      {!data && !inspect.error && (
        <p className="loading" role="status">
          Loading inspection…
        </p>
      )}
      {data &&
        (raw ? (
          <pre className="detail-json">{JSON.stringify(data, null, 2)}</pre>
        ) : (
          <div className="inspect-grid">
            {sections.map(([title, fields]) => (
              <section className="inspect-section" key={title}>
                <h3>{title}</h3>
                <dl>
                  {fields.map(([label, value]) => (
                    <div key={label}>
                      <dt>{label}</dt>
                      <dd>
                        <pre>{jsonValue(value)}</pre>
                      </dd>
                    </div>
                  ))}
                </dl>
              </section>
            ))}
          </div>
        ))}
    </>
  );
}
function Mounts({ container }: { container: Container }) {
  const inspect = useInspect(container);
  const mounts = Array.isArray(inspect.data?.Mounts)
    ? inspect.data.Mounts.map(object)
    : [];
  return (
    <>
      <div className="detail-section-heading">
        <div>
          <h2>Mounts</h2>
          <p>Bind mounts and volumes attached to this container.</p>
        </div>
        <button className="button secondary" onClick={inspect.refresh}>
          Refresh
        </button>
      </div>
      <Feedback error={inspect.error} />
      {!inspect.data && !inspect.error && (
        <p className="loading" role="status">
          Loading mounts…
        </p>
      )}
      {inspect.data &&
        (mounts.length ? (
          <div
            className="table-scroll"
            role="region"
            aria-label="Container mounts"
            tabIndex={0}
          >
            <table>
              <thead>
                <tr>
                  <th>Type</th>
                  <th>Source</th>
                  <th>Destination</th>
                  <th>Mode</th>
                  <th>Access</th>
                  <th>Propagation</th>
                </tr>
              </thead>
              <tbody>
                {mounts.map((mount, index) => (
                  <tr key={`${jsonValue(mount.Destination)}-${index}`}>
                    <td>{jsonValue(mount.Type)}</td>
                    <td className="mono">{jsonValue(mount.Source)}</td>
                    <td className="mono">{jsonValue(mount.Destination)}</td>
                    <td>{jsonValue(mount.Mode)}</td>
                    <td>
                      {mount.RW === true
                        ? 'Read / write'
                        : mount.RW === false
                          ? 'Read only'
                          : '—'}
                    </td>
                    <td>{jsonValue(mount.Propagation)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="detail-empty">No mounts reported.</p>
        ))}
    </>
  );
}

function fromBase64(value: string): Uint8Array {
  const decoded = atob(value);
  const result = new Uint8Array(decoded.length);
  for (let index = 0; index < decoded.length; index++)
    result[index] = decoded.charCodeAt(index);
  return result;
}
function toBase64(value: Uint8Array): string {
  let binary = '';
  for (let index = 0; index < value.length; index += 0x8000)
    binary += String.fromCharCode(...value.subarray(index, index + 0x8000));
  return btoa(binary);
}
function downloadSmallFile(
  data: Uint8Array,
  name: string,
  type = 'application/octet-stream',
) {
  const url = URL.createObjectURL(new Blob([new Uint8Array(data)], { type }));
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}

function Exec({ container }: { container: Container }) {
  const [command, setCommand] = useState(
    container.engine === 'windows' ? 'cmd.exe' : '/bin/sh',
  );
  const [args, setArgs] = useState('');
  const [user, setUser] = useState('');
  const [workdir, setWorkdir] = useState('');
  const [env, setEnv] = useState('');
  const [session, setSession] = useState('');
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const [status, setStatus] = useState('Disconnected');
  const element = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const sessionRef = useRef('');
  const active = useRef(false);
  const controller = useRef<AbortController | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const writes = useRef<Promise<void>>(Promise.resolve());
  const engine = container.engine;
  useEffect(() => {
    active.current = true;
    controller.current = new AbortController();
    const palette = getComputedStyle(document.documentElement);
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: 'Consolas, monospace',
      fontSize: 13,
      theme: {
        background: palette.getPropertyValue('--surface-input').trim(),
        foreground: palette.getPropertyValue('--text').trim(),
        cursor: palette.getPropertyValue('--accent').trim(),
      },
      scrollback: 10000,
    });
    const addon = new FitAddon();
    term.loadAddon(addon);
    if (element.current) {
      term.open(element.current);
      addon.fit();
    }
    terminal.current = term;
    fit.current = addon;
    const input = term.onData((data) => {
      const id = sessionRef.current;
      if (!id) return;
      writes.current = writes.current
        .then(async () => {
          const dataBytes = new TextEncoder().encode(data);
          for (
            let offset = 0;
            offset < dataBytes.length && sessionRef.current === id;
            offset += 32768
          ) {
            await call(
              'container.exec.write',
              engine,
              {
                sessionID: id,
                data: toBase64(dataBytes.subarray(offset, offset + 32768)),
              },
              { signal: controller.current?.signal },
            );
          }
        })
        .catch((reason) => {
          if (active.current && sessionRef.current === id)
            setError(`Terminal input: ${errorText(reason)}`);
        });
    });
    const resize = () => {
      addon.fit();
      const id = sessionRef.current;
      if (id)
        void call(
          'container.exec.resize',
          engine,
          {
            sessionID: id,
            rows: term.rows,
            cols: term.cols,
          },
          { signal: controller.current?.signal },
        ).catch((reason: unknown) => {
          if (active.current && sessionRef.current === id)
            setError(`Terminal resize: ${errorText(reason)}`);
        });
    };
    const observer = new ResizeObserver(resize);
    if (element.current) observer.observe(element.current);
    return () => {
      active.current = false;
      controller.current?.abort();
      window.clearTimeout(timer.current);
      observer.disconnect();
      input.dispose();
      const id = sessionRef.current;
      sessionRef.current = '';
      if (id)
        void call('container.exec.close', engine, { sessionID: id }).catch(
          () => undefined,
        );
      term.dispose();
      terminal.current = null;
      fit.current = null;
    };
  }, [engine]);
  const read = useCallback(
    async function poll(id: string) {
      try {
        const result = await call<{
          data: string;
          eof: boolean;
          exitCode?: number;
        }>(
          'container.exec.read',
          engine,
          { sessionID: id },
          { signal: controller.current?.signal },
        );
        if (!active.current || sessionRef.current !== id) return;
        if (result.data) terminal.current?.write(fromBase64(result.data));
        if (result.eof) {
          sessionRef.current = '';
          setSession('');
          setStatus(
            result.exitCode === undefined
              ? 'Session ended'
              : `Exited with code ${result.exitCode}`,
          );
          await call('container.exec.close', engine, { sessionID: id }).catch(
            (reason: unknown) => {
              if (active.current)
                setError(`Close terminal: ${errorText(reason)}`);
            },
          );
          return;
        }
        timer.current = window.setTimeout(
          () => void poll(id),
          result.data ? 0 : 150,
        );
      } catch (reason) {
        if (active.current && sessionRef.current === id) {
          sessionRef.current = '';
          setSession('');
          setError(`Terminal output: ${errorText(reason)}`);
          setStatus('Disconnected');
          void call('container.exec.close', engine, { sessionID: id }).catch(
            () => undefined,
          );
        }
      }
    },
    [engine],
  );
  async function connect(event: FormEvent) {
    event.preventDefault();
    if (!command.trim()) return;
    setPending(true);
    setError('');
    setStatus('Connecting');
    try {
      fit.current?.fit();
      const result = await call<{ sessionID: string }>(
        'container.exec.open',
        engine,
        {
          id: container.id,
          command: [
            command.trim(),
            ...args.split('\n').filter((line) => line.length > 0),
          ],
          user: user.trim(),
          workingDir: workdir.trim(),
          env: env.split('\n').filter((line) => line.length > 0),
          rows: terminal.current?.rows,
          cols: terminal.current?.cols,
        },
        { signal: controller.current?.signal },
      );
      if (!active.current) {
        await call('container.exec.close', engine, {
          sessionID: result.sessionID,
        });
        return;
      }
      sessionRef.current = result.sessionID;
      setSession(result.sessionID);
      setStatus('Connected');
      terminal.current?.clear();
      terminal.current?.focus();
      void read(result.sessionID);
    } catch (reason) {
      if (active.current) {
        setError(errorText(reason));
        setStatus('Disconnected');
      }
    } finally {
      if (active.current) setPending(false);
    }
  }
  async function disconnect() {
    const id = sessionRef.current;
    if (!id) return;
    sessionRef.current = '';
    window.clearTimeout(timer.current);
    setSession('');
    setPending(true);
    try {
      await call('container.exec.close', engine, { sessionID: id });
      setStatus('Disconnected');
    } catch (reason) {
      setError(`Disconnect: ${errorText(reason)}`);
      setStatus('Disconnected');
    } finally {
      if (active.current) setPending(false);
    }
  }
  return (
    <>
      <div className="detail-section-heading">
        <div>
          <h2>Exec terminal</h2>
          <p>
            Run an interactive command in the container. Arguments and
            environment entries are one per line.
          </p>
        </div>
        <span className="detail-terminal-status" role="status">
          {status}
        </span>
      </div>
      <Feedback error={error} />
      <form className="exec-form" onSubmit={(event) => void connect(event)}>
        <label>
          Command
          <input
            required
            value={command}
            disabled={!!session || pending}
            onChange={(e) => setCommand(e.target.value)}
          />
        </label>
        <label>
          User
          <input
            value={user}
            disabled={!!session || pending}
            onChange={(e) => setUser(e.target.value)}
            placeholder="Container default"
          />
        </label>
        <label>
          Working directory
          <input
            value={workdir}
            disabled={!!session || pending}
            onChange={(e) => setWorkdir(e.target.value)}
            placeholder="Container default"
          />
        </label>
        <label>
          Arguments
          <textarea
            rows={2}
            value={args}
            disabled={!!session || pending}
            onChange={(e) => setArgs(e.target.value)}
            placeholder="One argument per line"
          />
        </label>
        <label>
          Environment
          <textarea
            rows={2}
            value={env}
            disabled={!!session || pending}
            onChange={(e) => setEnv(e.target.value)}
            placeholder="KEY=value, one per line"
          />
        </label>
        <div className="exec-controls">
          {session ? (
            <button
              type="button"
              className="button secondary"
              disabled={pending}
              onClick={() => void disconnect()}
            >
              Disconnect
            </button>
          ) : (
            <button
              className="button primary"
              disabled={pending || !command.trim()}
            >
              Connect
            </button>
          )}
        </div>
      </form>
      <div
        className="detail-terminal"
        ref={element}
        aria-label="Interactive terminal"
      />
    </>
  );
}

type FileEntry = {
  name: string;
  path: string;
  type: string;
  size: number;
  modified: string;
};
type FileWrite = {
  path: string;
  description: string;
  title: string;
  action: string;
} & (
  | { kind: 'edit'; data: string; editedText: string }
  | { kind: 'upload'; file: File }
);
const maxEditorBytes = 8 * 1024 * 1024;
function directoryEntry(entry: FileEntry): boolean {
  return entry.type === 'directory' || entry.type === 'dir';
}
function joinPath(directory: string, name: string, windows: boolean): string {
  const separator = windows ? '\\' : '/';
  return directory.endsWith('/') || directory.endsWith('\\')
    ? directory + name
    : directory + separator + name;
}
function parentPath(path: string, windows: boolean): string {
  const trimmed = path.replace(/[\\/]+$/, '');
  const index = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'));
  if (windows)
    return index < 2 ? `${trimmed.slice(0, 2)}\\` : trimmed.slice(0, index);
  return index <= 0 ? '/' : trimmed.slice(0, index);
}
function validName(name: string): boolean {
  return !!name && name !== '.' && name !== '..' && !/[\\/]/.test(name);
}
function samePath(left: string, right: string, windows: boolean): boolean {
  return windows
    ? left.replaceAll('\\', '/').toLowerCase() ===
        right.replaceAll('\\', '/').toLowerCase()
    : left === right;
}
function Files({
  container,
  onChanged,
  onDirtyChange,
}: {
  container: Container;
  onChanged: () => void;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const windows = container.engine === 'windows';
  const [path, setPath] = useState(windows ? 'C:\\' : '/');
  const [pathInput, setPathInput] = useState(path);
  const [entries, setEntries] = useState<FileEntry[] | null>(null);
  const [listError, setListError] = useState('');
  const [revision, setRevision] = useState(0);
  const [directory, setDirectory] = useState('');
  const [selected, setSelected] = useState<FileEntry | null>(null);
  const [draft, setDraft] = useState('');
  const [original, setOriginal] = useState('');
  const [write, setWrite] = useState<FileWrite | null>(null);
  const [remove, setRemove] = useState<FileEntry | null>(null);
  const [prepare, setPrepare] = useState(false);
  const [prepareSuccess, setPrepareSuccess] = useState('');
  const [message, setMessage] = useState('');
  const [progress, setProgress] = useState<{
    sent: number;
    total: number;
  } | null>(null);
  const uploadController = useRef<AbortController | null>(null);
  const op = useOperation();
  const engine = container.engine;
  const id = container.id;
  const dirty = selected !== null && draft !== original;
  useEffect(() => {
    onDirtyChange(dirty);
    return () => onDirtyChange(false);
  }, [dirty, onDirtyChange]);
  useEffect(() => () => uploadController.current?.abort(), []);
  useEffect(() => {
    let active = true;
    call<{ entries: FileEntry[] }>('container.files.list', engine, {
      id,
      path,
    }).then(
      (result) => {
        if (active) {
          setEntries(result.entries || []);
          setListError('');
        }
      },
      (reason) => {
        if (active) {
          setEntries(null);
          setListError(errorText(reason));
        }
      },
    );
    return () => {
      active = false;
    };
  }, [engine, id, path, revision]);
  function refresh() {
    setRevision((value) => value + 1);
  }
  async function prepareFileAccess() {
    let restarted = false;
    setPrepareSuccess('');
    const ok = await op.run('Preparing file access', async () => {
      const result = await call<{ prepared: boolean; restarted: boolean }>(
        'container.files.prepare',
        engine,
        { id, confirmed: true },
      );
      if (!result?.prepared)
        throw new Error(
          'The container did not confirm file access preparation.',
        );
      restarted = result.restarted;
    });
    onChanged();
    if (ok) {
      setPrepare(false);
      setEntries(null);
      setListError('');
      setMessage('');
      setPrepareSuccess(
        restarted
          ? 'File access is prepared and the container was restarted.'
          : 'File access is prepared.',
      );
      refresh();
    }
  }
  function leaveDraft(): boolean {
    return (
      !dirty || window.confirm(`Discard unsaved changes to ${selected?.path}?`)
    );
  }
  function navigate(next: string) {
    if (!next.trim() || !leaveDraft()) return;
    setSelected(null);
    setPath(next.trim());
    setPathInput(next.trim());
    setEntries(null);
    setListError('');
    setMessage('');
    if (samePath(next.trim(), path, windows)) refresh();
  }
  async function open(entry: FileEntry) {
    if (directoryEntry(entry)) {
      navigate(entry.path);
      return;
    }
    if (!leaveDraft()) return;
    if (entry.size > maxEditorBytes) {
      op.clear();
      setMessage(
        'Text editing supports files up to 8 MiB. Download this file to view it.',
      );
      return;
    }
    await op.run(`Reading ${entry.path}`, async () => {
      const result = await call<{ data: string }>(
        'container.files.read',
        engine,
        { id, path: entry.path },
      );
      let value: string;
      try {
        value = new TextDecoder('utf-8', { fatal: true }).decode(
          fromBase64(result.data),
        );
      } catch {
        throw new Error(
          'This file is not valid UTF-8 text. Download it to view it.',
        );
      }
      if (value.includes('\0'))
        throw new Error(
          'This file contains binary data. Download it to view it.',
        );
      setSelected(entry);
      setDraft(value);
      setOriginal(value);
      setMessage('');
    });
  }
  async function saveFile(item: FileWrite) {
    const controller = item.kind === 'upload' ? new AbortController() : null;
    if (controller) {
      uploadController.current = controller;
      setProgress({
        sent: 0,
        total: item.kind === 'upload' ? item.file.size : 0,
      });
    }
    const ok = await op.run(
      `${item.kind === 'upload' ? 'Uploading' : 'Writing'} ${item.path}`,
      () =>
        item.kind === 'upload'
          ? uploadTransfer(
              engine,
              'container.files.upload',
              { id, path: item.path },
              item.file,
              {
                signal: controller?.signal,
                onProgress: (sent, total) => setProgress({ sent, total }),
              },
            ).then(() => undefined)
          : call('container.files.write', engine, {
              id,
              path: item.path,
              data: item.data,
            }).then(() => undefined),
      `${item.path} saved.`,
    );
    uploadController.current = null;
    setProgress(null);
    if (ok) {
      if (selected && samePath(selected.path, item.path, windows)) {
        if (item.kind === 'upload') setSelected(null);
        else setOriginal(item.editedText);
      }
      setWrite(null);
      refresh();
      onChanged();
    }
  }
  function upload(file: File) {
    if (!validName(file.name)) {
      setMessage('Choose a file with a valid name.');
      return;
    }
    const target = joinPath(path, file.name, windows);
    const sameFile = selected && samePath(selected.path, target, windows);
    if (sameFile && !leaveDraft()) return;
    const exists = entries?.some((entry) =>
      windows
        ? entry.name.toLowerCase() === file.name.toLowerCase()
        : entry.name === file.name,
    );
    op.clear();
    setMessage('');
    setWrite({
      kind: 'upload',
      path: target,
      file,
      title: exists ? 'Overwrite file?' : 'Upload file?',
      action: exists ? 'Overwrite file' : 'Upload file',
      description: `Upload to ${target}? An existing file at this path, including concurrent changes, will be replaced. ACLs and extended attributes may not be preserved.`,
    });
  }
  function prepareEdit() {
    if (!selected) return;
    const content = new TextEncoder().encode(draft);
    if (content.length > maxEditorBytes) {
      op.clear();
      setMessage(
        'Text editing supports files up to 8 MiB. Use upload for larger files.',
      );
      return;
    }
    setMessage('');
    setWrite({
      kind: 'edit',
      path: selected.path,
      data: toBase64(content),
      editedText: draft,
      title: 'Overwrite file?',
      action: 'Overwrite file',
      description: `Replace the contents of ${selected.path} with your edits? Concurrent changes will be overwritten. ACLs and extended attributes may not be preserved.`,
    });
  }
  async function mkdir(event: FormEvent) {
    event.preventDefault();
    if (!validName(directory)) {
      setMessage('Enter a directory name without path separators.');
      return;
    }
    const target = joinPath(path, directory, windows);
    const ok = await op.run(
      `Creating ${target}`,
      () =>
        call('container.files.mkdir', engine, { id, path: target }).then(
          () => undefined,
        ),
      `${target} created.`,
    );
    if (ok) {
      setDirectory('');
      setMessage('');
      refresh();
      onChanged();
    }
  }
  async function removeEntry(entry: FileEntry) {
    const ok = await op.run(
      `Removing ${entry.path}`,
      () =>
        call('container.files.remove', engine, {
          id,
          path: entry.path,
          recursive: directoryEntry(entry),
        }).then(() => undefined),
      `${entry.path} removed.`,
    );
    if (ok) {
      if (
        selected?.path === entry.path ||
        (selected &&
          directoryEntry(entry) &&
          selected.path.startsWith(joinPath(entry.path, '', windows)))
      )
        setSelected(null);
      setRemove(null);
      refresh();
      onChanged();
    }
  }
  async function downloadEntry(entry: FileEntry) {
    setMessage('');
    await op.run(`Downloading ${entry.path}`, async () => {
      const method =
        entry.type === 'file'
          ? 'container.files.download'
          : 'container.files.archive';
      await downloadTransfer(
        engine,
        method,
        { id, path: entry.path },
        entry.type === 'file' ? entry.name : `${entry.name}.tar`,
      );
    });
  }
  return (
    <>
      <div className="detail-section-heading">
        <div>
          <h2>Files</h2>
          <p>
            Browse, upload and download container files. Text editing is limited
            to 8 MiB.
          </p>
        </div>
        <div className="detail-inline">
          {windows && (
            <button
              className="button secondary"
              disabled={!!op.pending}
              onClick={() => {
                op.clear();
                setPrepareSuccess('');
                setPrepare(true);
              }}
            >
              Prepare file access
            </button>
          )}
          <button
            className="button secondary"
            onClick={refresh}
            disabled={!!op.pending}
          >
            Refresh
          </button>
        </div>
      </div>
      <Feedback
        error={op.error || listError || message}
        success={op.success || prepareSuccess}
        pending={op.pending}
      />
      <form
        className="detail-path"
        onSubmit={(event) => {
          event.preventDefault();
          navigate(pathInput);
        }}
      >
        <button
          type="button"
          className="button secondary"
          onClick={() => navigate(parentPath(path, windows))}
          disabled={!!op.pending}
        >
          Up
        </button>
        <label>
          Directory
          <input
            aria-label="Directory path"
            value={pathInput}
            onChange={(e) => setPathInput(e.target.value)}
          />
        </label>
        <button className="button secondary" disabled={!!op.pending}>
          Open
        </button>
      </form>
      <div className="file-actions">
        <form className="detail-inline" onSubmit={(event) => void mkdir(event)}>
          <label>
            New directory
            <input
              value={directory}
              onChange={(e) => setDirectory(e.target.value)}
              placeholder="Directory name"
            />
          </label>
          <button className="button secondary" disabled={!!op.pending}>
            Create directory
          </button>
        </form>
        <label className="button secondary detail-upload">
          Upload file
          <input
            type="file"
            disabled={!!op.pending || entries === null}
            onChange={(event) => {
              const file = event.target.files?.[0];
              event.target.value = '';
              if (file) upload(file);
            }}
          />
        </label>
      </div>
      {entries === null && !listError && (
        <p className="loading" role="status">
          Loading directory…
        </p>
      )}
      {entries &&
        (entries.length ? (
          <div
            className="table-scroll"
            role="region"
            aria-label="Container files"
            tabIndex={0}
          >
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Type</th>
                  <th>Size</th>
                  <th>Modified</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((entry) => (
                  <tr key={entry.path}>
                    <td>
                      {entry.type === 'file' || directoryEntry(entry) ? (
                        <button
                          className="text-button"
                          disabled={!!op.pending}
                          onClick={() => void open(entry)}
                        >
                          {entry.name}
                        </button>
                      ) : (
                        entry.name
                      )}
                    </td>
                    <td>{entry.type}</td>
                    <td>{directoryEntry(entry) ? '—' : bytes(entry.size)}</td>
                    <td>
                      {entry.modified
                        ? new Date(entry.modified).toLocaleString()
                        : '—'}
                    </td>
                    <td>
                      <div className="row-actions">
                        <button
                          disabled={!!op.pending}
                          onClick={() => void downloadEntry(entry)}
                        >
                          {entry.type === 'file'
                            ? 'Download'
                            : 'Download archive'}
                        </button>
                        <button
                          className="danger-text"
                          disabled={!!op.pending}
                          onClick={() => setRemove(entry)}
                        >
                          Delete
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="detail-empty">This directory is empty.</p>
        ))}
      {selected && (
        <section className="file-editor">
          <div className="detail-section-heading">
            <div>
              <h3>Editor · {selected.path}</h3>
              <p>{dirty ? 'Unsaved changes' : 'UTF-8 text'}</p>
            </div>
            <div className="detail-inline">
              <button
                className="button secondary"
                disabled={!!op.pending}
                onClick={() => {
                  if (leaveDraft()) setSelected(null);
                }}
              >
                Close editor
              </button>
              <button
                className="button primary"
                disabled={!dirty || !!op.pending}
                onClick={prepareEdit}
              >
                Save
              </button>
            </div>
          </div>
          <label>
            <span className="sr-only">File contents</span>
            <textarea
              className="code-input editor"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
            />
          </label>
        </section>
      )}
      {prepare && (
        <Confirm
          title="Prepare file access?"
          detail="This installs the UCD file utility in the container. If the container is running, it will be briefly stopped and restarted, interrupting its services."
          action="Prepare file access"
          pending={!!op.pending}
          error={op.error}
          close={() => {
            if (!op.pending) setPrepare(false);
          }}
          confirm={() => void prepareFileAccess()}
        />
      )}
      {write?.kind === 'edit' && (
        <Confirm
          title={write.title}
          detail={write.description}
          action={write.action}
          pending={!!op.pending}
          error={op.error}
          close={() => setWrite(null)}
          confirm={() => void saveFile(write)}
        />
      )}
      {write?.kind === 'upload' && (
        <Modal
          title={write.title}
          close={() => {
            if (op.pending) uploadController.current?.abort();
            else setWrite(null);
          }}
        >
          <Feedback error={op.error} />
          <div className="muted">{write.description}</div>
          {progress && (
            <p className="detail-transfer-progress" role="status">
              Uploading {bytes(progress.sent)} of {bytes(progress.total)}
            </p>
          )}
          <div className="modal-actions">
            <button
              className="button secondary"
              onClick={() => {
                if (op.pending) uploadController.current?.abort();
                else setWrite(null);
              }}
            >
              {op.pending ? 'Cancel upload' : 'Cancel'}
            </button>
            <button
              className="button primary"
              disabled={!!op.pending}
              onClick={() => void saveFile(write)}
            >
              {op.pending ? 'Uploading…' : write.action}
            </button>
          </div>
        </Modal>
      )}
      {remove && (
        <Confirm
          title={`Delete ${remove.name}?`}
          detail={
            directoryEntry(remove)
              ? `Delete ${remove.path} and everything in it?`
              : `Delete ${remove.path}?`
          }
          action="Delete"
          pending={!!op.pending}
          error={op.error}
          close={() => setRemove(null)}
          confirm={() => void removeEntry(remove)}
        />
      )}
    </>
  );
}

type StatsSample = {
  cpuPercent?: number;
  memoryBytes?: number;
  memoryLimitBytes?: number;
  networkRxBytes?: number;
  networkTxBytes?: number;
  blockReadBytes?: number;
  blockWriteBytes?: number;
  pids?: number;
  sampleTime?: string;
  metricsAvailable: boolean;
};
type MetricKey = Exclude<keyof StatsSample, 'metricsAvailable' | 'sampleTime'>;
function Sparkline({ samples, name }: { samples: number[]; name: string }) {
  const max = Math.max(...samples, 1);
  const points = samples
    .map(
      (value, index) =>
        `${samples.length === 1 ? 0 : (index / (samples.length - 1)) * 100},${32 - (Math.max(0, value) / max) * 30}`,
    )
    .join(' ');
  return (
    <svg
      className="detail-sparkline"
      viewBox="0 0 100 32"
      preserveAspectRatio="none"
      role="img"
      aria-label={`${name} recent trend`}
    >
      {samples.length === 1 ? (
        <circle
          cx="50"
          cy={32 - (Math.max(0, samples[0] ?? 0) / max) * 30}
          r="1.5"
        />
      ) : (
        <polyline points={points} vectorEffect="non-scaling-stroke" />
      )}
    </svg>
  );
}
function Stats({ container }: { container: Container }) {
  const [samples, setSamples] = useState<StatsSample[]>([]);
  const [error, setError] = useState('');
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let active = true;
    let timer: number | undefined;
    const controller = new AbortController();
    async function load() {
      try {
        const result = await call<StatsSample>(
          'container.stats',
          container.engine,
          { id: container.id },
          { signal: controller.signal },
        );
        if (active) {
          setSamples((current) => [...current, result].slice(-60));
          setError('');
        }
      } catch (reason) {
        if (active) setError(errorText(reason));
      }
      if (active) timer = window.setTimeout(() => void load(), 2000);
    }
    void load();
    return () => {
      active = false;
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [container.engine, container.id, revision]);
  const current = samples.at(-1);
  const metrics: {
    key: MetricKey;
    label: string;
    format: (n: number) => string;
  }[] = [
    {
      key: 'cpuPercent',
      label: 'CPU',
      format: (value) => `${value.toFixed(1)}%`,
    },
    { key: 'memoryBytes', label: 'Memory', format: bytes },
    { key: 'networkRxBytes', label: 'Network received', format: bytes },
    { key: 'networkTxBytes', label: 'Network sent', format: bytes },
    { key: 'blockReadBytes', label: 'Disk read', format: bytes },
    { key: 'blockWriteBytes', label: 'Disk written', format: bytes },
    { key: 'pids', label: 'PIDs', format: (value) => value.toLocaleString() },
  ];
  return (
    <>
      <div className="detail-section-heading">
        <div>
          <h2>Live statistics</h2>
          <p>
            Refreshes about every two seconds. Network and disk values are
            cumulative totals.
          </p>
        </div>
        <button
          className="button secondary"
          onClick={() => setRevision((value) => value + 1)}
        >
          Refresh
        </button>
      </div>
      <Feedback error={error} />
      {!current && !error && (
        <p className="loading" role="status">
          Loading statistics…
        </p>
      )}
      {current && !current.metricsAvailable && (
        <p className="detail-empty">
          Statistics are unavailable for this container.
        </p>
      )}
      {current?.metricsAvailable && (
        <>
          <p className="detail-sample-time">
            Latest sample:{' '}
            {current.sampleTime
              ? new Date(current.sampleTime).toLocaleString()
              : 'time unavailable'}{' '}
            · Memory limit:{' '}
            {current.memoryLimitBytes === undefined
              ? 'unavailable'
              : bytes(current.memoryLimitBytes)}
          </p>
          <div className="detail-stats-grid">
            {metrics.map(({ key, label, format }) => {
              const value = current[key];
              const series = samples
                .filter(
                  (sample) =>
                    sample.metricsAvailable && typeof sample[key] === 'number',
                )
                .map((sample) => sample[key] as number);
              return (
                <article className="detail-stat" key={key}>
                  <span>{label}</span>
                  <strong>
                    {typeof value === 'number' ? format(value) : 'Unavailable'}
                  </strong>
                  {series.length > 0 && (
                    <Sparkline samples={series} name={label} />
                  )}
                </article>
              );
            })}
          </div>
        </>
      )}
    </>
  );
}
