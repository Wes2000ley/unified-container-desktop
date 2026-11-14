import { useState } from 'react';
import type { FormEvent } from 'react';
import { call, errorText } from './api';
import type { Engine, ListResult } from './api';
import { EngineSelect, InventoryErrors, lines } from './resource-utils';
import {
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
import './resources.css';

type Network = {
  Id: string;
  Name: string;
  Driver: string;
  Scope: string;
  Internal: boolean;
  Attachable: boolean;
  engine: Engine;
};
const loadNetworks = async (): Promise<ListResult<Network>> => {
  const engines: Engine[] = ['windows', 'linux'];
  const results = await Promise.allSettled(
    engines.map((engine) =>
      call<Omit<Network, 'engine'>[]>('network.list', engine),
    ),
  );
  const items: Network[] = [];
  const errors: { engine: string; message: string }[] = [];
  results.forEach((result, index) =>
    result.status === 'fulfilled'
      ? items.push(
          ...(result.value || []).map((network) => ({
            ...network,
            engine: engines[index]!,
          })),
        )
      : errors.push({
          engine: engines[index]!,
          message: errorText(result.reason),
        }),
  );
  return { items, errors };
};
export function Networks({
  revision,
  canOperate,
}: {
  revision: number;
  canOperate: boolean;
}) {
  const list = useData(loadNetworks, revision);
  const op = useOperation();
  const [create, setCreate] = useState(false);
  const [engine, setEngine] = useState<Engine>('linux');
  const [name, setName] = useState('');
  const [driver, setDriver] = useState('');
  const [dnsInhibition, setDNSInhibition] = useState<
    'global' | 'auto' | 'none' | 'ipv4' | 'ipv6'
  >('global');
  const [internal, setInternal] = useState(false);
  const [attachable, setAttachable] = useState(false);
  const [connect, setConnect] = useState<{
    item: Network;
    disconnect: boolean;
  } | null>(null);
  const [container, setContainer] = useState('');
  const [aliases, setAliases] = useState('');
  const [remove, setRemove] = useState<Network | null>(null);
  const [output, setOutput] = useState<{
    title: string;
    value: unknown;
  } | null>(null);
  async function createNetwork(event: FormEvent) {
    event.preventDefault();
    const ok = await op.run(
      'Creating network',
      () =>
        call('network.create', engine, {
          name: name.trim(),
          ...(driver.trim() ? { driver: driver.trim() } : {}),
          ...(engine === 'linux' && dnsInhibition !== 'global'
            ? { options: { 'com.ucd.dns-inhibition': dnsInhibition } }
            : {}),
          internal,
          attachable,
        }).then(() => undefined),
      `${name} created.`,
    );
    if (ok) {
      setCreate(false);
      setName('');
      setDriver('');
      setDNSInhibition('global');
      setInternal(false);
      setAttachable(false);
      list.refresh();
    }
  }
  async function attach(event: FormEvent) {
    event.preventDefault();
    if (!connect) return;
    const method = connect.disconnect
      ? 'network.disconnect'
      : 'network.connect';
    const ok = await op.run(
      method,
      () =>
        call(method, connect.item.engine, {
          id: connect.item.Id,
          container: container.trim(),
          ...(!connect.disconnect ? { aliases: lines(aliases) } : {}),
        }).then(() => undefined),
      `${connect.disconnect ? 'Disconnected' : 'Connected'} ${container}.`,
    );
    if (ok) {
      setConnect(null);
      setContainer('');
      setAliases('');
      list.refresh();
    }
  }
  async function inspect(item: Network) {
    await op.run(`Inspecting ${item.Name}`, async () =>
      setOutput({
        title: `Network · ${item.Name}`,
        value: await call('network.inspect', item.engine, { id: item.Id }),
      }),
    );
  }
  async function removeNetwork(item: Network) {
    const ok = await op.run(
      `Removing ${item.Name}`,
      () =>
        call('network.remove', item.engine, { id: item.Id }).then(
          () => undefined,
        ),
      `${item.Name} removed.`,
    );
    if (ok) {
      setRemove(null);
      list.refresh();
    }
  }
  return (
    <>
      <SectionTitle
        eyebrow="CONNECTIVITY"
        title="Networks"
        description="Inspect networks and connect containers on either engine."
      >
        <button className="button secondary" onClick={list.refresh}>
          ↻ &nbsp;Refresh
        </button>
        <button
          className="button primary"
          disabled={!canOperate}
          onClick={() => {
            op.clear();
            setCreate(true);
          }}
        >
          ＋ &nbsp;Create network
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <section className="panel table-panel">
        <div className="toolbar">
          <div>
            <h2>Network inventory</h2>
            <p>Networks are scoped to their engine.</p>
          </div>
        </div>
        <LoadState {...list} retry={list.refresh} />
        <InventoryErrors errors={list.data?.errors} />
        {list.data?.items.length === 0 ? (
          <Empty title="No networks reported" />
        ) : (
          <div
            className="table-scroll"
            role="region"
            tabIndex={0}
            aria-label="Network inventory"
          >
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Engine</th>
                  <th>Driver</th>
                  <th>Scope</th>
                  <th>Options</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.data?.items.map((item) => (
                  <tr key={`${item.engine}-${item.Id}`}>
                    <td>
                      <strong>{item.Name}</strong>
                    </td>
                    <td>
                      <span className="engine-tag">{item.engine}</span>
                    </td>
                    <td>{item.Driver}</td>
                    <td>{item.Scope}</td>
                    <td>
                      {[
                        item.Internal && 'Internal',
                        item.Attachable && 'Attachable',
                      ]
                        .filter(Boolean)
                        .join(', ') || '—'}
                    </td>
                    <td>
                      <div className="row-actions">
                        <button
                          disabled={!!op.pending}
                          onClick={() => void inspect(item)}
                        >
                          Inspect
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => {
                            op.clear();
                            setConnect({ item, disconnect: false });
                          }}
                        >
                          Connect
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => {
                            op.clear();
                            setConnect({ item, disconnect: true });
                          }}
                        >
                          Disconnect
                        </button>
                        <button
                          className="danger-text"
                          disabled={!canOperate || !!op.pending}
                          onClick={() => setRemove(item)}
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
        <Modal title="Create network" close={() => setCreate(false)}>
          <form
            className="form-stack"
            onSubmit={(event) => void createNetwork(event)}
          >
            <Feedback error={op.error} pending={op.pending} />
            <EngineSelect value={engine} onChange={setEngine} />
            <label>
              Name
              <input
                required
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            </label>
            <label>
              Driver{' '}
              <span className="muted">Optional; engine default when blank</span>
              <input
                value={driver}
                onChange={(event) => setDriver(event.target.value)}
              />
            </label>
            {engine === 'linux' && (
              <>
                <label>
                  DNS inhibition override
                  <select
                    value={dnsInhibition}
                    onChange={(event) =>
                      setDNSInhibition(
                        event.target.value as typeof dnsInhibition,
                      )
                    }
                  >
                    <option value="global">Use global setting</option>
                    <option value="auto">Automatic</option>
                    <option value="none">No filtering</option>
                    <option value="ipv4">Inhibit IPv4 (A) records</option>
                    <option value="ipv6">Inhibit IPv6 (AAAA) records</option>
                  </select>
                </label>
                <p className="muted small">
                  Applies to this Linux network. Windows networks use HNS.
                </p>
              </>
            )}
            <label className="check-row">
              <input
                type="checkbox"
                checked={internal}
                onChange={(event) => setInternal(event.target.checked)}
              />{' '}
              Internal network
            </label>
            <label className="check-row">
              <input
                type="checkbox"
                checked={attachable}
                onChange={(event) => setAttachable(event.target.checked)}
              />{' '}
              Attachable
            </label>
            <div className="modal-actions">
              <button
                type="button"
                className="button secondary"
                onClick={() => setCreate(false)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={!!op.pending}>
                Create network
              </button>
            </div>
          </form>
        </Modal>
      )}
      {connect && (
        <Modal
          title={`${connect.disconnect ? 'Disconnect from' : 'Connect to'} ${connect.item.Name}`}
          close={() => setConnect(null)}
        >
          <form className="form-stack" onSubmit={(event) => void attach(event)}>
            <Feedback error={op.error} pending={op.pending} />
            <label>
              Container name or ID
              <input
                required
                value={container}
                onChange={(event) => setContainer(event.target.value)}
              />
            </label>
            {!connect.disconnect && (
              <label>
                Aliases <span className="muted">One per line, optional</span>
                <textarea
                  value={aliases}
                  onChange={(event) => setAliases(event.target.value)}
                />
              </label>
            )}
            <div className="modal-actions">
              <button
                type="button"
                className="button secondary"
                onClick={() => setConnect(null)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={!!op.pending}>
                {connect.disconnect ? 'Disconnect' : 'Connect'}
              </button>
            </div>
          </form>
        </Modal>
      )}
      {remove && (
        <Confirm
          title={`Remove network ${remove.Name}?`}
          detail={`Remove this network from the ${remove.engine} engine. Connected containers must be disconnected first.`}
          action="Remove network"
          pending={!!op.pending}
          error={op.error}
          close={() => setRemove(null)}
          confirm={() => void removeNetwork(remove)}
        />
      )}
      {output && (
        <OutputModal
          title={output.title}
          value={output.value}
          close={() => setOutput(null)}
        />
      )}
    </>
  );
}
