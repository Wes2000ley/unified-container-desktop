# Contributing

UCD integrates the standard Docker engines and Compose SDK. Keep changes small,
preserve ordinary Docker/Compose behavior, and report engine errors directly.
Kubernetes, Swarm management, cloud accounts and telemetry are outside the product.

Open an issue describing the problem and expected behavior before a large change.
For bugs, include the UCD version, Windows edition/build, engine, exact command or
GUI steps, expected result and relevant redacted logs. Never attach registry
passwords, tokens, browser launch URLs, service-account secrets or private project
files. Explain which settings or resources the reproduction changes.

## Development

See [README.md](README.md#build-and-test) and [packaging/README.md](packaging/README.md)
for toolchain versions and build commands. The desktop and browser share the React
frontend and authenticated controller API. Linux operations run in the dedicated
helper; Windows operations use the native engine. Do not introduce another runtime
or a second Compose implementation.

Build the frontend first because Go embeds `frontend/dist`. In `frontend`, run
`npm ci`, `npm run check`, `npm run build` and `npm run test:e2e`. Then run
`go test ./...` and `go vet ./...` from the repository root. On Windows,
run both scripts under `packaging/windows/tests` with Windows PowerShell 5.1.
These prerequisite tests replace system-changing commands; they do not enable
features or install WSL.

Use `gofmt` and the frontend's existing formatting conventions. Add focused tests
for changed behavior. State any tests not run and the actual limitation. For
changes to provisioning or privileged operations, include a reproduction and
preservation checks on an isolated Windows target. Never run destructive cleanup
or engine restart tests against someone else's workloads.

Application contributions are licensed under Apache 2.0. Preserve third-party
notices and update them when a shipped dependency changes. A binary release must
include its exact corresponding Debian sources; see the packaging instructions.
