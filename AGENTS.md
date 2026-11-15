# Unified Container Desktop

Build the specification supplied in the updated active goal. Keep the implementation thin: official Compose v5 SDK, native Windows Docker Engine, one dedicated noninteractive service-account-owned Debian WSL2 distribution stored in ProgramData, Wails 3 and React/TypeScript. The user's WSL2 update supersedes the old Hyper-V VM architecture. No Hyper-V VM backend, cloud service, telemetry, fake production data, or alternate runtimes.

Use additional agents only when the user explicitly authorizes them for the current task. Agents share this worktree; preserve other edits and obey assigned file ownership.

Never access or use AWS authentication material without explicit authorization for that action. Do not provision, enable Windows features, change services, networking, group membership, install this product, or alter existing Docker/Hyper-V resources without explicit target authorization. Notify the user before native administrator prompts and obtain approval before restarting Windows. Preserve unrelated workloads and resources.

Commit or push only when the user requests it. Never rewrite history or discard unrelated changes without explicit authorization. Use pinned dependencies. Only the primary agent edits root go.mod/go.sum. Read the supplied contracts in internal/api before implementation. No TODO implementations, silent fallback, or simulated success. Errors must retain the underlying cause.

Use gofmt and focused behavioral tests. Frontend production uses real Wails bindings, with test-only API simulation explicitly isolated under tests. Keep secrets out of logs and responses. Persistent infrastructure state and shared registry credentials belong under ProgramData or /var/lib/ucd, never a Windows user profile. Current-user desktop startup preferences use HKCU, and interface preferences and the UCD Docker client configuration are user-scoped; these must not create separate engines or alter another application's Docker configuration.
