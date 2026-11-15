# Shared desktop and web interface

UCD uses one React/TypeScript interface for its Wails 3 desktop application and
the loopback browser adapter. Both call the same authenticated machine controller;
production assets do not simulate engines or container data.

## Development

Use Node.js 24.15 or newer. From this directory:

```powershell
npm ci
npm run check
npm run build
npm run test:e2e
```

The Go applications embed `dist` through `assets.go`. Build the frontend before
compiling them, or use the root [build script](../scripts/build.ps1).

`npm run check` runs TypeScript, lint and component tests. Playwright fixture tests
alias the runtime to `tests/fixtures/fake-runtime.ts` only when `UCD_TEST_BRIDGE=1`.
Those assets go to `.test-dist`, separate from production `dist`; fixture tests do
not qualify native IPC, the controller or either real engine.

## Installed application checks

```powershell
npm run test:web
```

This launches the built `../bin/ucd.exe web` adapter against production assets and
checks its actual service response. Build the binaries first, or set `UCD_CLI` to
the executable being tested. It reports a missing machine service and does not
replace installed dual-engine acceptance. See [VALIDATION.md](../VALIDATION.md)
for recorded qualification results.

## Browser sessions

Run `ucd web` and open the loopback URL it prints. The adapter uses the invoking
Windows user's permissions. The URL fragment contains a session token; the
interface removes it from the address bar and retains it in that tab's session
storage for refreshes. Keep the launch URL private. If the token is missing,
restart `ucd web` and use its new URL. Stop the adapter with Ctrl+C.

See the [operating guide](../docs/OPERATIONS.md) for user workflows and permissions.
