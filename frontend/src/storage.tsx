import { Icon } from './icon';
import { useCallback, useState } from 'react';
import { bytes, call } from './api';
import type { Engine } from './api';
import { EngineSelect } from './resource-utils';
import {
  Confirm,
  Feedback,
  LoadState,
  OutputModal,
  SectionTitle,
  useOperation,
} from './ui';
import { useData } from './use-data';
import './resources.css';

type Scope = 'containers' | 'images' | 'networks' | 'volumes' | 'build-cache';
type Filters = Record<string, string[]>;
function parseFilters(input: string): Filters {
  const filters: Filters = Object.create(null) as Filters;
  for (const line of input.split(/\r?\n/)) {
    if (!line.trim()) continue;
    const separator = line.indexOf('=');
    const key = line.slice(0, separator).trim();
    if (separator < 1 || !key)
      throw new Error('Use one Docker filter per line in key=value format.');
    (filters[key] ||= []).push(line.slice(separator + 1));
  }
  return filters;
}
type Usage = {
  TotalCount: number;
  ActiveCount: number;
  Reclaimable: number;
  TotalSize: number;
};
type DiskUsage = {
  Containers: Usage;
  Images: Usage;
  Volumes: Usage;
  BuildCache: Usage;
};
type Preview = {
  scope: string;
  all: boolean;
  totalCount: number;
  activeCount: number;
  reclaimable: number;
  description: string;
};
export function Storage({
  revision,
  canOperate,
}: {
  revision: number;
  canOperate: boolean;
}) {
  const [engine, setEngine] = useState<Engine>('linux');
  const [engineRevision, setEngineRevision] = useState(0);
  const [scope, setScope] = useState<Scope>('containers');
  const [all, setAll] = useState(false);
  const [filterText, setFilterText] = useState('');
  const [preview, setPreview] = useState<
    (Preview & { engine: Engine; filters: Filters }) | null
  >(null);
  const [output, setOutput] = useState<unknown>();
  const op = useOperation();
  const load = useCallback(
    () => call<DiskUsage>('storage.df', engine),
    [engine],
  );
  const usage = useData(load, revision + engineRevision);
  async function review() {
    const selectedEngine = engine;
    await op.run('Calculating cleanup preview', async () => {
      const filters = parseFilters(filterText);
      const result = await call<Preview>('storage.preview', selectedEngine, {
        scope,
        all,
        ...(Object.keys(filters).length ? { filters } : {}),
      });
      setPreview({ ...result, engine: selectedEngine, filters });
    });
  }
  async function prune() {
    if (!preview) return;
    const ok = await op.run(
      'Pruning storage',
      async () =>
        setOutput(
          await call('storage.prune', preview.engine, {
            scope: preview.scope,
            all: preview.all,
            ...(Object.keys(preview.filters).length
              ? { filters: preview.filters }
              : {}),
            confirmed: true,
          }),
        ),
      'Cleanup completed.',
    );
    if (ok) {
      setPreview(null);
      usage.refresh();
    }
  }
  return (
    <>
      <SectionTitle
        eyebrow="ENGINE STORAGE"
        title="Storage"
        description="Review disk usage and preview scoped cleanup on the selected engine."
      >
        <button className="button secondary" onClick={usage.refresh}>
          <Icon name="refresh" size={16} /> Refresh
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <section className="panel resource-section">
        <div className="resource-inline">
          <EngineSelect
            value={engine}
            disabled={!!op.pending}
            onChange={(value) => {
              setEngine(value);
              setEngineRevision((current) => current + 1);
              setPreview(null);
            }}
          />
        </div>
        <LoadState {...usage} retry={usage.refresh} />
        {!usage.loading && !usage.error && usage.data && (
          <div
            className="table-scroll"
            role="region"
            tabIndex={0}
            aria-label="Engine disk usage"
          >
            <table>
              <thead>
                <tr>
                  <th>Resource</th>
                  <th>Total</th>
                  <th>Active</th>
                  <th>Size</th>
                  <th>Reclaimable</th>
                </tr>
              </thead>
              <tbody>
                {(
                  [
                    ['Containers', usage.data.Containers],
                    ['Images', usage.data.Images],
                    ['Volumes', usage.data.Volumes],
                    ['Build cache', usage.data.BuildCache],
                  ] as const
                ).map(([label, value]) => (
                  <tr key={label}>
                    <td>{label}</td>
                    <td>{value?.TotalCount ?? '—'}</td>
                    <td>{value?.ActiveCount ?? '—'}</td>
                    <td>{value ? bytes(value.TotalSize) : '—'}</td>
                    <td>{value ? bytes(value.Reclaimable) : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <section className="panel resource-section">
        <div className="panel-heading">
          <div>
            <h2>Cleanup</h2>
            <p>Review an engine estimate before removing unused resources.</p>
          </div>
        </div>
        <div className="resource-inline">
          <label>
            Resource scope
            <select
              value={scope}
              disabled={!!op.pending}
              onChange={(event) => {
                setScope(event.target.value as Scope);
                setAll(false);
                setPreview(null);
              }}
            >
              <option value="containers">Stopped containers</option>
              <option value="images">Unused images</option>
              <option value="networks">Unused networks</option>
              <option value="volumes">Unused volumes</option>
              <option value="build-cache">Build cache</option>
            </select>
          </label>
          {(scope === 'images' ||
            scope === 'build-cache' ||
            scope === 'volumes') && (
            <label className="check-row">
              <input
                type="checkbox"
                checked={all}
                disabled={!!op.pending}
                onChange={(event) => {
                  setAll(event.target.checked);
                  setPreview(null);
                }}
              />{' '}
              Include all eligible resources
            </label>
          )}
        </div>
        <label className="prune-filters">
          Docker filters{' '}
          <span className="muted">
            Optional; one key=value per line.{' '}
            {scope === 'build-cache'
              ? 'Use each key once for build cache.'
              : 'Positive label filters may repeat; use one label! exclusion.'}
          </span>
          <textarea
            rows={3}
            value={filterText}
            disabled={!!op.pending}
            onChange={(event) => {
              setFilterText(event.target.value);
              setPreview(null);
            }}
            placeholder={
              scope === 'build-cache' ? 'until=24h' : 'label=project=my-app'
            }
          />
        </label>
        <p className="muted small">
          {scope === 'build-cache'
            ? 'Build cache filters: id, type, description, until, unused-for. The id is a regular expression; use ^...$ for an exact ID.'
            : scope === 'volumes'
              ? 'Volume filters: label, label!.'
              : 'Filters: label, label!, until.'}
        </p>
        <button
          className="button secondary"
          disabled={!canOperate || !!op.pending}
          onClick={() => void review()}
        >
          Preview cleanup
        </button>
      </section>
      {preview && (
        <Confirm
          title={`Prune ${preview.scope}?`}
          detail={
            <>
              <p>{preview.description}</p>
              {preview.scope === 'networks' ? (
                <p>Reclaimable space and network counts are unavailable.</p>
              ) : (
                <p>
                  Estimated reclaimable: {bytes(preview.reclaimable)}.{' '}
                  {preview.totalCount} total, {preview.activeCount} active.
                  Docker’s aggregate estimate is not a guarantee of the exact
                  amount removed.
                </p>
              )}
              {Object.keys(preview.filters).length ? (
                <>
                  <p>
                    Docker will apply these filters. The estimate above covers
                    the whole scope and is not filtered.
                  </p>
                  <pre className="output">
                    {Object.entries(preview.filters)
                      .flatMap(([key, values]) =>
                        values.map((value) => `${key}=${value}`),
                      )
                      .join('\n')}
                  </pre>
                </>
              ) : (
                <p>No filters are applied.</p>
              )}
              <p>
                This removes eligible resources from the {preview.engine}{' '}
                engine.
              </p>
            </>
          }
          action="Prune selected scope"
          pending={!!op.pending}
          error={op.error}
          close={() => setPreview(null)}
          confirm={() => void prune()}
        />
      )}
      {output !== undefined && (
        <OutputModal
          title="Cleanup result"
          value={output}
          close={() => setOutput(undefined)}
        />
      )}
    </>
  );
}
