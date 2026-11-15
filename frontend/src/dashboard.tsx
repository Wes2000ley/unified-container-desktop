import { useState } from 'react';
import { bytes, call } from './api';
import type { Container, EngineStatus, ListResult } from './api';
import {
  Badge,
  Empty,
  Feedback,
  LoadState,
  SectionTitle,
  useOperation,
} from './ui';
import { useData } from './use-data';
import { Icon } from './icon';

const loadEngines = () => call<EngineStatus[]>('engine.list');
const loadContainers = () =>
  call<ListResult<Container>>('container.list', '', { metrics: true });
export function Dashboard({
  revision,
  admin,
  onNavigate,
}: {
  revision: number;
  admin: boolean;
  onNavigate: (page: 'containers' | 'projects') => void;
}) {
  const engines = useData(loadEngines, revision);
  const containers = useData(loadContainers, revision);
  const op = useOperation();
  const [selected, setSelected] = useState('');
  function refresh() {
    engines.refresh();
    containers.refresh();
  }
  async function engineAction(id: string, action: 'start' | 'stop') {
    setSelected(id);
    const ok = await op.run(
      `${action === 'start' ? 'Starting' : 'Stopping'} ${id} engine`,
      () => call(`engine.${action}`, id).then(() => undefined),
      `${id} engine ${action} requested.`,
    );
    if (ok) refresh();
  }
  const items = containers.data?.items || [];
  const running = items.filter((c) => c.state.toLowerCase() === 'running');
  const measured = running.filter((c) => c.metricsAvailable);
  const metricsComplete =
    !!containers.data &&
    measured.length === running.length &&
    containers.data.errors.length === 0;
  const hasMetrics = metricsComplete || measured.length > 0;
  const metricNote = metricsComplete
    ? 'Running containers'
    : hasMetrics
      ? 'Partial · available measurements'
      : 'Measurements unavailable';
  const totals = engines.data || [];
  return (
    <>
      <SectionTitle
        eyebrow="YOUR LOCAL WORKSPACE"
        title="Dashboard"
        description="Two engines. One place to build, run and manage your containers."
      >
        <button className="button secondary" onClick={refresh}>
          <Icon name="refresh" size={16} /> Refresh
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <div className="overview-grid">
        <div className="metric">
          <span className="metric-icon">
            <Icon name="storage" />
          </span>
          <div>
            <span className="metric-label">Engines online</span>
            <strong>
              {engines.data
                ? `${totals.filter((e) => e.online).length} / ${totals.length}`
                : '—'}
            </strong>
            <small>Windows + Linux</small>
          </div>
        </div>
        <div className="metric">
          <span className="metric-icon">
            <Icon name="containers" />
          </span>
          <div>
            <span className="metric-label">Running containers</span>
            <strong>{containers.data ? running.length : '—'}</strong>
            <small>
              {containers.data
                ? `${items.length} total reported`
                : 'Awaiting inventory'}
            </small>
          </div>
        </div>
        <div className="metric">
          <span className="metric-icon">
            <Icon name="cpu" />
          </span>
          <div>
            <span className="metric-label">Container CPU</span>
            <strong>
              {hasMetrics
                ? `${measured.reduce((n, c) => n + c.cpuPercent, 0).toFixed(1)}%`
                : '—'}
            </strong>
            <small>{metricNote}</small>
          </div>
        </div>
        <div className="metric">
          <span className="metric-icon">
            <Icon name="memory" />
          </span>
          <div>
            <span className="metric-label">Container memory</span>
            <strong>
              {hasMetrics
                ? bytes(measured.reduce((n, c) => n + c.memoryBytes, 0))
                : '—'}
            </strong>
            <small>{metricNote}</small>
          </div>
        </div>
      </div>
      <section className="dashboard-engines">
        <div className="panel-heading">
          <div>
            <h2>Your engines</h2>
            <p>Linux and Windows run side by side, shared across this PC.</p>
          </div>
          <span className="mini-label">MACHINE-WIDE</span>
        </div>
        <LoadState {...engines} retry={engines.refresh} />
        {engines.data?.length === 0 && (
          <Empty title="No engines reported">
            Refresh to check the controller connection.
          </Empty>
        )}
        <div className="engine-list">
          {engines.data?.map((e) => (
            <div className="engine-card panel" key={e.id}>
              <div className="engine-card-top">
                <div className={`engine-symbol ${e.id}`}>
                  <Icon
                    name={e.id === 'windows' ? 'windows' : 'linux'}
                    size={23}
                  />
                </div>
                <div className="grow">
                  <div className="row-title">
                    {e.id === 'windows' ? 'Windows engine' : 'Linux engine'}
                  </div>
                  <div className="muted small">
                    {e.id === 'windows'
                      ? 'Native Docker Engine'
                      : 'Service-owned WSL 2'}
                  </div>
                </div>
                <Badge
                  tone={e.resourceSaver ? 'neutral' : e.online ? 'good' : 'bad'}
                >
                  {e.resourceSaver
                    ? 'Sleeping'
                    : e.online
                      ? 'Online'
                      : 'Offline'}
                </Badge>
              </div>
              <div className="engine-capacity">
                {e.resourceSaver
                  ? 'Resource Saver · Starts automatically when needed'
                  : e.online
                    ? `${e.version || 'Version unavailable'} · ${e.cpus} CPUs · ${bytes(e.memoryBytes)}`
                    : e.error || 'Engine unavailable'}
              </div>
              <div className="engine-card-footer">
                <span>
                  {e.running} running{' '}
                  <span className="muted">/ {e.containers} containers</span>
                </span>
                {admin && (
                  <button
                    className="button quiet"
                    disabled={!!op.pending}
                    onClick={() =>
                      void engineAction(e.id, e.online ? 'stop' : 'start')
                    }
                  >
                    {op.pending && selected === e.id
                      ? 'Working…'
                      : e.online
                        ? 'Stop'
                        : 'Start'}
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      </section>
      <section className="panel dashboard-containers">
        <div className="panel-heading">
          <div>
            <h2>Containers</h2>
            <p>Your running workloads across both engines</p>
          </div>
          <button
            className="button quiet"
            onClick={() => onNavigate('containers')}
          >
            View containers <Icon name="arrow" size={15} />
          </button>
        </div>
        <LoadState {...containers} retry={containers.refresh} />
        {containers.data?.errors.map((e) => (
          <div key={e.engine} className="inline-error">
            {e.engine}: {e.message}
          </div>
        ))}
        {containers.data && running.length === 0 ? (
          <div className="dashboard-empty">
            <span className="empty-icon">
              <Icon name="containers" size={30} />
            </span>
            <h2>No containers running</h2>
            <p>
              Run an image from Containers, or deploy an application with
              Compose.
            </p>
            <div className="heading-actions">
              <button
                className="button primary"
                onClick={() => onNavigate('containers')}
              >
                Open containers
              </button>
              <button
                className="button secondary"
                onClick={() => onNavigate('projects')}
              >
                Open projects
              </button>
            </div>
          </div>
        ) : (
          running.length > 0 && (
            <div
              className="table-scroll"
              role="region"
              aria-label="Running containers"
              tabIndex={0}
            >
              <table>
                <thead>
                  <tr>
                    <th>Container / image</th>
                    <th>Engine</th>
                    <th>Status</th>
                    <th>CPU</th>
                    <th>Memory</th>
                  </tr>
                </thead>
                <tbody>
                  {running.slice(0, 5).map((item) => (
                    <tr key={`${item.engine}:${item.id}`}>
                      <td>
                        <button
                          className="table-action"
                          onClick={() => onNavigate('containers')}
                        >
                          {item.name}
                        </button>
                        <span className="subline">{item.image}</span>
                      </td>
                      <td>
                        <span className="engine-tag">{item.engine}</span>
                      </td>
                      <td>
                        <Badge tone="good">{item.status || item.state}</Badge>
                      </td>
                      <td className="numeric">
                        {item.metricsAvailable
                          ? `${item.cpuPercent.toFixed(1)}%`
                          : '—'}
                      </td>
                      <td className="numeric">
                        {item.metricsAvailable ? bytes(item.memoryBytes) : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        )}
      </section>
    </>
  );
}
