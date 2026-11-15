import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { bytes, call } from './api';
import type {
  EngineStatus,
  Event,
  Principal,
  Settings as SettingsData,
  Share,
  WSLStatus,
} from './api';
import {
  Badge,
  Confirm,
  Empty,
  Feedback,
  LoadState,
  Modal,
  OutputModal,
  SectionTitle,
  useOperation,
} from './ui';
import { useData } from './use-data';
import {
  getDesktopPreferences,
  isDesktopClient,
  setDesktopPreferences,
} from './desktop-preferences';
import type { DesktopPreferences } from './desktop-preferences';
import { savePreferences, usePreferences } from './preferences';
import type { UserPreferences } from './preferences';
import './settings.css';
import { Icon } from './icon';
import type { IconName } from './icon';

const sections: { id: string; label: string; icon: IconName }[] = [
  { id: 'general', label: 'General', icon: 'settings' },
  { id: 'resources', label: 'Resources', icon: 'cpu' },
  { id: 'sharing', label: 'File sharing', icon: 'volumes' },
  { id: 'proxies', label: 'Proxies', icon: 'registry' },
  { id: 'network', label: 'Network', icon: 'networks' },
  { id: 'access', label: 'Access', icon: 'projects' },
  { id: 'activity', label: 'Activity', icon: 'dashboard' },
];

const loadSettings = () =>
  call<{ settings: SettingsData; principal: Principal }>('system.settings');
const loadLinuxEngine = () => call<EngineStatus>('engine.status', 'linux');
const loadWSL = () => call<WSLStatus>('wsl.status');
const loadOperators = () => call<string[]>('operator.list');
const loadShares = () => call<Share[]>('share.list');

