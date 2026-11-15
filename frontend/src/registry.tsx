import { Icon } from './icon';
import { useCallback, useState } from 'react';
import type { FormEvent } from 'react';
import { call } from './api';
import {
  Confirm,
  Empty,
  Feedback,
  LoadState,
  SectionTitle,
  useOperation,
} from './ui';
import { useData } from './use-data';
import './resources.css';

type Login = { server: string; username: string };
export function Registry({ canOperate }: { canOperate: boolean }) {
  const [server, setServer] = useState('');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [logout, setLogout] = useState<Login | null>(null);
  const op = useOperation();
  const load = useCallback(() => call<Login[]>('registry.list'), []);
  const logins = useData(load);
  async function login(event: FormEvent) {
    event.preventDefault();
    const ok = await op.run(
      'Signing in to registry',
      () =>
        call('registry.login', '', {
          server: server.trim(),
          username: username.trim(),
          password,
        }).then(() => undefined),
      'Registry sign-in saved.',
    );
    if (ok) {
      setPassword('');
      logins.refresh();
    }
  }
  async function signOut() {
    if (!logout) return;
    const ok = await op.run(
      'Signing out of registry',
      () =>
        call('registry.logout', '', { server: logout.server }).then(
          () => undefined,
        ),
      'Registry sign-in removed.',
    );
    if (ok) {
      setLogout(null);
      logins.refresh();
    }
  }
  return (
    <>
      <SectionTitle
        eyebrow="IMAGE ACCESS"
        title="Registries"
        description="One sign-in powers image pulls and pushes from both engines through the GUI and Docker CLI."
      >
        <button className="button secondary" onClick={logins.refresh}>
          <Icon name="refresh" size={16} /> Refresh
        </button>
      </SectionTitle>
      <Feedback error={op.error} success={op.success} pending={op.pending} />
      <div className="registry-layout">
        <section className="panel resource-section">
          <h2>Configured sign-ins</h2>
          <LoadState {...logins} retry={logins.refresh} />
          {!logins.loading && !logins.error && logins.data?.length === 0 ? (
            <Empty title="No registry sign-ins" />
          ) : (
            <div className="simple-list">
              {!logins.loading &&
                !logins.error &&
                logins.data?.map((entry) => (
                  <div key={entry.server}>
                    <span>
                      <strong>{entry.server || 'Docker Hub'}</strong>
                      <span className="subline">{entry.username}</span>
                    </span>
                    <button
                      className="table-action danger-text"
                      disabled={!canOperate || !!op.pending}
                      onClick={() => setLogout(entry)}
                    >
                      Sign out
                    </button>
                  </div>
                ))}
            </div>
          )}
        </section>
        <section className="panel resource-section">
          <div className="panel-heading">
            <div>
              <h2>Sign in</h2>
              <p>Leave server blank for Docker Hub.</p>
            </div>
          </div>
          <form className="form-stack" onSubmit={(event) => void login(event)}>
            <label>
              Registry server
              <input
                value={server}
                onChange={(event) => setServer(event.target.value)}
                placeholder="registry.example.com"
              />
            </label>
            <div className="form-grid">
              <label>
                Username
                <input
                  required
                  autoComplete="username"
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                />
              </label>
              <label>
                Password or token
                <input
                  required
                  type="password"
                  autoComplete="current-password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                />
              </label>
            </div>
            <div>
              <button
                className="button primary"
                disabled={!canOperate || !!op.pending}
              >
                Sign in
              </button>
            </div>
          </form>
        </section>
      </div>
      {logout && (
        <Confirm
          title={`Sign out of ${logout.server || 'Docker Hub'}?`}
          detail={`Remove the shared registry credential for ${logout.username} from both engines and the Docker CLI.`}
          action="Sign out"
          pending={!!op.pending}
          error={op.error}
          close={() => setLogout(null)}
          confirm={() => void signOut()}
        />
      )}
    </>
  );
}
