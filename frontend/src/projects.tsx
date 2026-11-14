import { useCallback, useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { call, operationErrors } from './api';
import type {
  Engine,
  Event,
  EventBatch,
  OperationResult,
  Project,
  ProjectFile,
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

const loadProjects = () => call<Project[]>('project.list');
const actions = [
  'up',
  'start',
  'stop',
  'restart',
  'down',
  'remove',
  'build',
  'pull',
  'logs',
] as const;
type Action = (typeof actions)[number];
const actionLabels: Record<Action, string> = {
  up: 'Deploy',
  start: 'Start',
  stop: 'Stop',
  restart: 'Restart',
  down: 'Down',
  remove: 'Remove',
  build: 'Build',
  pull: 'Pull',
  logs: 'Logs',
};

export function Projects({
  revision,
  canOperate,
}: {
  revision: number;
  canOperate: boolean;
}) {
  const list = useData(loadProjects, revision);
  const op = useOperation();
  const [form, setForm] = useState<Project | 'new' | null>(null);
  const [editing, setEditing] = useState<Project | null>(null);
  const [target, setTarget] = useState<Record<string, string>>({});
  const [services, setServices] = useState<Record<string, string>>({});
  const [confirm, setConfirm] = useState<{
    project: Project;
    action: Action;
  } | null>(null);
  const [removeVolumes, setRemoveVolumes] = useState(false);
  const [output, setOutput] = useState<{
    title: string;
    value: unknown;
  } | null>(null);
  const [logs, setLogs] = useState<{
    project: Project;
    engine: string;
    services: string[];
  } | null>(null);
  const [progress, setProgress] = useState<{
    project: Project;
    engine: string;
    action: Action;
    epoch: string;
    sequence: number;
    active: boolean;
    error: string;
  } | null>(null);
  async function perform(project: Project, action: Action, remove = false) {
    const engine = target[project.id] || '';
    const selectedServices = (services[project.id] || '')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean);
    const stream = action === 'up' || action === 'build' || action === 'pull';
    const ok = await op.run(
      `${actionLabels[action]}: ${project.name}`,
      async () => {
        if (stream) {
          const batch = await call<EventBatch>('system.events', '', {
            after: 0,
            epoch: '',
          });
          setProgress({
            project,
            engine,
            action,
            epoch: batch.epoch,
            sequence: Math.max(
              0,
              ...batch.events.map((event) => event.sequence),
            ),
            active: true,
            error: '',
          });
        }
        try {
          const result = await call<OperationResult & { success?: boolean }>(
            'project.action',
            '',
            {
              id: project.id,
              action,
              engine,
              removeVolumes: remove,
              services: selectedServices,
            },
          );
          const failures = operationErrors(result);
          if ((failures.length || result.success === false) && !stream) {
            setConfirm(null);
            setOutput({
              title: `${project.name} · ${actionLabels[action]}`,
              value: result,
            });
          }
          if (failures.length || result.success === false)
            throw new Error(
              failures.join('\n') ||
                'The project operation did not complete successfully.',
            );
        } catch (reason) {
          if (stream)
            setProgress((current) =>
              current
                ? {
                    ...current,
                    error:
                      reason instanceof Error ? reason.message : String(reason),
                  }
                : current,
            );
          throw reason;
        } finally {
          if (stream)
            setProgress((current) =>
              current ? { ...current, active: false } : current,
            );
        }
      },
      action === 'logs'
        ? undefined
        : `${actionLabels[action]} completed for ${project.name}.`,
    );
    if (ok) {
      setConfirm(null);
      setRemoveVolumes(false);
      list.refresh();
    }
  }
  return (
    <>
      <SectionTitle
        eyebrow="COMPOSE WORKSPACE"
        title="Projects"
        description="Manage Windows and Linux Compose files as one application."
      >
        <button className="button secondary" onClick={list.refresh}>
          ↻ &nbsp;Refresh
        </button>
        <button
          className="button primary"
          disabled={!canOperate}
          onClick={() => setForm('new')}
        >
          ＋ &nbsp;New project
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <LoadState {...list} retry={list.refresh} />
      {list.data?.length === 0 && (
        <div className="panel">
          <Empty title="No projects yet">
            Create a project by pasting or selecting a Compose YAML file.
          </Empty>
        </div>
      )}
      <div className="project-list">
        {list.data?.map((project) => (
          <article className="panel project-card" key={project.id}>
            <div className="project-top">
              <div className="project-mark">▧</div>
              <div className="grow">
                <h2>{project.name}</h2>
                <div className="muted small">
                  {project.id}
                  {project.discovered ? ' · Discovered external project' : ''}
                </div>
              </div>
              <div className="part-badges">
                {project.parts?.map((part) => (
                  <Badge key={part.engine} tone="good">
                    {part.engine}
                  </Badge>
                ))}
              </div>
            </div>
            <div className="project-parts">
              {project.parts?.map((part) => (
                <div key={part.engine}>
                  <span className="mini-label">
                    {part.engine.toUpperCase()} COMPOSE
                  </span>
                  <span title={part.composeFile}>{part.composeFile}</span>
                </div>
              ))}
            </div>
            {project.discovered ? (
              <div className="project-actions">
                <p className="muted small">
                  Import its Compose file to create a managed project. Existing
                  containers remain separate.
                </p>
                <button
                  className="button secondary"
                  disabled={!canOperate}
                  onClick={() => setForm(project)}
                >
                  Import Compose
                </button>
              </div>
            ) : (
              <div className="project-actions">
                <label className="target-select">
                  Target{' '}
                  <select
                    aria-label={`Target engine for ${project.name}`}
                    value={target[project.id] || ''}
                    onChange={(e) =>
                      setTarget({ ...target, [project.id]: e.target.value })
                    }
                  >
                    <option value="">All engines</option>
                    {project.parts?.map((p) => (
                      <option key={p.engine} value={p.engine}>
                        {p.engine}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="target-select">
                  Services{' '}
                  <input
                    aria-label={`Services for ${project.name}`}
                    placeholder="All (or names, comma separated)"
                    value={services[project.id] || ''}
                    onChange={(e) =>
                      setServices({ ...services, [project.id]: e.target.value })
                    }
                  />
                </label>
                <div className="action-wrap">
                  {actions.map((action) => (
                    <button
                      key={action}
                      className={`button ${action === 'up' ? 'primary' : 'quiet'}`}
                      disabled={
                        (action !== 'logs' && !canOperate) ||
                        !!op.pending ||
                        !!progress ||
                        ((action === 'down' || action === 'remove') &&
                          !!services[project.id]?.trim())
                      }
                      onClick={() =>
                        action === 'logs'
                          ? setLogs({
                              project,
                              engine: target[project.id] || '',
                              services: (services[project.id] || '')
                                .split(',')
                                .map((s) => s.trim())
                                .filter(Boolean),
                            })
                          : action === 'down' || action === 'remove'
                            ? (setRemoveVolumes(false),
                              setConfirm({ project, action }))
                            : void perform(project, action)
                      }
                    >
                      {actionLabels[action]}
                    </button>
                  ))}
                  <button
                    className="button quiet"
                    disabled={!canOperate}
                    onClick={() => setEditing(project)}
                  >
                    Edit YAML
                  </button>
                  {project.parts?.length < 2 && (
                    <button
                      className="button quiet"
                      disabled={!canOperate}
                      onClick={() => setForm(project)}
                    >
                      Add OS file
                    </button>
                  )}
                </div>
                {!!services[project.id]?.trim() && (
                  <p className="muted small">
                    Down and Remove apply to the whole project. Use Stop for
                    selected services.
                  </p>
                )}
              </div>
            )}
          </article>
        ))}
      </div>
      {form && (
        <ProjectForm
          project={form === 'new' ? undefined : form}
          close={() => setForm(null)}
          saved={() => {
            setForm(null);
            list.refresh();
          }}
        />
      )}
      {editing && (
        <ProjectEditor
          project={editing}
          close={() => setEditing(null)}
          saved={list.refresh}
        />
      )}
      {confirm && (
        <Confirm
          title={`${actionLabels[confirm.action]} ${confirm.project.name}?`}
          action={actionLabels[confirm.action]}
          pending={!!op.pending}
          error={op.error}
          close={() => setConfirm(null)}
          confirm={() =>
            void perform(confirm.project, confirm.action, removeVolumes)
          }
          detail={
            <>
              <span>
                This applies to{' '}
                {target[confirm.project.id] || 'all project engines'}.
              </span>
              <label className="check-row">
                <input
                  type="checkbox"
                  checked={removeVolumes}
                  onChange={(e) => setRemoveVolumes(e.target.checked)}
                />{' '}
                Also remove project volumes
              </label>
              <span>Volumes are retained unless you select this option.</span>
            </>
          }
        />
      )}
      {output && (
        <OutputModal
          title={output.title}
          value={output.value}
          close={() => setOutput(null)}
        />
      )}
      {logs && (
        <ProjectLogs
          project={logs.project}
          engine={logs.engine}
          services={logs.services}
          close={() => setLogs(null)}
        />
      )}
      {progress && (
        <ProjectProgress state={progress} close={() => setProgress(null)} />
      )}
    </>
  );
}

function ProjectProgress({
  state,
  close,
}: {
  state: {
    project: Project;
    engine: string;
    action: Action;
    epoch: string;
    sequence: number;
    active: boolean;
    error: string;
  };
  close: () => void;
}) {
  const cursor = useRef({ epoch: state.epoch, sequence: state.sequence });
  const [text, setText] = useState('');
  const [error, setError] = useState('');
  useEffect(() => {
    let mounted = true;
    let timer = 0;
    async function poll() {
      try {
        const batch = await call<EventBatch>('system.events', '', {
          after: cursor.current.sequence,
          epoch: cursor.current.epoch,
        });
        if (!mounted) return;
        if (cursor.current.epoch !== batch.epoch) cursor.current.sequence = 0;
        cursor.current.epoch = batch.epoch;
        if (batch.events.length)
          cursor.current.sequence = Math.max(
            ...batch.events.map((event) => event.sequence),
          );
        const matches = batch.events.filter(
          (event: Event) =>
            (event.type === 'compose.output' ||
              event.type === 'compose.progress') &&
            state.project.parts.some(
              (part) =>
                part.engine === event.engine &&
                part.name === event.project &&
                (!state.engine || part.engine === state.engine),
            ),
        );
        if (matches.length)
          setText((current) =>
            (
              current +
              matches
                .map(
                  (event) =>
                    `[${event.engine}] ${event.message}${event.type === 'compose.output' ? '' : '\n'}`,
                )
                .join('')
            ).slice(-8 * 1024 * 1024),
          );
        setError('');
      } catch (reason) {
        if (mounted)
          setError(reason instanceof Error ? reason.message : String(reason));
      }
      if (mounted && state.active)
        timer = window.setTimeout(() => void poll(), 500);
    }
    void poll();
    return () => {
      mounted = false;
      window.clearTimeout(timer);
    };
  }, [state.active, state.engine, state.project]);
  return (
    <Modal
      title={`${state.project.name} · ${actionLabels[state.action]}`}
      close={close}
    >
      <p className="muted small">
        {state.active
          ? 'Operation in progress. Output updates automatically.'
          : 'Operation finished.'}{' '}
        Recent progress is retained temporarily; earlier output may expire.
      </p>
      {(state.error || error) && (
        <div className="inline-error">
          {state.error || `Progress updates unavailable: ${error}`}
        </div>
      )}
      <pre className="output" aria-label="Operation output">
        {text || 'Waiting for Compose output…'}
      </pre>
      <div className="modal-actions">
        <button className="button secondary" onClick={close}>
          Close
        </button>
      </div>
    </Modal>
  );
}

function ProjectLogs({
  project,
  engine,
  services,
  close,
}: {
  project: Project;
  engine: string;
  services: string[];
  close: () => void;
}) {
  const [following, setFollowing] = useState(true);
  const follow = useRef(true);
  const [search, setSearch] = useState('');
  const [texts, setTexts] = useState<
    Record<string, { text: string; truncated: boolean }>
  >({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [finished, setFinished] = useState<Record<string, boolean>>({});
  const selected = project.parts.filter(
    (part) => !engine || part.engine === engine,
  );
  useEffect(() => {
    let mounted = true;
    const abort = new AbortController();
    const sessions = new Map<string, string>();
    async function read(partEngine: Engine) {
      let id = '';
      try {
        const opened = await call<OperationResult>('project.logs.open', '', {
          id: project.id,
          engine: partEngine,
          services,
        });
        const part = opened.parts.find((item) => item.engine === partEngine);
        if (part?.error) throw new Error(part.error);
        id =
          (part?.result as { sessionID?: string } | undefined)?.sessionID || '';
        if (!id) throw new Error('The engine did not return a log session.');
        if (!mounted) {
          await call('project.logs.close', partEngine, { sessionID: id });
          return;
        }
        sessions.set(partEngine, id);
        const decoder = new TextDecoder();
        while (mounted) {
          if (!follow.current) {
            await new Promise((resolve) => window.setTimeout(resolve, 250));
            continue;
          }
          const result = await call<{ data: string; eof: boolean }>(
            'project.logs.read',
            partEngine,
            { sessionID: id, maxBytes: 65536 },
            { signal: abort.signal },
          );
          if (!mounted) break;
          const bytes = Uint8Array.from(atob(result.data || ''), (ch) =>
            ch.charCodeAt(0),
          );
          const chunk = decoder.decode(bytes, { stream: !result.eof });
          if (chunk)
            setTexts((current) => {
              const combined = (current[partEngine]?.text || '') + chunk;
              return {
                ...current,
                [partEngine]: {
                  text: combined.slice(-8 * 1024 * 1024),
                  truncated:
                    !!current[partEngine]?.truncated ||
                    combined.length > 8 * 1024 * 1024,
                },
              };
            });
          if (result.eof) {
            setFinished((current) => ({ ...current, [partEngine]: true }));
            break;
          }
          if (!chunk)
            await new Promise((resolve) => window.setTimeout(resolve, 250));
        }
      } catch (reason) {
        if (mounted)
          setErrors((current) => ({
            ...current,
            [partEngine]:
              reason instanceof Error ? reason.message : String(reason),
          }));
      } finally {
        if (id && sessions.get(partEngine) === id) {
          sessions.delete(partEngine);
          try {
            await call('project.logs.close', partEngine, { sessionID: id });
          } catch {
            /* Already closed. */
          }
        }
      }
    }
    for (const part of selected) void read(part.engine);
    return () => {
      mounted = false;
      abort.abort();
      for (const [partEngine, id] of sessions)
        void call('project.logs.close', partEngine, { sessionID: id }).catch(
          () => undefined,
        );
      sessions.clear();
    };
    // The session follows the selection made when the dialog was opened.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  function exportLogs() {
    const content = selected
      .map(
        (part) => `--- ${part.engine} ---\n${texts[part.engine]?.text || ''}`,
      )
      .join('\n');
    const url = URL.createObjectURL(
      new Blob([content], { type: 'text/plain;charset=utf-8' }),
    );
    const link = document.createElement('a');
    link.href = url;
    link.download = `${project.id}-logs.txt`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 0);
  }
  return (
    <Modal title={`${project.name} · Logs`} close={close}>
      <div className="form-stack">
        <div className="filters">
          <input
            aria-label="Search project logs"
            placeholder="Search logs…"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
          <label className="check-row">
            <input
              type="checkbox"
              checked={following}
              onChange={(event) => {
                follow.current = event.target.checked;
                setFollowing(event.target.checked);
              }}
            />{' '}
            Follow
          </label>
          <button className="button secondary" onClick={exportLogs}>
            Export logs
          </button>
        </div>
        <p className="muted small">
          Starts with the most recent 500 lines per container.{' '}
          {following
            ? 'Following new output.'
            : 'Paused; idle sessions expire after 10 minutes.'}{' '}
          {Object.values(texts).some((value) => value.truncated) &&
            'Only the latest 8 million characters per engine are retained in this view.'}
        </p>
        {selected.map((part) => {
          const text = texts[part.engine]?.text || '';
          const visible = search
            ? text
                .split('\n')
                .filter((line) =>
                  line.toLowerCase().includes(search.toLowerCase()),
                )
                .join('\n')
            : text;
          return (
            <div key={part.engine}>
              <strong>{part.engine}</strong>
              {finished[part.engine] && (
                <span className="muted small"> · Stream ended</span>
              )}
              {errors[part.engine] && (
                <div className="inline-error">{errors[part.engine]}</div>
              )}
              <pre
                className="output"
                aria-label={`${part.engine} project logs`}
              >
                {visible ||
                  (text
                    ? 'No matching log lines.'
                    : finished[part.engine]
                      ? 'No logs.'
                      : 'Waiting for logs…')}
              </pre>
            </div>
          );
        })}
        <div className="modal-actions">
          <button className="button secondary" onClick={close}>
            Close
          </button>
        </div>
      </div>
    </Modal>
  );
}

function ProjectForm({
  project,
  close,
  saved,
}: {
  project?: Project;
  close: () => void;
  saved: () => void;
}) {
  const attaching = !!project && !project.discovered;
  const [name, setName] = useState(project?.name || '');
  const [engine, setEngine] = useState<Engine>(
    project?.discovered
      ? project.parts[0]?.engine || 'windows'
      : project?.parts?.some((p) => p.engine === 'windows')
        ? 'linux'
        : 'windows',
  );
  const [yaml, setYAML] = useState('');
  const [sourceFile, setSourceFile] = useState<File>();
  const [contextFiles, setContextFiles] = useState<File[]>([]);
  const [replace, setReplace] = useState(false);
  const [first, setFirst] = useState<Engine>('windows');
  const op = useOperation();
  async function submit(event: FormEvent) {
    event.preventDefault();
    const startupOrder = attaching
      ? [first, first === 'windows' ? 'linux' : 'windows']
      : undefined;
    const ok = await op.run('Saving project', async () => {
      const files: { path: string; content: string }[] = [];
      for (const file of contextFiles) {
        const relative = file.webkitRelativePath
          ? file.webkitRelativePath.split('/').slice(1).join('/')
          : file.name;
        if (relative.toLowerCase() === '.ucd-import.json')
          throw new Error(
            'The context directory contains the reserved project-root file .ucd-import.json. Remove or rename it before importing.',
          );
        if (relative.toLowerCase() === 'compose.yaml') {
          if (
            sourceFile?.name.toLowerCase() === 'compose.yaml' &&
            (await sourceFile.text()) === (await file.text())
          )
            continue;
          throw new Error(
            'The context directory contains a different compose.yaml. Select that file as the Compose source, or remove it from the context.',
          );
        }
        const content = await new Promise<string>((resolve, reject) => {
          const reader = new FileReader();
          reader.onerror = () =>
            reject(new Error(`Could not read ${relative}.`));
          reader.onload = () =>
            resolve(String(reader.result).split(',')[1] || '');
          reader.readAsDataURL(file);
        });
        files.push({ path: relative, content });
      }
      await call<Project>('project.import', '', {
        id: attaching ? project?.id : undefined,
        name: name.trim(),
        engine,
        yaml,
        files,
        startupOrder,
        replace,
      });
    });
    if (ok) saved();
  }
  return (
    <Modal
      title={
        attaching
          ? `Add OS file · ${project?.name}`
          : project?.discovered
            ? `Import Compose · ${project.name}`
            : 'Create project'
      }
      close={close}
    >
      <form onSubmit={(event) => void submit(event)} className="form-stack">
        <Feedback error={op.error} pending={op.pending} />
        {project?.discovered && (
          <p className="callout">
            This imports a managed copy of the supplied Compose file. Existing
            external containers remain separate.
          </p>
        )}
        <div className="form-grid">
          <label>
            Project name
            <input
              required
              maxLength={128}
              value={name}
              disabled={attaching}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-application"
            />
          </label>
          <label>
            Engine
            <select
              value={engine}
              disabled={attaching}
              onChange={(e) => setEngine(e.target.value as Engine)}
            >
              <option value="windows">Windows</option>
              <option value="linux">Linux</option>
            </select>
          </label>
        </div>
        {attaching && (
          <label>
            Startup order
            <select
              value={first}
              onChange={(e) => setFirst(e.target.value as Engine)}
            >
              <option value="windows">Windows first</option>
              <option value="linux">Linux first</option>
            </select>
          </label>
        )}
        <label>
          Compose YAML
          <textarea
            className="code-input"
            required
            value={yaml}
            onChange={(e) => setYAML(e.target.value)}
            placeholder={'services:\n  app:\n    image: example/image'}
            spellCheck={false}
          />
        </label>
        <label className="file-label">
          Or select a YAML file
          <input
            type="file"
            accept=".yaml,.yml,text/yaml"
            onChange={(e) => {
              const file = e.target.files?.[0];
              setSourceFile(file);
              if (file)
                void file
                  .text()
                  .then(setYAML)
                  .catch((reason) =>
                    alert(`Could not read file: ${String(reason)}`),
                  );
            }}
          />
        </label>
        <label className="file-label">
          Build context directory (optional)
          <input
            type="file"
            multiple
            {...{ webkitdirectory: '' }}
            onChange={(e) => setContextFiles(Array.from(e.target.files || []))}
          />
        </label>
        {contextFiles.length > 0 && (
          <p className="muted small">
            {contextFiles.length} context file(s) selected. The selected
            compose.yaml is stored as the project's Compose file.
          </p>
        )}
        <label className="check-row">
          <input
            type="checkbox"
            checked={replace}
            onChange={(e) => setReplace(e.target.checked)}
          />{' '}
          Replace previously imported source files
        </label>
        {replace && (
          <p className="muted small">
            Tracked source files may be replaced or removed. Existing containers
            and volumes are retained.
          </p>
        )}
        <p className="muted small">
          A second OS Compose file can be attached to the same project after
          creation.
        </p>
        <div className="modal-actions">
          <button
            type="button"
            className="button secondary"
            onClick={close}
            disabled={!!op.pending}
          >
            Cancel
          </button>
          <button className="button primary" disabled={!!op.pending}>
            {op.pending
              ? 'Saving…'
              : attaching
                ? 'Attach file'
                : project?.discovered
                  ? 'Import Compose'
                  : 'Create project'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function ProjectEditor({
  project,
  close,
  saved,
}: {
  project: Project;
  close: () => void;
  saved: () => void;
}) {
  const load = useCallback(async () => {
    const results = await Promise.allSettled(
      project.parts.map((part) =>
        call<ProjectFile[]>('project.read', '', {
          id: project.id,
          engine: part.engine,
        }),
      ),
    );
    const files: ProjectFile[] = [];
    const errors: string[] = [];
    results.forEach((result, index) => {
      if (result.status === 'fulfilled') files.push(...result.value);
      else
        errors.push(
          `${project.parts[index]?.engine || 'Engine'}: ${result.reason instanceof Error ? result.reason.message : String(result.reason)}`,
        );
    });
    return { files, errors };
  }, [project.id, project.parts]);
  const data = useData(load);
  return (
    <Modal title={`Edit Compose · ${project.name}`} close={close}>
      <LoadState {...data} retry={data.refresh} />
      {data.data?.errors.map((error) => (
        <div className="inline-error" key={error}>
          {error}
        </div>
      ))}
      {!!data.data?.errors.length && (
        <button
          className="button secondary"
          disabled={data.loading}
          onClick={data.refresh}
        >
          Retry unavailable files
        </button>
      )}
      {data.data && (
        <ProjectFiles
          project={project}
          initial={data.data.files}
          saved={saved}
        />
      )}
    </Modal>
  );
}
function ProjectFiles({
  project,
  initial,
  saved,
}: {
  project: Project;
  initial: ProjectFile[];
  saved: () => void;
}) {
  const [edits, setEdits] = useState<Partial<Record<Engine, string>>>({});
  const [selected, setSelected] = useState<Engine | undefined>(
    initial[0]?.engine,
  );
  const op = useOperation();
  const current =
    initial.find((file) => file.engine === selected) || initial[0];
  const yaml = current ? (edits[current.engine] ?? current.yaml) : '';
  async function save() {
    if (!current) return;
    const ok = await op.run(
      `Saving ${current.engine} Compose`,
      () =>
        call('project.write', '', {
          id: project.id,
          engine: current.engine,
          yaml,
        }).then(() => undefined),
      `${current.engine} Compose saved.`,
    );
    if (ok) saved();
  }
  return (
    <div className="form-stack">
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <div className="segmented" role="group" aria-label="Compose file">
        {initial.map((file) => (
          <button
            key={file.engine}
            className={file.engine === current?.engine ? 'selected' : ''}
            onClick={() => setSelected(file.engine)}
          >
            {file.engine}
          </button>
        ))}
      </div>
      {current ? (
        <>
          <label>
            YAML for {current.engine}
            <textarea
              className="code-input editor"
              value={yaml}
              onChange={(e) =>
                setEdits({ ...edits, [current.engine]: e.target.value })
              }
              spellCheck={false}
            />
          </label>
          <div className="modal-actions">
            <button
              className="button primary"
              disabled={!!op.pending}
              onClick={() => void save()}
            >
              Save {current.engine} file
            </button>
          </div>
        </>
      ) : (
        <Empty title="No Compose files reported" />
      )}
    </div>
  );
}
