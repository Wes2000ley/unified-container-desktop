import type { ReactNode } from 'react';

const paths = {
  dashboard: (
    <>
      <rect x="3" y="3" width="7" height="7" rx="1.5" />
      <rect x="14" y="3" width="7" height="7" rx="1.5" />
      <rect x="3" y="14" width="7" height="7" rx="1.5" />
      <rect x="14" y="14" width="7" height="7" rx="1.5" />
    </>
  ),
  projects: (
    <>
      <rect x="3" y="3" width="7" height="7" rx="1.5" />
      <rect x="14" y="14" width="7" height="7" rx="1.5" />
      <path d="M14 6h4v4M6 14v4h4M10 6h4M6 10v4M18 10v4M10 18h4" />
    </>
  ),
  containers: (
    <>
      <path d="m12 3 9 5v8l-9 5-9-5V8l9-5Z" />
      <path d="m3 8 9 5 9-5M12 13v8M7.5 5.5l9 5" />
    </>
  ),
  images: (
    <>
      <path d="m12 3 9 5-9 5-9-5 9-5ZM3 12l9 5 9-5M3 16l9 5 9-5" />
    </>
  ),
  volumes: (
    <>
      <ellipse cx="12" cy="5" rx="9" ry="3" />
      <path d="M3 5v14c0 1.7 4 3 9 3s9-1.3 9-3V5M3 12c0 1.7 4 3 9 3s9-1.3 9-3" />
    </>
  ),
  networks: (
    <>
      <rect x="8" y="2" width="8" height="6" rx="1" />
      <rect x="2" y="16" width="7" height="6" rx="1" />
      <rect x="15" y="16" width="7" height="6" rx="1" />
      <path d="M12 8v4M5.5 16v-4h13v4" />
    </>
  ),
  storage: (
    <>
      <rect x="3" y="4" width="18" height="7" rx="2" />
      <rect x="3" y="14" width="18" height="7" rx="2" />
      <path d="M7 7.5h.01M7 17.5h.01M15 7.5h3M15 17.5h3" />
    </>
  ),
  registry: (
    <>
      <path d="M7 3v15m-4-4 4 4 4-4M17 21V6m-4 4 4-4 4 4" />
    </>
  ),
  settings: (
    <>
      <path d="m9 3-.5 3-2 1L4 6 2 9l2.5 2v2L2 15l2 3 2.5-1 2 1 .5 3h6l.5-3 2-1 2.5 1 2-3-2.5-2v-2L22 9l-2-3-2.5 1-2-1L15 3H9Z" />
      <circle cx="12" cy="12" r="3" />
    </>
  ),
  refresh: (
    <>
      <path d="M20 7v5h-5M4 17v-5h5" />
      <path d="M6 6a8 8 0 0 1 13 2l1 4M4 12l1 4a8 8 0 0 0 13 2" />
    </>
  ),
  cpu: (
    <>
      <rect x="6" y="6" width="12" height="12" rx="2" />
      <rect x="9" y="9" width="6" height="6" rx="1" />
      <path d="M9 2v4m6-4v4M9 18v4m6-4v4M2 9h4m-4 6h4M18 9h4m-4 6h4" />
    </>
  ),
  memory: (
    <>
      <rect x="3" y="6" width="18" height="12" rx="2" />
      <path d="M7 10h3v4H7zM14 10h3v4h-3zM6 18v3m4-3v3m4-3v3m4-3v3" />
    </>
  ),
  windows: (
    <>
      <path d="M3 5 11 4v7H3V5ZM14 3.5 22 2v9h-8V3.5ZM3 14h8v7l-8-1v-6ZM14 14h8v9l-8-1.5V14Z" />
    </>
  ),
  linux: (
    <>
      <path d="m7 3 4 4-4 4M14 12h5" />
      <rect x="2" y="1" width="20" height="21" rx="3" />
    </>
  ),
  arrow: <path d="M4 12h16m-6-6 6 6-6 6" />,
  plus: <path d="M12 5v14M5 12h14" />,
} satisfies Record<string, ReactNode>;

export type IconName = keyof typeof paths;

export function Icon({ name, size = 18 }: { name: IconName; size?: number }) {
  return (
    <svg
      className="icon"
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths[name]}
    </svg>
  );
}
