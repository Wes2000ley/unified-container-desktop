# Unified Container Desktop frontend

React and TypeScript interface for the Wails 3 desktop application. The production interface calls the `main.Bridge.Call` service and requires the local controller; it does not simulate engine data.

Use Node.js 24.15 or newer and run from this directory:

```powershell
npm ci
npm run check
npm run build
npm run test:e2e
npm run test:web
```

The Go desktop build embeds `dist` through `assets.go`; run the frontend build before compiling the desktop application. Browser tests deliberately alias the runtime to `tests/fixtures/fake-runtime.ts` only when `UCD_TEST_BRIDGE=1` and write their assets to `.test-dist`, separate from production `dist`. They do not verify the native service, engines, or Wails IPC.

The same interface is also served by `ucd web` at its printed loopback launch URL. The URL fragment contains a session token; the interface removes it from the address bar and retains it in that tab's session storage for refreshes. Keep the launch URL private. If the session token is missing, restart `ucd web` and open its new launch URL. Browser calls use the same local controller operations as the desktop interface.

`npm run test:web` launches the built `../bin/ucd.exe` and exercises the real production web adapter. It does not use the fixture runtime. Build the release binaries first, or set `UCD_CLI` to the CLI executable to test. A missing machine service is reported honestly; this test does not replace installed dual-engine acceptance.