export function Settings({
  revision,
  admin,
  events,
}: {
  revision: number;
  admin: boolean;
  events: Event[];
}) {
  const settings = useData(loadSettings, revision);
  const linuxEngine = useData(loadLinuxEngine, revision);
  const wsl = useData(loadWSL, revision);
  const operators = useData(loadOperators, revision);
  const shares = useData(loadShares, revision);
  const op = useOperation();
  const [output, setOutput] = useState<unknown>();
  const [section, setSection] = useState('general');
  const [eventPage, setEventPage] = useState(0);
  const [eventPageSize, setEventPageSize] = useState(10);
  const newestFirst = [...events].reverse();
  const pageCount = Math.max(1, Math.ceil(newestFirst.length / eventPageSize));
  const currentPage = Math.min(eventPage, pageCount - 1);
  const pageEvents = newestFirst.slice(
    currentPage * eventPageSize,
    (currentPage + 1) * eventPageSize,
  );
  async function diagnostics() {
    await op.run('Collecting diagnostics', async () =>
      setOutput(await call('system.diagnostics')),
    );
  }
  return (
    <>
      <SectionTitle
        eyebrow="SYSTEM CONTROL"
        title="Settings"
        description="Configure the service, Linux resources, access and app preferences."
      >
        <button
          className="button secondary"
          onClick={() => {
            settings.refresh();
            linuxEngine.refresh();
            wsl.refresh();
            operators.refresh();
            shares.refresh();
          }}
        >
          <Icon name="refresh" size={16} /> Refresh
        </button>
        <button
          className="button primary"
          disabled={!!op.pending}
          onClick={() => void diagnostics()}
        >
          Run diagnostics
        </button>
      </SectionTitle>
      <Feedback error={op.error} pending={op.pending} />
      <LoadState {...settings} retry={settings.refresh} />
      <div className="settings-layout">
        <nav className="settings-nav" aria-label="Settings sections">
          {sections.map((item) => (
            <button
              key={item.id}
              aria-current={section === item.id ? 'page' : undefined}
              onClick={() => setSection(item.id)}
            >
              <Icon name={item.icon} size={17} />
              {item.label}
            </button>
          ))}
          <p>Changes to machine settings apply to every authorized user.</p>
        </nav>
        <div className="settings-content">
          <div className="settings-section" hidden={section !== 'general'}>
            <InterfacePreferences />
            <DesktopPreferencesPanel />
          </div>
          <div className="settings-section" hidden={section !== 'proxies'}>
            {settings.data && (
              <section className="panel settings-proxy-panel">
                <div className="panel-heading">
                  <div>
                    <h2>Proxies and registry mirrors</h2>
                    <p>Shared by the machine's Linux and Windows engines</p>
                  </div>
                </div>
                <ConfigForm
                  key={`${JSON.stringify(settings.data.settings.dockerProxy)}:${JSON.stringify(settings.data.settings.containersProxy)}:${JSON.stringify(settings.data.settings.registryMirrors)}`}
                  settings={settings.data.settings}
                  admin={admin}
                />
              </section>
            )}
          </div>
          <div className="settings-section" hidden={section !== 'resources'}>
            <section className="panel">
              <div className="panel-heading">
                <div>
                  <h2>Linux WSL resources</h2>
                  <p>Dedicated service-owned distribution</p>
                </div>
                {wsl.data && (
                  <Badge tone={wsl.data.running ? 'good' : 'neutral'}>
                    {wsl.data.running ? 'Running' : 'Stopped'}
                  </Badge>
                )}
              </div>
              <LoadState {...wsl} retry={wsl.refresh} />
              {wsl.data && (
                <WSLForm status={wsl.data} admin={admin} saved={wsl.refresh} />
              )}
            </section>
            {settings.data && (
              <section className="panel">
                <div className="panel-heading">
                  <div>
                    <h2>Resource saver</h2>
                    <p>Reduce idle Linux engine resource use</p>
                  </div>
                </div>
                <ResourceSaverForm
                  key={JSON.stringify(settings.data.settings.resourceSaver)}
                  settings={settings.data.settings}
                  admin={admin}
                  engine={linuxEngine.data}
                  saved={linuxEngine.refresh}
                />
              </section>
            )}
          </div>
          <div className="settings-section" hidden={section !== 'network'}>
            {settings.data && (
              <section className="panel settings-network-panel">
                <div className="panel-heading">
                  <div>
                    <h2>Network defaults</h2>
                    <p>
                      Port binding applies to both engines; other defaults are
                      Linux-only
                    </p>
                  </div>
                </div>
                <NetworkForm
                  key={JSON.stringify(settings.data.settings.network)}
                  settings={settings.data.settings}
                  admin={admin}
                />
              </section>
            )}
          </div>
          <div className="settings-section" hidden={section !== 'access'}>
            <section className="panel">
              <div className="panel-heading">
                <div>
                  <h2>Container operators</h2>
                  <p>Windows accounts allowed to manage containers</p>
                </div>
              </div>
              <LoadState {...operators} retry={operators.refresh} />
              {operators.data && (
                <Operators
                  items={operators.data}
                  admin={admin}
                  saved={operators.refresh}
                />
              )}
            </section>
          </div>
          <div className="settings-section" hidden={section !== 'sharing'}>
            <section className="panel">
              <div className="panel-heading">
                <div>
                  <h2>Virtual file shares</h2>
                  <p>Directories available to Linux container bind mounts</p>
                </div>
              </div>
              <LoadState {...shares} retry={shares.refresh} />
              {shares.data && (
                <Shares
                  items={shares.data}
                  admin={admin}
                  saved={shares.refresh}
                />
              )}
            </section>
          </div>
          <section
            className="panel events-panel settings-section"
            hidden={section !== 'activity'}
          >
            <div className="panel-heading">
              <div>
                <h2>Recent events</h2>
                <p>Controller and operation activity for this session</p>
              </div>
              {events.length > 0 && (
                <label className="events-page-size">
                  Rows
                  <select
                    value={eventPageSize}
                    onChange={(event) => {
                      setEventPageSize(Number(event.target.value));
                      setEventPage(0);
                    }}
                  >
                    <option value={10}>10</option>
                    <option value={25}>25</option>
                    <option value={50}>50</option>
                  </select>
                </label>
              )}
            </div>
            {events.length === 0 ? (
              <Empty title="No events reported" />
            ) : (
              <>
                <div className="event-list">
                  {pageEvents.map((event) => (
                    <div className="event-row" key={event.sequence}>
                      <span className="event-dot" />
                      <span className="event-time">
                        {new Date(event.time).toLocaleString()}
                      </span>
                      <span>
                        <strong>{event.type}</strong>{' '}
                        {event.engine && (
                          <span className="engine-tag">{event.engine}</span>
                        )}
                        <span className="subline">{event.message}</span>
                      </span>
                    </div>
                  ))}
                </div>
                <nav
                  className="events-pagination"
                  aria-label="Recent event pages"
                >
                  <span>
                    Showing {currentPage * eventPageSize + 1}–
                    {Math.min((currentPage + 1) * eventPageSize, events.length)}{' '}
                    of {events.length}
                  </span>
                  <div>
                    <button
                      className="button secondary"
                      disabled={currentPage === 0}
                      onClick={() =>
                        setEventPage((page) => Math.max(0, page - 1))
                      }
                    >
                      Previous
                    </button>
                    <span aria-live="polite">
                      Page {currentPage + 1} of {pageCount}
                    </span>
                    <button
                      className="button secondary"
                      disabled={currentPage >= pageCount - 1}
                      onClick={() =>
                        setEventPage((page) =>
                          Math.min(pageCount - 1, page + 1),
                        )
                      }
                    >
                      Next
                    </button>
                  </div>
                </nav>
              </>
            )}
          </section>
        </div>
      </div>
      {output !== undefined && (
        <OutputModal
          title="System diagnostics"
          value={output}
          close={() => setOutput(undefined)}
        />
      )}
    </>
  );
}

