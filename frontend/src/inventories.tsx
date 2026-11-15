import { Icon } from './icon';
import { useState } from 'react';
import { bytes, call, errorText } from './api';
import type { Container, ListResult } from './api';
import { ContainerDetail } from './container-detail';
import { ContainerActions } from './resource-container-actions';
import { CreateContainer } from './resource-container-create';
import { InventoryErrors } from './resource-utils';
import {
  Badge,
  Confirm,
  Empty,
  Feedback,
  LoadState,
  SectionTitle,
  useOperation,
} from './ui';
import { useData } from './use-data';
import './resources.css';

export { Images } from './resource-images';
export { Volumes } from './resource-volumes';

const loadContainers = () =>
  call<ListResult<Container>>('container.list', '', { metrics: true });
const keyOf = (item: Container) => `${item.engine}:${item.id}`;

export function Containers({
  revision,
  canOperate,
}: {
  revision: number;
  canOperate: boolean;
}) {
  const list = useData(loadContainers, revision);
  const op = useOperation();
  const [search, setSearch] = useState('');
  const [engine, setEngine] = useState('');
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [detail, setDetail] = useState<Container | null>(null);
  const [tools, setTools] = useState<Container | null>(null);
  const [create, setCreate] = useState(false);
  const [confirm, setConfirm] = useState<{
    action: string;
    items: Container[];
  } | null>(null);
  const items = (list.data?.items || []).filter(
    (item) =>
      (!engine || item.engine === engine) &&
      `${item.name} ${item.image} ${item.id} ${item.project || ''}`
        .toLowerCase()
        .includes(search.toLowerCase()),
  );
  const selectedItems = (list.data?.items || []).filter((item) =>
    selected.has(keyOf(item)),
  );
  async function action(actionName: string, targets: Container[]) {
    const failures: string[] = [];
    await op.run(
      `${actionName}: ${targets.length} container(s)`,
      async () => {
        for (const item of targets) {
          try {
            await call('container.action', item.engine, {
              id: item.id,
              action: actionName,
            });
            setSelected((previous) => {
              const next = new Set(previous);
              next.delete(keyOf(item));
              return next;
            });
          } catch (reason) {
            failures.push(
              `${item.name} (${item.engine}): ${errorText(reason)}`,
            );
          }
        }
        list.refresh();
        if (failures.length)
          throw new Error(`Some operations failed:\n${failures.join('\n')}`);
      },
      `${actionName} completed for ${targets.length} container(s).`,
    );
    setConfirm(null);
  }
  function request(actionName: string, targets: Container[]) {
    if (actionName === 'remove' || actionName === 'kill')
      setConfirm({ action: actionName, items: targets });
    else void action(actionName, targets);
  }
  if (detail)
    return (
      <ContainerDetail
        container={
          list.data?.items.find((item) => keyOf(item) === keyOf(detail)) ||
          detail
        }
        canOperate={canOperate}
        onClose={() => {
          setDetail(null);
          list.refresh();
        }}
        onChanged={list.refresh}
      />
    );
  return (
    <>
      <SectionTitle
        eyebrow="RUNTIME INVENTORY"
        title="Containers"
        description="Inspect and control containers across both engines."
      >
        <button className="button secondary" onClick={list.refresh}>
          <Icon name="refresh" size={16} /> Refresh
        </button>
        <button
          className="button primary"
          disabled={!canOperate}
          onClick={() => setCreate(true)}
        >
          <Icon name="plus" size={16} /> Create container
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <section className="panel table-panel">
        <div className="toolbar">
          <div>
            <h2>All containers</h2>
            <p>
              {items.length} {items.length === 1 ? 'container' : 'containers'}{' '}
              shown across {engine || 'both engines'}
            </p>
          </div>
          <div className="filters">
            <input
              aria-label="Search containers"
              placeholder="Search containers…"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
            <select
              aria-label="Filter containers by engine"
              value={engine}
              onChange={(event) => setEngine(event.target.value)}
            >
              <option value="">All engines</option>
              <option value="windows">Windows</option>
              <option value="linux">Linux</option>
            </select>
          </div>
        </div>
        {selectedItems.length > 0 && (
          <div className="resource-bulk">
            <strong>{selectedItems.length} selected</strong>
            {[
              'start',
              'stop',
              'restart',
              'pause',
              'unpause',
              'kill',
              'remove',
            ].map((name) => (
              <button
                key={name}
                className={
                  name === 'kill' || name === 'remove'
                    ? 'button quiet danger-text'
                    : 'button quiet'
                }
                disabled={!canOperate || !selectedItems.length || !!op.pending}
                onClick={() => request(name, selectedItems)}
              >
                {name.charAt(0).toUpperCase() + name.slice(1)}
              </button>
            ))}
            <button
              className="text-button"
              onClick={() => setSelected(new Set())}
            >
              Clear selection
            </button>
          </div>
        )}
        <LoadState {...list} retry={list.refresh} />
        <InventoryErrors errors={list.data?.errors} />
        {list.data && items.length === 0 ? (
          <Empty title="No containers found">
            Try another filter or create a container.
          </Empty>
        ) : (
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label="Container inventory"
          >
            <table>
              <thead>
                <tr>
                  <th>
                    <input
                      type="checkbox"
                      aria-label="Select all shown containers"
                      checked={
                        items.length > 0 &&
                        items.every((item) => selected.has(keyOf(item)))
                      }
                      onChange={(event) =>
                        setSelected((previous) => {
                          const next = new Set(previous);
                          for (const item of items) {
                            if (event.target.checked) next.add(keyOf(item));
                            else next.delete(keyOf(item));
                          }
                          return next;
                        })
                      }
                    />
                  </th>
                  <th>Name / image</th>
                  <th>Engine</th>
                  <th>Status</th>
                  <th>CPU</th>
                  <th>Memory</th>
                  <th>Ports</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={keyOf(item)}>
                    <td>
                      <input
                        type="checkbox"
                        aria-label={`Select ${item.name}`}
                        checked={selected.has(keyOf(item))}
                        onChange={(event) =>
                          setSelected((previous) => {
                            const next = new Set(previous);
                            if (event.target.checked) next.add(keyOf(item));
                            else next.delete(keyOf(item));
                            return next;
                          })
                        }
                      />
                    </td>
                    <td>
                      <button
                        className="table-action"
                        onClick={() => setDetail(item)}
                      >
                        {item.name}
                      </button>
                      <span className="subline" title={item.image}>
                        {item.image}
                      </span>
                    </td>
                    <td>
                      <span className="engine-tag">{item.engine}</span>
                    </td>
                    <td>
                      <Badge
                        tone={
                          item.state.toLowerCase() === 'running'
                            ? 'good'
                            : 'neutral'
                        }
                      >
                        {item.status || item.state}
                      </Badge>
                    </td>
                    <td className="numeric">
                      {item.metricsAvailable
                        ? `${item.cpuPercent.toFixed(1)}%`
                        : '—'}
                    </td>
                    <td className="numeric">
                      {item.metricsAvailable ? bytes(item.memoryBytes) : '—'}
                    </td>
                    <td className="small">
                      {item.ports?.length
                        ? item.ports
                            .map(
                              (port) =>
                                `${port.published ? `${port.hostIP || '*'}:${port.published} → ` : ''}${port.target}/${port.protocol}`,
                            )
                            .join(', ')
                        : '—'}
                    </td>
                    <td>
                      <div className="row-actions">
                        <button onClick={() => setDetail(item)}>Open</button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => setTools(item)}
                        >
                          Tools
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() =>
                            request(
                              item.state.toLowerCase() === 'running'
                                ? 'stop'
                                : 'start',
                              [item],
                            )
                          }
                        >
                          {item.state.toLowerCase() === 'running'
                            ? 'Stop'
                            : 'Start'}
                        </button>
                        <button
                          className="danger-text"
                          disabled={!canOperate || !!op.pending}
                          onClick={() => request('remove', [item])}
                        >
                          Remove
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
      {create && (
        <CreateContainer
          close={() => {
            setCreate(false);
            list.refresh();
          }}
          onCreated={list.refresh}
        />
      )}
      {tools && (
        <ContainerActions
          container={tools}
          close={() => setTools(null)}
          changed={list.refresh}
        />
      )}
      {confirm && (
        <Confirm
          title={`${confirm.action === 'kill' ? 'Kill' : 'Remove'} ${confirm.items.length} container(s)?`}
          detail={`${confirm.items.map((item) => `${item.name} (${item.engine})`).join(', ')}. ${confirm.action === 'remove' ? 'Volumes are not removed.' : 'Running processes will be terminated immediately.'}`}
          action={
            confirm.action === 'kill' ? 'Kill containers' : 'Remove containers'
          }
          pending={!!op.pending}
          error={op.error}
          close={() => setConfirm(null)}
          confirm={() => void action(confirm.action, confirm.items)}
        />
      )}
    </>
  );
}
