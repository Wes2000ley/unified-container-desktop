import { useEffect, useRef, useState } from 'react';
import { call } from '../api';
import type {
  Event,
  EventBatch,
  Principal,
  Settings as SettingsData,
} from '../api';
import { Dashboard } from '../dashboard';
import { Containers, Images, Volumes } from '../inventories';
import { Projects } from '../projects';
import { Settings } from '../settings';
import { Networks } from '../networks';
import { Storage } from '../storage';
import { Registry } from '../registry';
import { useData } from '../use-data';
import { usePreferences } from '../preferences';

type Page =
  | 'dashboard'
  | 'projects'
  | 'containers'
  | 'images'
  | 'volumes'
  | 'networks'
  | 'storage'
  | 'registry'
  | 'settings';
const nav: { id: Page; label: string; icon: string }[] = [
  { id: 'dashboard', label: 'Dashboard', icon: '◫' },
  { id: 'projects', label: 'Projects', icon: '▧' },
  { id: 'containers', label: 'Containers', icon: '▣' },
  { id: 'images', label: 'Images', icon: '◈' },
  { id: 'volumes', label: 'Volumes', icon: '▤' },
  { id: 'networks', label: 'Networks', icon: '⬡' },
  { id: 'storage', label: 'Storage', icon: '▥' },
  { id: 'registry', label: 'Registries', icon: '⇅' },
  { id: 'settings', label: 'Settings', icon: '⚙' },
];
const loadIdentity = () =>
  call<{ settings: SettingsData; principal: Principal }>('system.settings');