function InterfacePreferences() {
  const preferences = usePreferences();
  const [draft, setDraft] = useState<UserPreferences>(preferences);
  const [saved, setSaved] = useState('');
  function submit(event: FormEvent) {
    event.preventDefault();
    const persisted = savePreferences(draft);
    setSaved(
      persisted
        ? 'Preferences saved on this device.'
        : 'Preferences applied for this session. Browser storage is unavailable.',
    );
  }
  return (
    <section className="panel">
      <div className="panel-heading">
        <div>
          <h2>Interface preferences</h2>
          <p>Refresh cadence and container log defaults</p>
        </div>
      </div>
      <form className="form-stack" onSubmit={submit}>
        <label>
          Refresh data every
          <select
            value={draft.refreshSeconds}
            onChange={(event) =>
              setDraft({
                ...draft,
                refreshSeconds: Number(
                  event.target.value,
                ) as UserPreferences['refreshSeconds'],
              })
            }
          >
            <option value={5}>5 seconds</option>
            <option value={10}>10 seconds</option>
            <option value={15}>15 seconds</option>
            <option value={30}>30 seconds</option>
          </select>
        </label>
        <label>
          Default log lines
          <select
            value={draft.logTail}
            onChange={(event) =>
              setDraft({
                ...draft,
                logTail: Number(
                  event.target.value,
                ) as UserPreferences['logTail'],
              })
            }
          >
            <option value={100}>100 lines</option>
            <option value={300}>300 lines</option>
            <option value={1000}>1,000 lines</option>
            <option value={5000}>5,000 lines</option>
          </select>
        </label>
        <label className="setting-check">
          <input
            type="checkbox"
            checked={draft.logTimestamps}
            onChange={(event) =>
              setDraft({ ...draft, logTimestamps: event.target.checked })
            }
          />
          Include timestamps in container logs by default
        </label>
        <div className="settings-save-row">
          <button className="button secondary">Save preferences</button>
          <span className="muted small">
            Stored locally for this app profile.
          </span>
        </div>
      </form>
      {saved && (
        <div className="notice success" role="status">
          {saved}
        </div>
      )}
    </section>
  );
}

