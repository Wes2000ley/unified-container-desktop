import { useState } from 'react';
import type { FormEvent } from 'react';
import { call } from './api';
import type { Engine } from './api';
import { EngineSelect, lines } from './resource-utils';
import { Feedback, Modal, useOperation } from './ui';

export function CreateContainer({
  initialImage = '',
  initialEngine = 'linux',
  close,
  onCreated,
}: {
  initialImage?: string;
  initialEngine?: Engine;
  close: () => void;
  onCreated: () => void;
}) {
  const [engine, setEngine] = useState<Engine>(initialEngine);
  const [image, setImage] = useState(initialImage);
  const [name, setName] = useState('');
  const [ports, setPorts] = useState('');
  const [environment, setEnvironment] = useState('');
  const [volumes, setVolumes] = useState('');
  const [command, setCommand] = useState('');
  const [restart, setRestart] = useState('no');
  const [memory, setMemory] = useState('');
  const [cpus, setCpus] = useState('');
  const [warnings, setWarnings] = useState<string[]>([]);
  const [completed, setCompleted] = useState(false);
  const op = useOperation();
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const run =
      (event.nativeEvent as SubmitEvent).submitter?.getAttribute('value') ===
      'run';
    let hadWarnings = false;
    const ok = await op.run(
      run ? 'Creating and starting container' : 'Creating container',
      async () => {
        const exposed: Record<string, object> = {};
        const bindings: Record<string, { HostPort: string }[]> = {};
        for (const line of lines(ports)) {
          const match = /^(\d+):(\d+)(?:\/(tcp|udp))?$/.exec(line);
          if (
            !match ||
            Number(match[1]) < 1 ||
            Number(match[1]) > 65535 ||
            Number(match[2]) < 1 ||
            Number(match[2]) > 65535
          )
            throw new Error(
              'Ports must use HOST:CONTAINER[/tcp|udp], with ports from 1 to 65535.',
            );
          const key = `${match[2]}/${match[3] || 'tcp'}`;
          exposed[key] = {};
          (bindings[key] ||= []).push({ HostPort: match[1]! });
        }
        const env = environment.split(/\r?\n/).filter((line) => line.trim());
        if (env.some((line) => !/^[^=]+=.*/.test(line)))
          throw new Error(
            'Environment entries must use KEY=VALUE, one per line.',
          );
        let cmd: string[] | undefined;
        if (command.trim()) {
          const parsed: unknown = JSON.parse(command);
          if (
            !Array.isArray(parsed) ||
            !parsed.every((part) => typeof part === 'string')
          )
            throw new Error('Command must be a JSON array of strings.');
          cmd = parsed;
        }
        const result = await call<{ warnings?: string[] }>(
          run ? 'container.run' : 'container.create',
          engine,
          {
            name: name.trim(),
            config: {
              Image: image.trim(),
              ...(cmd ? { Cmd: cmd } : {}),
              ...(env.length ? { Env: env } : {}),
              ...(Object.keys(exposed).length ? { ExposedPorts: exposed } : {}),
            },
            hostConfig: {
              ...(Object.keys(bindings).length
                ? { PortBindings: bindings }
                : {}),
              ...(lines(volumes).length ? { Binds: lines(volumes) } : {}),
              RestartPolicy: { Name: restart },
              ...(memory ? { Memory: Number(memory) * 1024 * 1024 } : {}),
              ...(cpus
                ? { NanoCpus: Math.round(Number(cpus) * 1_000_000_000) }
                : {}),
            },
          },
        );
        if (result.warnings?.length) {
          hadWarnings = true;
          setWarnings(result.warnings);
          setCompleted(true);
        }
      },
    );
    if (ok) {
      onCreated();
      if (!hadWarnings) close();
    }
  }
  return (
    <Modal title="Create container" close={close}>
      <form className="form-stack" onSubmit={(event) => void submit(event)}>
        <Feedback error={op.error} pending={op.pending} />
        {warnings.length > 0 && (
          <div className="notice progress" role="status">
            Container created. Engine warnings: {warnings.join(' · ')}
          </div>
        )}
        <div className="form-grid">
          <label>
            Image
            <input
              required
              value={image}
              onChange={(event) => setImage(event.target.value)}
              placeholder="nginx:latest"
            />
          </label>
          <EngineSelect value={engine} onChange={setEngine} />
        </div>
        <label>
          Name <span className="muted">Optional</span>
          <input
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </label>
        <div className="form-grid">
          <label>
            Published ports{' '}
            <span className="muted">
              One HOST:CONTAINER[/protocol] per line
            </span>
            <textarea
              rows={3}
              value={ports}
              onChange={(event) => setPorts(event.target.value)}
              placeholder={'8080:80/tcp'}
            />
          </label>
          <label>
            Environment <span className="muted">One KEY=VALUE per line</span>
            <textarea
              rows={3}
              value={environment}
              onChange={(event) => setEnvironment(event.target.value)}
            />
          </label>
        </div>
        <label>
          Volume binds{' '}
          <span className="muted">
            Docker bind syntax, one per line, for example data:/data
          </span>
          <textarea
            rows={3}
            value={volumes}
            onChange={(event) => setVolumes(event.target.value)}
          />
        </label>
        <label>
          Command{' '}
          <span className="muted">
            Optional JSON array, for example ["server", "--verbose"]
          </span>
          <input
            value={command}
            onChange={(event) => setCommand(event.target.value)}
          />
        </label>
        <div className="form-grid thirds">
          <label>
            Restart policy
            <select
              value={restart}
              onChange={(event) => setRestart(event.target.value)}
            >
              <option value="no">No</option>
              <option value="always">Always</option>
              <option value="unless-stopped">Unless stopped</option>
              <option value="on-failure">On failure</option>
            </select>
          </label>
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
        <div className="modal-actions">
          <button
            type="button"
            className="button secondary"
            disabled={!!op.pending}
            onClick={close}
          >
            {completed ? 'Close' : 'Cancel'}
          </button>
          <button
            name="action"
            value="create"
            className="button secondary"
            disabled={!!op.pending || completed}
          >
            Create
          </button>
          <button
            name="action"
            value="run"
            className="button primary"
            disabled={!!op.pending || completed}
          >
            Create and run
          </button>
        </div>
      </form>
    </Modal>
  );
}
