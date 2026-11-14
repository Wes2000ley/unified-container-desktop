import { useSyncExternalStore } from 'react';

export type UserPreferences = {
  refreshSeconds: 5 | 10 | 15 | 30;
  logTail: 100 | 300 | 1000 | 5000;
  logTimestamps: boolean;
};

const key = 'ucd.ui-preferences.v1';
const defaults: UserPreferences = {
  refreshSeconds: 5,
  logTail: 300,
  logTimestamps: false,
};
let current: UserPreferences | undefined;
const listeners = new Set<() => void>();

function readStored(): UserPreferences {
  try {
    const saved = localStorage.getItem(key);
    return saved ? valid(JSON.parse(saved)) : defaults;
  } catch {
    return defaults;
  }
}

function valid(value: unknown): UserPreferences {
  if (!value || typeof value !== 'object') return defaults;
  const candidate = value as Record<string, unknown>;
  const refreshSeconds = [5, 10, 15, 30].includes(
    Number(candidate.refreshSeconds),
  )
    ? (Number(candidate.refreshSeconds) as UserPreferences['refreshSeconds'])
    : defaults.refreshSeconds;
  const logTail = [100, 300, 1000, 5000].includes(Number(candidate.logTail))
    ? (Number(candidate.logTail) as UserPreferences['logTail'])
    : defaults.logTail;
  return {
    refreshSeconds,
    logTail,
    logTimestamps:
      typeof candidate.logTimestamps === 'boolean'
        ? candidate.logTimestamps
        : defaults.logTimestamps,
  };
}

function snapshot(): UserPreferences {
  if (!current) current = readStored();
  return current;
}

function changed() {
  for (const listener of listeners) listener();
}

function onStorage(event: StorageEvent) {
  if (event.key !== key) return;
  try {
    current = event.newValue ? valid(JSON.parse(event.newValue)) : defaults;
  } catch {
    current = defaults;
  }
  changed();
}

function subscribe(listener: () => void) {
  if (listeners.size === 0) {
    const stored = readStored();
    if (
      !current ||
      current.refreshSeconds !== stored.refreshSeconds ||
      current.logTail !== stored.logTail ||
      current.logTimestamps !== stored.logTimestamps
    )
      current = stored;
  }
  listeners.add(listener);
  if (listeners.size === 1) window.addEventListener('storage', onStorage);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) window.removeEventListener('storage', onStorage);
  };
}

export function usePreferences(): UserPreferences {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

export function savePreferences(value: UserPreferences): boolean {
  current = valid(value);
  let saved = true;
  try {
    localStorage.setItem(key, JSON.stringify(current));
  } catch {
    saved = false;
  }
  changed();
  return saved;
}