function DesktopPreferencesPanel() {
  const [preferences, setPreferences] = useState<DesktopPreferences>({
    startAtLogin: false,
    startMinimized: false,
    closeToTray: true,
  });
  const [loading, setLoading] = useState(isDesktopClient());
  const [error, setError] = useState('');
  const [loadRevision, setLoadRevision] = useState(0);
  const op = useOperation();
  const desktop = isDesktopClient();
  useEffect(() => {
    if (!desktop) return;
    let active = true;
    getDesktopPreferences().then(
      (value) => {
        if (active) {
          setPreferences(value);
          setLoading(false);
        }
      },
      (reason) => {
        if (active) {
          setError(reason instanceof Error ? reason.message : String(reason));
          setLoading(false);
        }
      },
    );
    return () => {
      active = false;
    };
  }, [desktop, loadRevision]);
  async function submit(event: FormEvent) {
    event.preventDefault();
    setError('');
    await op.run(
      'Saving desktop preferences',
      () => setDesktopPreferences(preferences),
      'Desktop preferences saved.',
    );
  }
  return (
    <section className="panel">
      <div className="panel-heading">
        <div>
          <h2>Desktop behavior</h2>
          <p>Windows startup and notification area</p>
        </div>
      </div>
      {!desktop ? (
        <p className="muted small">
          Open the desktop app to change Windows startup and tray behavior.
        </p>
      ) : loading ? (
        <p className="loading" role="status">
          Loading desktop preferences…
        </p>
      ) : (
        <form className="form-stack" onSubmit={(event) => void submit(event)}>
          {error && (
            <div className="notice error" role="alert">
              <span>{error}</span>
              <button
                type="button"
                className="text-button"
                onClick={() => {
                  setError('');
                  setLoading(true);
                  setLoadRevision((revision) => revision + 1);
                }}
              >
                Retry
              </button>
            </div>
          )}
          <label className="setting-check">
            <input
              type="checkbox"
              checked={preferences.startAtLogin}
              onChange={(event) =>
                setPreferences({
                  ...preferences,
                  startAtLogin: event.target.checked,
                })
              }
            />
            Start Unified Container Desktop when I sign in to Windows
          </label>
          <label className="setting-check">
            <input
              type="checkbox"
              checked={preferences.startMinimized}
              disabled={!preferences.startAtLogin}
              onChange={(event) =>
                setPreferences({
                  ...preferences,
                  startMinimized: event.target.checked,
                })
              }
            />
            Start minimized to the notification area
          </label>
          <label className="setting-check">
            <input
              type="checkbox"
              checked={preferences.closeToTray}
              onChange={(event) =>
                setPreferences({
                  ...preferences,
                  closeToTray: event.target.checked,
                })
              }
            />
            Keep the app running when I close the window
          </label>
          <div className="settings-save-row">
            <button
              className="button secondary"
              disabled={!!op.pending || loading || !!error}
            >
              Save desktop preferences
            </button>
            <span className="muted small">
              Applies to this Windows account.
            </span>
          </div>
          <Feedback
            error={op.error}
            success={op.success}
            pending={op.pending}
          />
        </form>
      )}
    </section>
  );
}

function WSLForm({
  status,
  admin,
  saved,
}: {
  status: WSLStatus;
  admin: boolean;
  saved: () => void;
}) {
  const [cpus, setCPUs] = useState(String(status.cpus));
  const [memory, setMemory] = useState(String(status.memoryMB));
  const [disk, setDisk] = useState(String(status.diskGB));
  const op = useOperation();
  async function submit(event: FormEvent) {
    event.preventDefault();
    const ok = await op.run(
      'Saving WSL limits',
      () =>
        call('wsl.configure', '', {
          cpus: Number(cpus),
          memoryMB: Number(memory),
          diskGB: Number(disk),
        }).then(() => undefined),
      'WSL limits saved. A full Windows/WSL VM restart may be required.',
    );
    if (ok) saved();
  }
  return (
    <form className="form-stack" onSubmit={(event) => void submit(event)}>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      {status.error && <div className="inline-error">{status.error}</div>}
      <div className="wsl-summary">
        <span>
          Distribution <strong>{status.distro || 'Not reported'}</strong>
        </span>
        <span>
          Memory used{' '}
          <strong>{bytes(status.memoryUsedMB * 1024 * 1024)}</strong>
        </span>
        <span>
          Disk used <strong>{bytes(status.diskUsedBytes)}</strong>
        </span>
      </div>
      <div className="form-grid thirds">
        <label>
          CPUs
          <input
            type="number"
            min="1"
            required
            value={cpus}
            onChange={(e) => setCPUs(e.target.value)}
          />
        </label>
        <label>
          Memory (MiB)
          <input
            type="number"
            min="1"
            required
            value={memory}
            onChange={(e) => setMemory(e.target.value)}
          />
        </label>
        <label>
          Disk (GiB)
          <input
            type="number"
            min="1"
            required
            value={disk}
            onChange={(e) => setDisk(e.target.value)}
          />
        </label>
      </div>
      <p className="callout">
        {status.restartRequired
          ? 'A full Windows/WSL VM restart is required for these limits to take effect.'
          : 'Changes may require a full Windows/WSL VM restart. This app will not stop other WSL distributions.'}
      </p>
      <div>
        <button className="button secondary" disabled={!admin || !!op.pending}>
          Save WSL limits
        </button>
      </div>
    </form>
  );
}

