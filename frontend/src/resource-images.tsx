import { Icon } from './icon';
import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { createTarPacker } from 'modern-tar';
import { bytes, call } from './api';
import type { Engine, Event, Image, ListResult } from './api';
import { CreateContainer } from './resource-container-create';
import { EngineSelect, InventoryErrors, lines, pairs } from './resource-utils';
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
import {
  downloadTransfer,
  uploadStreamTransfer,
  uploadTransfer,
} from './transfers';

const loadImages = () => call<ListResult<Image>>('image.list');
type Dialog = 'pull' | 'tag' | 'push' | 'load' | 'import' | 'build' | null;
export function Images({
  revision,
  canOperate,
  events = [],
}: {
  revision: number;
  canOperate: boolean;
  events?: Event[];
}) {
  const list = useData(loadImages, revision);
  const op = useOperation();
  const [dialog, setDialog] = useState<Dialog>(null);
  const [engine, setEngine] = useState<Engine>('linux');
  const [reference, setReference] = useState('');
  const [target, setTarget] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [files, setFiles] = useState<File[]>([]);
  const [dockerfile, setDockerfile] = useState('Dockerfile');
  const [buildArgs, setBuildArgs] = useState('');
  const [executables, setExecutables] = useState('');
  const [operationId, setOperationId] = useState('');
  const [consoleOutput, setConsoleOutput] = useState('');
  const [uploaded, setUploaded] = useState<{
    sent: number;
    total: number;
  } | null>(null);
  const [uploading, setUploading] = useState(false);
  const uploadAbort = useRef<AbortController | null>(null);
  useEffect(() => () => uploadAbort.current?.abort(), []);
  const [remove, setRemove] = useState<Image | null>(null);
  const [run, setRun] = useState<Image | null>(null);
  const [output, setOutput] = useState<{
    title: string;
    value: unknown;
  } | null>(null);
  const progress = events
    .filter(
      (event) =>
        operationId &&
        event.type === 'image.progress' &&
        event.project === operationId,
    )
    .map((event) => event.message)
    .join('');
  function open(next: Exclude<Dialog, null>, item?: Image) {
    op.clear();
    setDialog(next);
    setEngine(item?.engine || 'linux');
    setReference(item?.tags?.[0] || item?.id || '');
    setTarget('');
    setFile(null);
    setFiles([]);
    setDockerfile('Dockerfile');
    setBuildArgs('');
    setExecutables('');
    setOperationId('');
    setConsoleOutput('');
    setUploaded(null);
  }
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!dialog) return;
    const id = crypto.randomUUID();
    setOperationId(id);
    setConsoleOutput('');
    const ok = await op.run(
      `${dialog} image`,
      async () => {
        let result: { output?: string } | undefined;
        if (dialog === 'pull')
          result = await call('image.pull', engine, {
            reference: reference.trim(),
          });
        if (dialog === 'tag')
          result = await call('image.tag', engine, {
            reference,
            target: target.trim(),
          });
        if (dialog === 'push')
          result = await call('image.push', engine, {
            reference: reference.trim(),
            operationId: id,
          });
        if (dialog === 'load' || dialog === 'import') {
          if (!file) throw new Error('Choose an archive to upload.');
          const controller = new AbortController();
          uploadAbort.current = controller;
          setUploading(true);
          try {
            result = (await uploadTransfer(
              engine,
              `image.${dialog}`,
              {
                operationId: id,
                ...(dialog === 'import' ? { reference: reference.trim() } : {}),
              },
              file,
              {
                signal: controller.signal,
                onProgress: (sent, total) => setUploaded({ sent, total }),
              },
            )) as { output?: string };
          } finally {
            uploadAbort.current = null;
            setUploading(false);
          }
        }
        if (dialog === 'build') {
          if (!files.length) throw new Error('Choose a build context folder.');
          const paths = files.map((entry) => ({
            entry,
            path: entry.webkitRelativePath
              ? entry.webkitRelativePath.split('/').slice(1).join('/')
              : entry.name,
          }));
          const recipe = paths.find(
            (entry) => entry.path === dockerfile.trim(),
          );
          if (!recipe)
            throw new Error(
              'The Dockerfile path was not found in the selected context.',
            );
          const executablePaths = lines(executables);
          if (
            executablePaths.some(
              (path) =>
                !paths.some((entry) => entry.path === path && entry !== recipe),
            )
          )
            throw new Error(
              'Each executable path must identify a file in the context.',
            );
          const metadata = {
            dockerfile: await recipe.entry.text(),
            dockerfilePath: recipe.path,
            tags: lines(reference),
            buildArgs: pairs(buildArgs),
            operationId: id,
          };
          const packer = createTarPacker();
          void (async () => {
            try {
              for (const { entry, path } of paths) {
                await entry.stream().pipeTo(
                  packer.controller.add({
                    name: path,
                    size: entry.size,
                    type: 'file',
                    mode: executablePaths.includes(path) ? 0o755 : 0o644,
                  }),
                );
              }
              packer.controller.finalize();
            } catch (error) {
              packer.controller.error(error);
            }
          })();
          const controller = new AbortController();
          uploadAbort.current = controller;
          setUploading(true);
          try {
            result = (await uploadStreamTransfer(
              engine,
              'image.build',
              metadata,
              packer.readable,
              undefined,
              {
                signal: controller.signal,
                onProgress: (sent, total) => setUploaded({ sent, total }),
              },
            )) as { output?: string };
          } finally {
            uploadAbort.current = null;
            setUploading(false);
          }
        }
        setConsoleOutput(result?.output || 'Operation completed.');
        list.refresh();
      },
      `${dialog} completed.`,
    );
    if (ok && dialog !== 'build' && dialog !== 'push') setDialog(null);
  }
  async function inspect(item: Image) {
    await op.run(`Inspecting ${item.id}`, async () => {
      const reference = item.tags?.[0] || item.id;
      const [inspect, history] = await Promise.all([
        call('image.inspect', item.engine, { reference }),
        call('image.history', item.engine, { reference }),
      ]);
      setOutput({
        title: `Image details and layers · ${reference}`,
        value: { inspect, history },
      });
    });
  }
  async function save(item: Image) {
    await op.run('Saving image', async () => {
      const reference = item.tags?.[0] || item.id;
      await downloadTransfer(
        item.engine,
        'image.save',
        { references: [reference] },
        'images.tar',
      );
    });
  }
  async function removeImage(item: Image) {
    const ok = await op.run(
      'Removing image',
      () =>
        call('image.remove', item.engine, { id: item.id }).then(
          () => undefined,
        ),
      'Image removed.',
    );
    if (ok) {
      setRemove(null);
      list.refresh();
    }
  }
  return (
    <>
      <SectionTitle
        eyebrow="RUNTIME INVENTORY"
        title="Images"
        description="Build, transfer and manage images on each engine."
      >
        <button className="button secondary" onClick={list.refresh}>
          <Icon name="refresh" size={16} /> Refresh
        </button>
        {(['load', 'import', 'build', 'pull'] as const).map((name) => (
          <button
            key={name}
            className={name === 'pull' ? 'button primary' : 'button secondary'}
            disabled={!canOperate}
            onClick={() => open(name)}
          >
            {name.charAt(0).toUpperCase() + name.slice(1)} image
          </button>
        ))}
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <section className="panel table-panel">
        <div className="toolbar">
          <div>
            <h2>Image inventory</h2>
            <p>Removal is available for images with no containers</p>
          </div>
        </div>
        <LoadState {...list} retry={list.refresh} />
        <InventoryErrors errors={list.data?.errors} />
        {list.data?.items.length === 0 ? (
          <Empty title="No images reported">
            Pull or build an image to get started.
          </Empty>
        ) : (
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label="Image inventory"
          >
            <table>
              <thead>
                <tr>
                  <th>Repository / tag</th>
                  <th>Engine</th>
                  <th>Image ID</th>
                  <th>Size</th>
                  <th>Containers</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {list.data?.items.map((item) => (
                  <tr key={`${item.engine}-${item.id}`}>
                    <td>
                      <strong>
                        {item.tags?.length
                          ? item.tags.join(', ')
                          : '<untagged>'}
                      </strong>
                    </td>
                    <td>
                      <span className="engine-tag">{item.engine}</span>
                    </td>
                    <td className="mono">
                      {item.id.replace(/^sha256:/, '').slice(0, 12)}
                    </td>
                    <td className="numeric">{bytes(item.size)}</td>
                    <td className="numeric">
                      {item.containers < 0 ? 'Unknown' : item.containers}
                    </td>
                    <td>
                      <div className="row-actions">
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => void inspect(item)}
                        >
                          Details
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => setRun(item)}
                        >
                          Run
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => open('tag', item)}
                        >
                          Tag
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => open('push', item)}
                        >
                          Push
                        </button>
                        <button
                          disabled={!canOperate || !!op.pending}
                          onClick={() => void save(item)}
                        >
                          Save
                        </button>
                        <button
                          className="danger-text"
                          disabled={
                            !canOperate || !!op.pending || item.containers !== 0
                          }
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
      {dialog && (
        <Modal
          title={`${dialog.charAt(0).toUpperCase() + dialog.slice(1)} image`}
          close={() => {
            if (!op.pending) setDialog(null);
          }}
        >
          <form className="form-stack" onSubmit={(event) => void submit(event)}>
            <Feedback
              error={op.error}
              success={op.success}
              pending={op.pending}
            />
            <EngineSelect
              value={engine}
              onChange={setEngine}
              disabled={!!op.pending}
            />
            {(dialog === 'pull' || dialog === 'push') && (
              <label>
                Image reference
                <input
                  required
                  value={reference}
                  onChange={(event) => setReference(event.target.value)}
                  placeholder="registry.example/image:tag"
                />
              </label>
            )}
            {dialog === 'tag' && (
              <>
                <label>
                  Source reference
                  <input
                    required
                    value={reference}
                    onChange={(event) => setReference(event.target.value)}
                  />
                </label>
                <label>
                  Target reference
                  <input
                    required
                    value={target}
                    onChange={(event) => setTarget(event.target.value)}
                    placeholder="repository:tag"
                  />
                </label>
              </>
            )}
            {(dialog === 'load' || dialog === 'import') && (
              <>
                <label>
                  Archive file
                  <input
                    type="file"
                    required
                    onChange={(event) =>
                      setFile(event.target.files?.[0] || null)
                    }
                  />
                </label>
                {dialog === 'import' && (
                  <label>
                    Image reference <span className="muted">Optional</span>
                    <input
                      value={reference}
                      onChange={(event) => setReference(event.target.value)}
                    />
                  </label>
                )}
              </>
            )}
            {dialog === 'build' && (
              <>
                <label>
                  Build context folder{' '}
                  <span className="muted">
                    Selected files are uploaded to the local controller before
                    .dockerignore filtering.
                  </span>
                  <input
                    type="file"
                    multiple
                    {...({ webkitdirectory: '' } as object)}
                    onChange={(event) =>
                      setFiles(Array.from(event.target.files || []))
                    }
                  />
                </label>
                <label>
                  Dockerfile path
                  <input
                    required
                    value={dockerfile}
                    onChange={(event) => setDockerfile(event.target.value)}
                  />
                </label>
                <label>
                  Tags{' '}
                  <span className="muted">One image reference per line</span>
                  <textarea
                    rows={2}
                    value={reference}
                    onChange={(event) => setReference(event.target.value)}
                  />
                </label>
                <label>
                  Build arguments{' '}
                  <span className="muted">One KEY=VALUE per line</span>
                  <textarea
                    rows={2}
                    value={buildArgs}
                    onChange={(event) => setBuildArgs(event.target.value)}
                  />
                </label>
                <label>
                  Executable files{' '}
                  <span className="muted">
                    Optional relative paths, one per line; other files use mode
                    0644.
                  </span>
                  <textarea
                    rows={2}
                    value={executables}
                    onChange={(event) => setExecutables(event.target.value)}
                  />
                </label>
              </>
            )}
            {(dialog === 'build' || dialog === 'push') &&
              (progress || consoleOutput || op.pending) && (
                <>
                  <strong>Build and transfer output</strong>
                  <pre className="output" aria-live="polite">
                    {consoleOutput || progress || 'Waiting for engine output…'}
                  </pre>
                </>
              )}
            {(dialog === 'load' || dialog === 'import' || dialog === 'build') &&
              op.pending &&
              uploaded && (
                <p role="status" className="muted">
                  Uploaded {bytes(uploaded.sent)}
                  {uploaded.total > 0 ? ` of ${bytes(uploaded.total)}` : ''}.
                </p>
              )}
            <div className="modal-actions">
              {uploading && (
                <button
                  type="button"
                  className="button secondary"
                  onClick={() => uploadAbort.current?.abort()}
                >
                  Cancel upload
                </button>
              )}
              <button
                type="button"
                className="button secondary"
                disabled={!!op.pending}
                onClick={() => setDialog(null)}
              >
                Close
              </button>
              <button className="button primary" disabled={!!op.pending}>
                {dialog.charAt(0).toUpperCase() + dialog.slice(1)} image
              </button>
            </div>
          </form>
        </Modal>
      )}
      {remove && (
        <Confirm
          title="Remove image?"
          detail={`${remove.tags?.join(', ') || remove.id} will be removed from the ${remove.engine} engine.`}
          action="Remove image"
          pending={!!op.pending}
          error={op.error}
          close={() => setRemove(null)}
          confirm={() => void removeImage(remove)}
        />
      )}
      {run && (
        <CreateContainer
          initialEngine={run.engine}
          initialImage={run.tags?.[0] || run.id}
          close={() => setRun(null)}
          onCreated={list.refresh}
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
