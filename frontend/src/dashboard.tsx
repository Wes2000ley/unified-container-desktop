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

const loadEngines = () => call<EngineStatus[]>('engine.list');
const loadContainers = () =>
  call<ListResult<Container>>('container.list', '', { metrics: true });
export function Dashboard({
  revision,
  admin,
}: {
  revision: number;
  admin: boolean;
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
        eyebrow="WORKSPACE OVERVIEW"
        title="Dashboard"
        description="Monitor both container engines from one place."
      >
        <button className="button secondary" onClick={refresh}>
          ↻ &nbsp;Refresh
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <div className="overview-grid">
        <div className="metric">
          <span className="metric-icon">◈</span>
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
          <span className="metric-icon blue">▣</span>
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
          <span className="metric-icon violet">◉</span>
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
          <span className="metric-icon amber">▤</span>
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
      <div className="two-column">
        <section className="panel">
          <div className="panel-heading">
            <div>
              <h2>Engine health</h2>
              <p>Current engine status and capacity</p>
            </div>
            <span className="mini-label">LIVE STATUS</span>
          </div>
          <LoadState {...engines} retry={engines.refresh} />
          {engines.data?.length === 0 && (
            <Empty title="No engines reported">
              Refresh to check the controller connection.
            </Empty>
          )}
          <div className="engine-list">
            {engines.data?.map((e) => (
              <div className="engine-row" key={e.id}>
                <div className={`engine-symbol ${e.id}`}>
                  {e.id === 'windows' ? '⊞' : '◉'}
                </div>
                <div className="grow">
                  <div className="row-title">
                    {e.id === 'windows' ? 'Windows engine' : 'Linux engine'}
                  </div>
                  <div className="muted small">
                    {e.resourceSaver
                      ? 'Resource Saver · Starts automatically when needed'
                      : e.online
                        ? `${e.version || 'Version unavailable'} · ${e.cpus} CPUs · ${bytes(e.memoryBytes)}`
                        : e.error || 'Engine unavailable'}
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
            ))}
          </div>
        </section>
        <section className="panel">
          <div className="panel-heading">
            <div>
              <h2>Container activity</h2>
              <p>State across available engines</p>
            </div>
          </div>
          <LoadState {...containers} retry={containers.refresh} />
          {containers.data?.errors.map((e) => (
            <div key={e.engine} className="inline-error">
              {e.engine}: {e.message}
            </div>
          ))}
          <div className="activity-stats">
            <div>
              <span>Running</span>
              <strong>{containers.data ? running.length : '—'}</strong>
            </div>
            <div>
              <span>Other states</span>
              <strong>
                {containers.data ? items.length - running.length : '—'}
              </strong>
            </div>
            <div>
              <span>Reported</span>
              <strong>{containers.data ? items.length : '—'}</strong>
            </div>
          </div>
          <p className="muted small">
            Metrics reflect the latest reported container inventory. Unavailable
            engines are excluded.
          </p>
        </section>
      </div>
    </>
  );
}