function ConfigForm({
  settings,
  admin,
}: {
  settings: SettingsData;
  admin: boolean;
}) {
  const [dockerProxy, setDockerProxy] = useState(settings.dockerProxy);
  const [containersProxy, setContainersProxy] = useState(
    settings.containersProxy,
  );
  const [mirrors, setMirrors] = useState(
    (settings.registryMirrors || []).join('\n'),
  );
  const [savedMessage, setSavedMessage] = useState('');
  const op = useOperation();
  async function submit(event: FormEvent) {
    event.preventDefault();
    setSavedMessage('');
    await op.run('Saving proxy and registry settings', async () => {
      const current = await loadSettings();
      const updated = {
        ...current.settings,
        dockerProxy: cleanProxy(dockerProxy),
        containersProxy: cleanProxy(containersProxy),
        registryMirrors: mirrors
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean),
      };
      const result = await call<{ restartRequired: boolean }>(
        'system.configure',
        '',
        updated,
      );
      if (typeof result.restartRequired !== 'boolean')
        throw new Error(
          'The controller did not report the engine restart state.',
        );
      setSavedMessage(
        result.restartRequired
          ? 'Saved. A Docker engine restart is required to apply daemon proxy and registry mirror changes. New containers and builds on both engines receive the configured proxy defaults.'
          : 'Saved. New containers and builds on both engines receive the configured proxy defaults.',
      );
    });
  }
  return (
    <form
      className="form-stack settings-proxy-form"
      onSubmit={(event) => void submit(event)}
    >
      <Feedback error={op.error} pending={op.pending} />
      {savedMessage && (
        <div className="notice success" role="status">
          {savedMessage}
        </div>
      )}
      <div className="settings-proxy-groups">
        <fieldset className="settings-fieldset">
          <legend>Docker engine traffic</legend>
          <p className="muted small">
            Used by the Linux and Windows Docker engines for registry pulls and
            pushes.
          </p>
          <ProxyFields
            label="Docker proxy"
            value={dockerProxy}
            onChange={setDockerProxy}
            disabled={!admin || !!op.pending}
          />
        </fieldset>
        <fieldset className="settings-fieldset">
          <legend>Container and build traffic</legend>
          <p className="muted small">
            New containers on both engines receive standard proxy environment
            variables, and builds receive matching build arguments. Applications
            must honor the proxy variables; explicit container and build
            settings take precedence.
          </p>
          <ProxyFields
            label="Container proxy"
            value={containersProxy}
            onChange={setContainersProxy}
            disabled={!admin || !!op.pending}
          />
        </fieldset>
      </div>
      <label>
        Registry mirrors <span className="muted">(one URL per line)</span>
        <textarea
          rows={3}
          value={mirrors}
          onChange={(e) => setMirrors(e.target.value)}
          placeholder="https://mirror.example"
          disabled={!admin || !!op.pending}
        />
      </label>
      <div>
        <button className="button secondary" disabled={!admin || !!op.pending}>
          Save proxy settings
        </button>
      </div>
    </form>
  );
}

function cleanProxy(value: SettingsData['dockerProxy']) {
  return {
    http: (value.http ?? '').trim(),
    https: (value.https ?? '').trim(),
    socks5: (value.socks5 ?? '').trim(),
    noProxy: (value.noProxy ?? '').trim(),
  };
}

function ProxyFields({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string;
  value: SettingsData['dockerProxy'];
  onChange: (value: SettingsData['dockerProxy']) => void;
  disabled: boolean;
}) {
  return (
    <div className="form-grid">
      {(
        [
          ['http', 'HTTP URL', 'http://proxy.example:8080'],
          ['https', 'HTTPS URL', 'http://proxy.example:8080'],
          ['socks5', 'SOCKS5 URL', 'socks5://proxy.example:1080'],
          ['noProxy', 'Bypass list', 'localhost,127.0.0.1,.example.com'],
        ] as const
      ).map(([key, title, placeholder]) => (
        <label key={key}>
          {label} {title}
          <input
            value={value[key] ?? ''}
            onChange={(event) =>
              onChange({ ...value, [key]: event.target.value })
            }
            placeholder={placeholder}
            disabled={disabled}
            autoComplete="off"
            spellCheck={false}
            aria-label={`${label} ${title}`}
          />
        </label>
      ))}
    </div>
  );
}

