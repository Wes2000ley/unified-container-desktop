import { useCallback, useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { bytes, call } from './api';
import type { Container, Engine, ListResult, Volume } from './api';
import {
  decodeText,
  encodeText,
  EngineSelect,
  InventoryErrors,
} from './resource-utils';
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
import { downloadTransfer, uploadTransfer } from './transfers';

const loadVolumes = () => call<ListResult<Volume>>('volume.list');
type Entry = {
  name: string;
  path: string;
  type: string;
  size: number;
  modified: string;
};
type VolumeDetails = { volume: Volume; containers: Container[] };
export function Volumes({
  revision,
  canOperate,
}: {
  revision: number;
  canOperate: boolean;
}) {
  const list = useData(loadVolumes, revision);
  const op = useOperation();
  const [remove, setRemove] = useState<Volume | null>(null);
  const [detail, setDetail] = useState<Volume | null>(null);
  const [create, setCreate] = useState(false);
  const [name, setName] = useState('');
  const [driver, setDriver] = useState('');
  const [engine, setEngine] = useState<Engine>('linux');
  async function createVolume(event: FormEvent) {
    event.preventDefault();
    const ok = await op.run(
      'Creating volume',
      () =>
        call('volume.create', engine, {
          name: name.trim(),
          ...(driver.trim() ? { driver: driver.trim() } : {}),
        }).then(() => undefined),
      `${name} created.`,
    );
    if (ok) {
      setCreate(false);
      setName('');
      setDriver('');
      list.refresh();
    }
  }
  async function removeVolume(item: Volume) {
    const ok = await op.run(
      `Removing ${item.name}`,
      () =>
        call('volume.remove', item.engine, { name: item.name }).then(
          () => undefined,
        ),
      `${item.name} removed.`,
    );
    if (ok) {
      setRemove(null);
      list.refresh();
    }
  }
  if (detail)
    return (
      <VolumeDetail
        item={detail}
        canOperate={canOperate}
        close={() => {
          setDetail(null);
          list.refresh();
        }}
      />
    );
  return (
    <>
      <SectionTitle
        eyebrow="PERSISTENT STORAGE"
        title="Volumes"
        description="Inspect and manage persistent storage across both engines."
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
          ＋ &nbsp;Create volume
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <section className="panel table-panel">
        <div className="toolbar">
          <div>
            <h2>Volume inventory</h2>
            <p>Volumes are only removed when explicitly requested</p>
          </div>
        </div>
        <LoadState {...list} retry={list.refresh} />
        <InventoryErrors errors={list.data?.errors} />
        {list.data?.items.length === 0 ? (
          <Empty title="No volumes reported">
            Create a volume or attach one to a container.
          </Empty>
        ) : (
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label="Volume inventory"
          >
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Engine</th>
                  <th>Driver</th>
                  <th>Mountpoint</th>
                  <th>Usage</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.data?.items.map((item) => (
                  <tr key={`${item.engine}-${item.name}`}>
                    <td>
                      <button
                        className="table-action"
                        onClick={() => setDetail(item)}
                      >
                        {item.name}
                      </button>
                    </td>
                    <td>
                      <span className="engine-tag">{item.engine}</span>
                    </td>
                    <td>{item.driver}</td>
                    <td className="mono small">{item.mountpoint || '—'}</td>
                    <td>{item.usageCount < 0 ? 'Unknown' : item.usageCount}</td>
                    <td>
                      <div className="row-actions">
                        <button onClick={() => setDetail(item)}>Details</button>
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
        <Modal title="Create volume" close={() => setCreate(false)}>
          <form
            className="form-stack"
            onSubmit={(event) => void createVolume(event)}
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
            <div className="modal-actions">
              <button
                type="button"
                className="button secondary"
                disabled={!!op.pending}
                onClick={() => setCreate(false)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={!!op.pending}>
                Create volume
              </button>
            </div>
          </form>
        </Modal>
      )}
      {remove && (
        <Confirm
          title={`Remove volume ${remove.name}?`}
          detail={`Permanently remove ${remove.name} from the ${remove.engine} engine. Its data will be lost.`}
          action="Remove volume"
          pending={!!op.pending}
          error={op.error}
          close={() => setRemove(null)}
          confirm={() => void removeVolume(remove)}
        />
      )}
    </>
  );
}

function VolumeDetail({
  item,
  canOperate,
  close,
}: {
  item: Volume;
  canOperate: boolean;
  close: () => void;
}) {
  const op = useOperation();
  const [path, setPath] = useState('.');
  const [refresh, setRefresh] = useState(0);
  const [name, setName] = useState('');
  const [clone, setClone] = useState(false);
  const [cloneTarget, setCloneTarget] = useState('');
  const [editor, setEditor] = useState<{
    path: string;
    content: string;
  } | null>(null);
  const [restore, setRestore] = useState<File | null>(null);
  const [transferProgress, setTransferProgress] = useState<{
    sent: number;
    total: number;
  } | null>(null);
  const [uploading, setUploading] = useState(false);
  const uploadAbort = useRef<AbortController | null>(null);
  useEffect(() => () => uploadAbort.current?.abort(), []);
  const [confirm, setConfirm] = useState<
    'restore' | 'empty' | 'save' | Entry | null
  >(null);
  const [output, setOutput] = useState<unknown>();
  const inspect = useCallback(
    () =>
      call<VolumeDetails>('volume.inspect', item.engine, { name: item.name }),
    [item.engine, item.name],
  );
  const getFiles = useCallback(
    () =>
      canOperate
        ? call<{ entries: Entry[] }>('volume.files.list', item.engine, {
            name: item.name,
            path,
          })
        : Promise.resolve({ entries: [] }),
    [canOperate, item.engine, item.name, path],
  );
  const details = useData(inspect, refresh);
  const files = useData(getFiles, refresh);
  function reload() {
    setRefresh((value) => value + 1);
  }
  function navigatePath(next: string) {
    setPath(next);
    reload();
  }
  async function send(
    operation: string,
    params: Record<string, unknown>,
    file: File,
  ) {
    const controller = new AbortController();
    uploadAbort.current = controller;
    setUploading(true);
    try {
      await uploadTransfer(item.engine, operation, params, file, {
        signal: controller.signal,
        onProgress: (sent, total) => setTransferProgress({ sent, total }),
      });
    } finally {
      uploadAbort.current = null;
      setUploading(false);
    }
  }
  async function execute(
    method: string,
    params: Record<string, unknown> = {},
    success?: string,
  ) {
    const ok = await op.run(
      method,
      async () => {
        await call(method, item.engine, { name: item.name, ...params });
      },
      success,
    );
    if (ok) {
      setConfirm(null);
      reload();
    }
    return ok;
  }
  async function download(method: string, filePath?: string) {
    await op.run('Preparing download', async () => {
      const base = filePath?.split('/').at(-1) || item.name;
      const filename =
        method === 'volume.backup' || method === 'volume.files.archive'
          ? `${base}.tar`
          : base;
      await downloadTransfer(
        item.engine,
        method,
        { name: item.name, ...(filePath ? { path: filePath } : {}) },
        filename,
      );
    });
  }
  async function edit(entry: Entry) {
    await op.run(`Opening ${entry.path}`, async () => {
      const result = await call<{ data: string }>(
        'volume.files.read',
        item.engine,
        { name: item.name, path: entry.path },
      );
      setEditor({ path: entry.path, content: decodeText(result.data) });
    });
  }
  function save(event: FormEvent) {
    event.preventDefault();
    if (editor) {
      op.clear();
      setConfirm('save');
    }
  }
  async function upload(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const input = event.currentTarget.elements.namedItem(
      'upload',
    ) as HTMLInputElement;
    const file = input.files?.[0];
    if (!file) return;
    if (
      !window.confirm(
        `Upload ${file.name}? An existing file with that name will be replaced.`,
      )
    )
      return;
    setTransferProgress(null);
    const ok = await op.run(
      'Uploading file',
      async () => {
        await send(
          'volume.files.upload',
          {
            name: item.name,
            path: path === '.' ? file.name : `${path}/${file.name}`,
          },
          file,
        );
      },
      'File uploaded.',
    );
    if (ok) {
      input.value = '';
      reload();
    }
  }
  async function confirmAction() {
    if (confirm === 'save' && editor) {
      const ok = await execute(
        'volume.files.write',
        { path: editor.path, data: encodeText(editor.content) },
        'File saved.',
      );
      if (ok) setEditor(null);
    } else if (confirm === 'empty')
      await execute('volume.empty', { confirmed: true }, 'Volume emptied.');
    else if (confirm === 'restore' && restore) {
      setTransferProgress(null);
      const ok = await op.run(
        'Restoring volume',
        async () => {
          await send(
            'volume.restore',
            { name: item.name, confirmed: true },
            restore,
          );
        },
        'Volume restored.',
      );
      if (ok) {
        setConfirm(null);
        setRestore(null);
        reload();
      }
    } else if (confirm && typeof confirm === 'object')
      await execute(
        'volume.files.remove',
        { path: confirm.path, recursive: confirm.type === 'directory' },
        'Entry removed.',
      );
  }
  return (
    <>
      <SectionTitle
        eyebrow="PERSISTENT STORAGE"
        title={item.name}
        description={`${item.engine} volume · ${item.driver}`}
      >
        <button className="button secondary" onClick={close}>
          ← Back to volumes
        </button>
        <button
          className="button secondary"
          disabled={!canOperate || !!op.pending}
          onClick={() => void download('volume.backup')}
        >
          Back up
        </button>
        <button
          className="button secondary"
          disabled={!canOperate || !!op.pending}
          onClick={() => setClone(true)}
        >
          Clone
        </button>
        <button
          className="button danger"
          disabled={!canOperate || !!op.pending}
          onClick={() => setConfirm('empty')}
        >
          Empty volume
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      {(op.pending === 'Uploading file' || op.pending === 'Restoring volume') &&
        transferProgress && (
          <p role="status" className="muted">
            Transferred {bytes(transferProgress.sent)} of{' '}
            {bytes(transferProgress.total)}.
          </p>
        )}
      <section className="panel resource-section">
        <div className="panel-heading">
          <div>
            <h2>Volume details and attachments</h2>
            <p>
              Engine-reported properties and containers using this volume.
              Backups and clones are live snapshots and may be inconsistent
              while applications write to the volume.
            </p>
          </div>
          <button
            className="button quiet"
            disabled={!details.data}
            onClick={() => setOutput(details.data)}
          >
            Inspect JSON
          </button>
        </div>
        <LoadState {...details} retry={details.refresh} />
        {details.data && (
          <>
            <div className="detail-lines">
              <div>
                Driver<strong>{details.data.volume.driver}</strong>
              </div>
              <div>
                Mountpoint
                <strong>{details.data.volume.mountpoint || '—'}</strong>
              </div>
              <div>
                Size<strong>{bytes(details.data.volume.size)}</strong>
              </div>
              <div>
                Containers<strong>{details.data.containers.length}</strong>
              </div>
            </div>
            <h3 className="resource-subheading">Attached containers</h3>
            {details.data.containers.length ? (
              <div className="simple-list">
                {details.data.containers.map((container) => (
                  <div key={container.id}>
                    <strong>
                      {container.name || container.id.slice(0, 12)}
                    </strong>
                    <span className="muted">{container.state}</span>
                  </div>
                ))}
              </div>
            ) : (
              <p className="muted">No attached containers.</p>
            )}
          </>
        )}
      </section>
      {canOperate && (
        <section className="panel resource-section">
          <div className="panel-heading">
            <div>
              <h2>Files</h2>
              <p>
                Path: {path} · Text editing is limited to 8 MiB. File operations
                use a temporary utility container and may pull its image on
                first use.
              </p>
            </div>
            <div className="action-wrap">
              <button
                className="button quiet"
                disabled={path === '.' || !!op.pending}
                onClick={() =>
                  navigatePath(
                    path.includes('/')
                      ? path.slice(0, path.lastIndexOf('/'))
                      : '.',
                  )
                }
              >
                Up
              </button>
              <button className="button quiet" onClick={reload}>
                Refresh
              </button>
            </div>
          </div>
          <LoadState {...files} retry={files.refresh} />
          {!files.loading &&
          !files.error &&
          files.data &&
          !files.data.entries?.length ? (
            <Empty title="Directory is empty" />
          ) : (
            <div className="simple-list">
              {!files.loading &&
                !files.error &&
                files.data?.entries?.map((entry) => (
                  <div key={entry.path}>
                    <span>
                      {entry.name}{' '}
                      <span className="muted">
                        · {entry.type}
                        {entry.type === 'file' ? ` · ${bytes(entry.size)}` : ''}
                      </span>
                    </span>
                    <div className="row-actions">
                      {entry.type === 'directory' ? (
                        <button onClick={() => navigatePath(entry.path)}>
                          Open
                        </button>
                      ) : (
                        <button
                          disabled={!!op.pending}
                          onClick={() => void edit(entry)}
                        >
                          Edit text
                        </button>
                      )}
                      <button
                        disabled={!!op.pending}
                        onClick={() =>
                          void download(
                            entry.type === 'directory'
                              ? 'volume.files.archive'
                              : 'volume.files.download',
                            entry.path,
                          )
                        }
                      >
                        Download
                      </button>
                      <button
                        className="danger-text"
                        disabled={!!op.pending}
                        onClick={() => setConfirm(entry)}
                      >
                        Delete
                      </button>
                    </div>
                  </div>
                ))}
            </div>
          )}
          <form
            className="resource-inline"
            onSubmit={(event) => {
              event.preventDefault();
              if (name.trim())
                void execute(
                  'volume.files.mkdir',
                  {
                    path: path === '.' ? name.trim() : `${path}/${name.trim()}`,
                  },
                  'Directory created.',
                ).then((ok) => {
                  if (ok) setName('');
                });
            }}
          >
            <label>
              New directory
              <input
                required
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            </label>
            <button className="button secondary" disabled={!!op.pending}>
              Create directory
            </button>
          </form>
          <form
            className="resource-inline"
            onSubmit={(event) => void upload(event)}
          >
            <label>
              Upload file
              <input type="file" name="upload" required />
            </label>
            <button className="button secondary" disabled={!!op.pending}>
              Upload
            </button>
            {uploading && op.pending === 'Uploading file' && (
              <button
                type="button"
                className="button secondary"
                onClick={() => uploadAbort.current?.abort()}
              >
                Cancel upload
              </button>
            )}
          </form>
          <div className="resource-inline">
            <label>
              Restore backup{' '}
              <span className="muted">Replaces all volume contents</span>
              <input
                type="file"
                onChange={(event) =>
                  setRestore(event.target.files?.[0] || null)
                }
              />
            </label>
            <button
              className="button danger"
              disabled={!restore || !!op.pending}
              onClick={() => setConfirm('restore')}
            >
              Restore backup
            </button>
          </div>
        </section>
      )}
      {clone && (
        <Modal title={`Clone ${item.name}`} close={() => setClone(false)}>
          <form
            className="form-stack"
            onSubmit={(event) => {
              event.preventDefault();
              if (cloneTarget.trim())
                void execute(
                  'volume.clone',
                  { target: cloneTarget.trim() },
                  'Volume cloned.',
                ).then((ok) => {
                  if (ok) {
                    setClone(false);
                    setCloneTarget('');
                  }
                });
            }}
          >
            <Feedback error={op.error} pending={op.pending} />
            <p className="muted">
              This is a live snapshot. Stop writes to the volume first if you
              need a consistent copy.
            </p>
            <label>
              New volume name
              <input
                required
                value={cloneTarget}
                onChange={(event) => setCloneTarget(event.target.value)}
              />
            </label>
            <div className="modal-actions">
              <button
                type="button"
                className="button secondary"
                onClick={() => setClone(false)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={!!op.pending}>
                Clone
              </button>
            </div>
          </form>
        </Modal>
      )}
      {editor && (
        <Modal title={`Edit ${editor.path}`} close={() => setEditor(null)}>
          <form className="form-stack" onSubmit={save}>
            <Feedback error={op.error} pending={op.pending} />
            <label>
              File contents
              <textarea
                className="code-input editor"
                value={editor.content}
                onChange={(event) =>
                  setEditor({ ...editor, content: event.target.value })
                }
              />
            </label>
            <div className="modal-actions">
              <button
                type="button"
                className="button secondary"
                onClick={() => setEditor(null)}
              >
                Cancel
              </button>
              <button className="button primary" disabled={!!op.pending}>
                Save file
              </button>
            </div>
          </form>
        </Modal>
      )}
      {confirm && (
        <Confirm
          title={
            confirm === 'restore'
              ? 'Restore backup?'
              : confirm === 'empty'
                ? 'Empty volume?'
                : confirm === 'save'
                  ? 'Overwrite file?'
                  : `Delete ${confirm.name}?`
          }
          detail={
            <>
              {confirm === 'restore'
                ? `All existing contents of ${item.name} will be replaced. A failure can leave the volume empty or partially restored.`
                : confirm === 'empty'
                  ? `All contents of ${item.name} will be permanently removed. A failure can leave the volume partially emptied.`
                  : confirm === 'save'
                    ? `Replace the contents of ${editor?.path || 'this file'} with your edited text.`
                    : `Permanently delete ${confirm.path}${confirm.type === 'directory' ? ' and its contents' : ''}.`}
              {op.pending === 'Restoring volume' && transferProgress && (
                <p role="status">
                  Transferred {bytes(transferProgress.sent)} of{' '}
                  {bytes(transferProgress.total)}.
                </p>
              )}
              {op.pending === 'Restoring volume' && uploading && (
                <button
                  type="button"
                  className="button secondary"
                  onClick={() => uploadAbort.current?.abort()}
                >
                  Cancel restore
                </button>
              )}
            </>
          }
          action={
            confirm === 'restore'
              ? 'Restore backup'
              : confirm === 'empty'
                ? 'Empty volume'
                : confirm === 'save'
                  ? 'Overwrite file'
                  : 'Delete entry'
          }
          pending={!!op.pending}
          error={op.error}
          close={() => setConfirm(null)}
          confirm={() => void confirmAction()}
        />
      )}
      {output !== undefined && (
        <OutputModal
          title={`Inspect · ${item.name}`}
          value={output}
          close={() => setOutput(undefined)}
        />
      )}
    </>
  );
}