export function App() {
  const [page, setPage] = useState<Page>('dashboard');
  const [events, setEvents] = useState<Event[]>([]);
  const [revision, setRevision] = useState(0);
  const [eventError, setEventError] = useState('');
  const sequence = useRef(0);
  const epoch = useRef('');
  const inFlight = useRef(false);
  const main = useRef<HTMLElement>(null);
  const identity = useData(loadIdentity);
  const preferences = usePreferences();
  const refreshIdentity = identity.refresh;
  const role = identity.data?.principal.role || '';
  const admin = role === 'administrator';
  const canOperate = admin || role === 'operator';
  useEffect(() => {
    let mounted = true;
    async function poll() {
      if (
        !mounted ||
        inFlight.current ||
        document.visibilityState !== 'visible' ||
        !document.hasFocus()
      )
        return;
      refreshIdentity();
      inFlight.current = true;
      try {
        const batch = await call<EventBatch>('system.events', '', {
          after: sequence.current,
          epoch: epoch.current,
        });
        if (!mounted) return;
        setEventError('');
        const restarted = batch.epoch !== epoch.current;
        if (restarted) {
          epoch.current = batch.epoch;
          sequence.current = 0;
        }
        if (restarted || batch.events.length) {
          if (batch.events.length)
            sequence.current = Math.max(...batch.events.map((e) => e.sequence));
          setEvents((current) =>
            (restarted ? batch.events : [...current, ...batch.events]).slice(
              -100,
            ),
          );
        }
        setRevision((value) => value + 1);
      } catch (error) {
        if (mounted)
          setEventError(error instanceof Error ? error.message : String(error));
      } finally {
        inFlight.current = false;
      }
    }
    const timer = window.setInterval(
      () => void poll(),
      preferences.refreshSeconds * 1000,
    );
    window.addEventListener('focus', poll);
    document.addEventListener('visibilitychange', poll);
    void poll();
    return () => {
      mounted = false;
      window.clearInterval(timer);
      window.removeEventListener('focus', poll);
      document.removeEventListener('visibilitychange', poll);
    };
  }, [refreshIdentity, preferences.refreshSeconds]);
  useEffect(() => {
    document.title = `${nav.find((item) => item.id === page)?.label || 'Workspace'} · Unified Container Desktop`;
  }, [page]);
  function navigate(next: Page) {
    setPage(next);
    window.requestAnimationFrame(() => main.current?.focus());
  }
  return (
    <div className="app-shell">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <aside className="sidebar">
        <div className="brand">
          <div className="brand-mark">
            <img src="/ucd-icon.svg" alt="" aria-hidden="true" />
          </div>
          <div>
            <strong>Unified</strong>
            <span>CONTAINER DESKTOP</span>
          </div>
        </div>
        <div className="sidebar-label">WORKSPACE</div>
        <nav aria-label="Main navigation">
          {nav.slice(0, 6).map((item) => (
            <button
              key={item.id}
              className={page === item.id ? 'nav-item active' : 'nav-item'}
              aria-current={page === item.id ? 'page' : undefined}
              onClick={() => navigate(item.id)}
            >
              <span className="nav-icon" aria-hidden="true">
                {item.icon}
              </span>
              {item.label}
            </button>
          ))}
          <div className="sidebar-label manage">SYSTEM</div>
          {nav.slice(6).map((item) => (
            <button
              key={item.id}
              className={page === item.id ? 'nav-item active' : 'nav-item'}
              aria-current={page === item.id ? 'page' : undefined}
              onClick={() => navigate(item.id)}
            >
              <span className="nav-icon" aria-hidden="true">
                {item.icon}
              </span>
              {item.label}
            </button>
          ))}
        </nav>
        <div className="sidebar-footer">
          <div className="avatar">
            {identity.data?.principal.name?.slice(0, 1).toUpperCase() || 'U'}
          </div>
          <div className="grow">
            <strong>{identity.data?.principal.name || 'Local session'}</strong>
            <span>
              {role || (identity.loading ? 'Connecting…' : 'Unavailable')}
            </span>
          </div>
          <span
            className={
              identity.data ? 'connection-dot' : 'connection-dot offline'
            }
          />
        </div>
      </aside>
      <div className="workspace">
        <header className="appbar">
          <div className="breadcrumbs">
            <span>WORKSPACE</span>
            <span className="slash">/</span>
            <strong>{nav.find((item) => item.id === page)?.label}</strong>
          </div>
          <div className="top-status">
            <span
              className={
                identity.data ? 'connection-dot' : 'connection-dot offline'
              }
            />
            {identity.data
              ? 'Controller connected'
              : identity.loading
                ? 'Connecting'
                : 'Controller unavailable'}
          </div>
        </header>
        <main id="main" ref={main} tabIndex={-1} className="content">
          {identity.error && (
            <div className="notice error" role="alert">
              <span>Controller connection: {identity.error}</span>
              <button className="text-button" onClick={identity.refresh}>
                Retry
              </button>
            </div>
          )}
          {eventError && (
            <div className="notice error" role="alert">
              Event updates unavailable: {eventError}
            </div>
          )}
          {role && !canOperate && (
            <div className="notice progress">
              Read-only access. Ask an administrator to add your account to
              Container Operators. Access updates automatically while this app
              is open. Administrative settings require running the app as an
              administrator.
            </div>
          )}
          {role === 'operator' && page === 'settings' && (
            <div className="notice progress">
              Administrative settings require running the app as an
              administrator.
            </div>
          )}
          {page === 'dashboard' && (
            <Dashboard revision={revision} admin={admin} />
          )}
          {page === 'projects' && (
            <Projects revision={revision} canOperate={canOperate} />
          )}
          {page === 'containers' && (
            <Containers revision={revision} canOperate={canOperate} />
          )}
          {page === 'images' && (
            <Images
              revision={revision}
              canOperate={canOperate}
              events={events}
            />
          )}
          {page === 'volumes' && (
            <Volumes revision={revision} canOperate={canOperate} />
          )}
          {page === 'networks' && (
            <Networks revision={revision} canOperate={canOperate} />
          )}
          {page === 'storage' && (
            <Storage revision={revision} canOperate={canOperate} />
          )}
          {page === 'registry' && <Registry canOperate={canOperate} />}
          {page === 'settings' && (
            <Settings revision={revision} admin={admin} events={events} />
          )}
        </main>
      </div>
    </div>
  );
}