function NetworkForm({
  settings,
  admin,
}: {
  settings: SettingsData;
  admin: boolean;
}) {
  const [network, setNetwork] = useState(settings.network);
  const [savedMessage, setSavedMessage] = useState('');
  const op = useOperation();
  async function submit(event: FormEvent) {
    event.preventDefault();
    setSavedMessage('');
    await op.run('Saving network defaults', async () => {
      const current = await loadSettings();
      const result = await call<{ restartRequired: boolean }>(
        'system.configure',
        '',
        {
          ...current.settings,
          network: {
            ...network,
            dockerSubnet: (network.dockerSubnet ?? '').trim(),
          },
        },
      );
      if (typeof result.restartRequired !== 'boolean')
        throw new Error(
          'The controller did not report the engine restart state.',
        );
      setSavedMessage(
        result.restartRequired
          ? 'Saved. The Linux engine needs a restart to apply bridge and DNS defaults. Existing networks are unchanged.'
          : 'Saved. The updated defaults apply to new Linux networks and containers.',
      );
    });
  }
  return (
    <form className="form-stack" onSubmit={(event) => void submit(event)}>
      <Feedback error={op.error} pending={op.pending} />
      {savedMessage && (
        <div className="notice success" role="status">
          {savedMessage}
        </div>
      )}
      <p className="muted small">
        Default published port binding applies to newly created standalone
        containers and managed Compose on both engines. The subnet, IP mode, DNS
        filtering, and host networking settings configure Linux Docker; Windows
        networking otherwise uses native per-network HNS settings.
      </p>
      <p className="muted small">
        Default networking mode affects newly created custom Linux bridge
        networks, including Compose-created networks. Existing custom networks
        retain their addressing; this mode does not disable IPv4 on Docker's
        built-in bridge. The subnet override below configures the Docker bridge
        subnet.
      </p>
      <div className="form-grid">
        <label>
          Docker bridge subnet override
          <input
            value={network.dockerSubnet ?? ''}
            onChange={(event) =>
              setNetwork({ ...network, dockerSubnet: event.target.value })
            }
            placeholder="Leave blank to keep Docker's default"
            disabled={!admin || !!op.pending}
            spellCheck={false}
          />
        </label>
        <label>
          Default networking mode
          <select
            value={network.defaultNetworkingMode}
            onChange={(event) =>
              setNetwork({
                ...network,
                defaultNetworkingMode: event.target
                  .value as SettingsData['network']['defaultNetworkingMode'],
              })
            }
            disabled={!admin || !!op.pending}
          >
            <option value="dual-stack">Dual stack (IPv4 and IPv6)</option>
            <option value="ipv4only">IPv4 only</option>
            <option value="ipv6only">IPv6 only</option>
          </select>
        </label>
        <label>
          DNS record filtering
          <select
            value={network.dnsInhibition}
            onChange={(event) =>
              setNetwork({
                ...network,
                dnsInhibition: event.target
                  .value as SettingsData['network']['dnsInhibition'],
              })
            }
            disabled={!admin || !!op.pending}
          >
            <option value="auto">Automatic</option>
            <option value="none">No filtering</option>
            <option value="ipv4">Inhibit IPv4 (A) records</option>
            <option value="ipv6">Inhibit IPv6 (AAAA) records</option>
          </select>
        </label>
        <label>
          Default published port binding (both engines)
          <select
            value={network.defaultPortBinding}
            onChange={(event) =>
              setNetwork({
                ...network,
                defaultPortBinding: event.target.value as 'local' | 'all',
              })
            }
            disabled={!admin || !!op.pending}
          >
            <option value="local">Localhost only (127.0.0.1)</option>
            <option value="all">All network interfaces</option>
          </select>
        </label>
      </div>
      <label className="setting-check">
        <input
          type="checkbox"
          checked={network.hostNetworkingEnabled}
          onChange={(event) =>
            setNetwork({
              ...network,
              hostNetworkingEnabled: event.target.checked,
            })
          }
          disabled={!admin || !!op.pending}
        />
        Enable Linux host networking for containers using --network host
      </label>
      <p className="callout">
        Host networking shares the Linux VM network namespace. It does not give
        Windows containers host networking; those use native Windows HNS.
      </p>
      <div>
        <button className="button secondary" disabled={!admin || !!op.pending}>
          Save network defaults
        </button>
      </div>
    </form>
  );
}

