import { isWebTransport } from './api';

export type DesktopPreferences = {
  startAtLogin: boolean;
  startMinimized: boolean;
  closeToTray: boolean;
};

const defaults: DesktopPreferences = {
  startAtLogin: false,
  startMinimized: false,
  closeToTray: true,
};

export function isDesktopClient(): boolean {
  const declaredDesktop = Array.from(
    document.querySelectorAll('meta[name="ucd-transport"]'),
  ).some((meta) => meta.getAttribute('content') === 'desktop');
  return !import.meta.env.DEV && declaredDesktop && !isWebTransport();
}

export async function getDesktopPreferences(): Promise<DesktopPreferences> {
  const { Call } = await import('@wailsio/runtime');
  const result = await Call.ByName('main.Bridge.GetDesktopPreferences');
  if (!result || typeof result !== 'object')
    throw new Error('The desktop app returned invalid startup preferences.');
  return { ...defaults, ...(result as Partial<DesktopPreferences>) };
}

export async function setDesktopPreferences(
  preferences: DesktopPreferences,
): Promise<void> {
  const { Call } = await import('@wailsio/runtime');
  await Call.ByName('main.Bridge.SetDesktopPreferences', preferences);
}
