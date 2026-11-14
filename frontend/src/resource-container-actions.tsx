import { useState } from 'react';
import type { FormEvent } from 'react';
import { call } from './api';
import type { Container } from './api';
import { downloadTransfer } from './transfers';
import { Feedback, Modal, formatResult, useOperation } from './ui';

type Tool = 'rename' | 'update' | 'top' | 'diff' | 'commit' | 'export';
export function ContainerActions({
  container,
  close,
  changed,
}: {
  container: Container;
  close: () => void;
  changed: () => void;
}) {
  const [tool, setTool] = useState<Tool>('top');
  const [name, setName] = useState(container.name);
  const [memory, setMemory] = useState('');
  const [cpus, setCpus] = useState('');
  const [reference, setReference] = useState('');
  const [author, setAuthor] = useState('');
  const [comment, setComment] = useState('');
  const [output, setOutput] = useState<unknown>();
  const op = useOperation();
  async function submit(event: FormEvent) {
    event.preventDefault();
    await op.run(
      `Running ${tool}`,
      async () => {
        if (tool === 'export') {
          await downloadTransfer(
            container.engine,
            'container.export',
            { id: container.id },
            `${container.name}.tar`,
          );
          return;
        }
        let params: Record<string, unknown> = { id: container.id };
        if (tool === 'rename') params = { ...params, name: name.trim() };
        if (tool === 'update') {
          if (!memory && !cpus)
            throw new Error('Enter at least one resource limit.');
          params = {
            ...params,
            resources: {
              ...(memory ? { Memory: Number(memory) * 1024 * 1024 } : {}),
              ...(cpus
                ? { NanoCpus: Math.round(Number(cpus) * 1_000_000_000) }
                : {}),
            },
          };
        }
        if (tool === 'commit')
          params = {
            ...params,
            reference: reference.trim(),
            author: author.trim(),
            comment,
          };
        const result = await call<unknown>(
          `container.${tool}`,
          container.engine,
          params,
        );
        setOutput(result);
        if (tool === 'rename' || tool === 'update' || tool === 'commit')
          changed();
      },
      tool === 'export' ? undefined : `${tool} completed.`,
    );
  }
  return (
    <Modal title={`Container tools · ${container.name}`} close={close}>
      <form className="form-stack" onSubmit={(event) => void submit(event)}>
        <Feedback error={op.error} success={op.success} pending={op.pending} />
        <label>
          Operation
          <select
            value={tool}
            onChange={(event) => {
              setTool(event.target.value as Tool);
              setOutput(undefined);
              op.clear();
            }}
          >
            <option value="top">Processes</option>
            <option value="diff">Filesystem changes</option>
            <option value="rename">Rename</option>
            <option value="update">Update resource limits</option>
            <option value="commit">Commit image</option>
            <option value="export">Export filesystem</option>
          </select>
        </label>
        {tool === 'diff' && container.engine === 'windows' && (
          <p className="muted">
            Stop the Windows container before viewing filesystem changes.
            Windows Docker cannot diff a running container.
          </p>
        )}
        {tool === 'rename' && (
          <label>
            New name
            <input
              required
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </label>
        )}
        {tool === 'update' && (
          <div className="form-grid">
            <label>
              Memory limit (MiB)
              <input
                type="number"
                min="1"
                step="1"
                value={memory}
                onChange={(event) => setMemory(event.target.value)}
              />
            </label>
            <label>
              CPU limit
              <input
                type="number"
                min="0.01"
                step="0.01"
                value={cpus}
                onChange={(event) => setCpus(event.target.value)}
              />
            </label>
          </div>
        )}
        {tool === 'commit' && (
          <>
            {container.engine === 'windows' && (
              <p className="muted">
                Stop the Windows container before committing it. Windows Docker
                cannot commit a running container.
              </p>
            )}
            <label>
              Image reference
              <input
                value={reference}
                onChange={(event) => setReference(event.target.value)}
                placeholder="repository:tag"
              />
            </label>
            <div className="form-grid">
              <label>
                Author
                <input
                  value={author}
                  onChange={(event) => setAuthor(event.target.value)}
                />
              </label>
              <label>
                Comment
                <input
                  value={comment}
                  onChange={(event) => setComment(event.target.value)}
                />
              </label>
            </div>
          </>
        )}
        {tool === 'export' && (
          <p className="muted">
            {container.engine === 'windows'
              ? 'Windows Docker does not support container filesystem export. Use Save in Images to download a Windows image.'
              : 'Export downloads the container filesystem; mounted volume data is excluded.'}
          </p>
        )}
        {output !== undefined && (
          <pre className="output">{formatResult(output)}</pre>
        )}
        <div className="modal-actions">
          <button
            type="button"
            className="button secondary"
            onClick={close}
            disabled={!!op.pending}
          >
            Close
          </button>
          <button
            className="button primary"
            disabled={
              !!op.pending ||
              (tool === 'export' && container.engine === 'windows')
            }
          >
            {tool === 'top' || tool === 'diff'
              ? 'Load'
              : tool === 'export'
                ? 'Download export'
                : 'Apply'}
          </button>
        </div>
      </form>
    </Modal>
  );
}