function ResourceSaverForm({
  settings,
  admin,
  engine,
  saved,
}: {
  settings: SettingsData;
  admin: boolean;
  engine?: EngineStatus;
  saved: () => void;
}) {
  const [resourceSaver, setResourceSaver] = useState(settings.resourceSaver);
  const [savedMessage, setSavedMessage] = useState('');
  const op = useOperation();
  async function submit(event: FormEvent) {
    event.preventDefault();
    setSavedMessage('');
    const ok = await op.run('Saving resource saver settings', async () => {
      const current = await loadSettings();
      const result = await call<{ restartRequired: boolean }>(
        'system.configure',
        '',
        {
          ...current.settings,
          resourceSaver,
        },
      );
      if (typeof result.restartRequired !== 'boolean')
        throw new Error(
          'The controller did not report whether a restart is required.',
        );
      setSavedMessage(
        result.restartRequired
          ? 'Saved. The controller will apply Resource saver after the required engine restart.'
          : resourceSaver.enabled
            ? 'Saved. Resource saver will sleep the Linux engine after it has no running containers for the selected idle period.'
            : 'Saved. Resource saver is disabled.',
      );
    });
    if (ok) saved();
  }
  const minutes = Math.floor(resourceSaver.idleSeconds / 60);
  const seconds = resourceSaver.idleSeconds % 60;
  const threshold = minutes
    ? `${minutes} min${seconds ? ` ${seconds} sec` : ''}`
    : `${seconds} sec`;
  return (
    <form className="form-stack" onSubmit={(event) => void submit(event)}>
      <Feedback error={op.error} pending={op.pending} />
      {savedMessage && (
        <div className="notice success" role="status">
          {savedMessage}
        </div>
      )}
      {engine && (
        <div className="resource-saver-status" role="status">
          <span className="muted small">Linux engine</span>
          <strong>
            {engine.resourceSaver
              ? 'Sleeping'
              : engine.online
                ? 'Running'
                : 'Unavailable'}
          </strong>
          {engine.resourceSaver && (
            <span className="muted small">
              It wakes automatically when an operation needs the engine.
            </span>
          )}
        </div>
      )}
      <p className="muted small">
        When no Linux containers are running, the Linux engine can sleep. A new
        container operation wakes it automatically. Windows containers are
        unaffected.
      </p>
      <label className="setting-check">
        <input
          type="checkbox"
          checked={resourceSaver.enabled}
          onChange={(event) =>
            setResourceSaver({
              ...resourceSaver,
              enabled: event.target.checked,
            })
          }
          disabled={!admin || !!op.pending}
        />
        Enable Resource saver
      </label>
      <label className="resource-saver-threshold">
        <span className="resource-saver-label">
          Resource saver threshold <output>{threshold}</output>
        </span>
        <input
          type="range"
          min={31}
          max={3600}
          step={1}
          value={resourceSaver.idleSeconds}
          onChange={(event) =>
            setResourceSaver({
              ...resourceSaver,
              idleSeconds: Number(event.target.value),
            })
          }
          disabled={!admin || !resourceSaver.enabled || !!op.pending}
          aria-label="Resource saver threshold (seconds)"
        />
        <span className="muted small">
          31 seconds to 60 minutes after the last Linux container stops.
        </span>
      </label>
      <div>
        <button className="button secondary" disabled={!admin || !!op.pending}>
          Save resource saver
        </button>
      </div>
    </form>
  );
}

function Operators({
  items,
  admin,
  saved,
}: {
  items: string[];
  admin: boolean;
  saved: () => void;
}) {
  const [account, setAccount] = useState('');
  const [remove, setRemove] = useState('');
  const op = useOperation();
  async function add(event: FormEvent) {
    event.preventDefault();
    const ok = await op.run(
      `Adding ${account}`,
      () =>
        call('operator.add', '', { account: account.trim() }).then(
          () => undefined,
        ),
      `${account} added.`,
    );
    if (ok) {
      setAccount('');
      saved();
    }
  }
  async function removeAccount() {
    const ok = await op.run(
      `Removing ${remove}`,
      () =>
        call('operator.remove', '', { account: remove }).then(() => undefined),
      `${remove} removed.`,
    );
    if (ok) {
      setRemove('');
      saved();
    }
  }
  return (
    <>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      {items.length === 0 ? (
        <p className="muted small">No operators reported.</p>
      ) : (
        <div className="simple-list">
          {items.map((item) => (
            <div key={item}>
              <span>{item}</span>
              <button
                className="table-action danger-text"
                disabled={!admin || !!op.pending}
                onClick={() => setRemove(item)}
              >
                Remove
              </button>
            </div>
          ))}
        </div>
      )}
      <form className="inline-form" onSubmit={(event) => void add(event)}>
        <label className="sr-only" htmlFor="operator">
          Windows account
        </label>
        <input
          id="operator"
          required
          value={account}
          onChange={(e) => setAccount(e.target.value)}
          placeholder="DOMAIN\\account"
        />
        <button className="button secondary" disabled={!admin || !!op.pending}>
          Add
        </button>
      </form>
      {remove && (
        <Confirm
          title={`Remove ${remove}?`}
          detail="This account will no longer be a member of Container Operators."
          action="Remove operator"
          pending={!!op.pending}
          error={op.error}
          close={() => setRemove('')}
          confirm={() => void removeAccount()}
        />
      )}
    </>
  );
}

function Shares({
  items,
  admin,
  saved,
}: {
  items: Share[];
  admin: boolean;
  saved: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState('');
  const [path, setPath] = useState('');
  const [remove, setRemove] = useState<Share | null>(null);
  const op = useOperation();
  async function add(event: FormEvent) {
    event.preventDefault();
    const ok = await op.run(
      'Creating share',
      () =>
        call('share.create', '', {
          name: name.trim(),
          windowsPath: path.trim(),
        }).then(() => undefined),
      `${name} added.`,
    );
    if (ok) {
      setOpen(false);
      setName('');
      setPath('');
      saved();
    }
  }
  async function removeShare(item: Share) {
    const ok = await op.run(
      `Removing ${item.name}`,
      () => call('share.remove', '', { name: item.name }).then(() => undefined),
      `${item.name} removed.`,
    );
    if (ok) {
      setRemove(null);
      saved();
    }
  }
  return (
    <>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      {items.length === 0 ? (
        <p className="muted small">No virtual file shares configured.</p>
      ) : (
        <div className="simple-list">
          {items.map((item) => (
            <div key={item.name}>
              <span>
                <strong>{item.name}</strong>
                <span className="subline">
                  {item.windowsPath} → {item.linuxPath}
                </span>
              </span>
              <button
                className="table-action danger-text"
                disabled={!admin || !!op.pending}
                onClick={() => setRemove(item)}
              >
                Remove
              </button>
            </div>
          ))}
        </div>
      )}
      <button
        className="button secondary"
        disabled={!admin}
        onClick={() => setOpen(true)}
      >
        <Icon name="plus" size={16} /> Add virtual file share
      </button>
      {open && (
        <Modal title="Add virtual file share" close={() => setOpen(false)}>
          <form className="form-stack" onSubmit={(event) => void add(event)}>
            <Feedback error={op.error} pending={op.pending} />
            <label>
              Share name
              <input
                required
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="project-data"
              />
            </label>
            <label>
              Windows directory
              <input
                required
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="C:\\Projects\\project-data"
              />
            </label>
            <p className="muted small">
              The directory and its subdirectories are available in Linux at
              /mnt/ucd/&lt;name&gt; for Docker bind mounts. The service rejects
              UNC paths and directories inside its protected data folders.
            </p>
            <div className="modal-actions">
              <button
                type="button"
                className="button secondary"
                disabled={!!op.pending}
                onClick={() => setOpen(false)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={!!op.pending}>
                Add virtual file share
              </button>
            </div>
          </form>
        </Modal>
      )}
      {remove && (
        <Confirm
          title={`Remove share ${remove.name}?`}
          detail="This removes access to the shared directory; the original Windows directory is retained."
          action="Remove share"
          pending={!!op.pending}
          error={op.error}
          close={() => setRemove(null)}
          confirm={() => void removeShare(remove)}
        />
      )}
    </>
  );
}
