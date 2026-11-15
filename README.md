# Unified Container Desktop

**Windows and Linux containers, together.** UCD runs one shared native Windows
Docker Engine and one Linux Docker Engine in a dedicated Debian WSL 2
distribution. Manage both at the same time through a desktop app, the same
interface in your browser, and the system-wide `docker` and `docker compose` CLI.

The engines start with Windows and keep running when the interface closes or
users sign out. Authorized users share the same containers, images, volumes and
registry sign-ins. No cloud account, subscription or telemetry is required.

[Download 0.4.17 beta](https://github.com/Wes2000ley/unified-container-desktop/releases/tag/v0.4.17)
· [Installation details](packaging/README.md#installation)
· [Operating guide](docs/OPERATIONS.md)
· [Report a bug](https://github.com/Wes2000ley/unified-container-desktop/issues/new/choose)

> **Beta:** packages are unsigned. Upgrade and dual-engine tests passed on the
> development host. Fresh-machine prerequisites and the new Setup's reboot/resume
> path have not been live-qualified. Read the [beta limits](#beta-status-and-limits)
> before installing on a machine you depend on.

## What you can do

| Area | Features |
| --- | --- |
| Containers | Create, start, stop, restart, pause, remove, inspect, follow logs, view stats, open an exec terminal and manage files. |
| Images | Pull, push, build, tag, inspect, save, load, import and remove. |
| Volumes and storage | Create and inspect volumes, browse files, back up, restore and clone data; review disk usage and filtered cleanup. |
| Networks | Create and inspect networks, connect containers, publish TCP/UDP ports and configure networking defaults. |
| Compose | Import or create projects, edit source, deploy services and follow progress and logs. A logical application can contain Linux and Windows Compose parts. |
| Settings | Linux resource limits and Resource Saver, virtual file shares, Docker/container proxies, registry mirrors, DNS/network defaults and user access. |
| Shared access | One registry sign-in store and the same two engines for all authorized Windows users. |

The desktop and browser use the same React interface and controller. The CLI
uses the bundled official Docker client, Compose and Buildx through UCD's
authenticated engine broker. Kubernetes and Swarm management are outside scope.
Engine-specific restrictions are described in the [operating guide](docs/OPERATIONS.md).

## Install

You need **Windows 11 Pro or Enterprise x64, build 22621 or newer**, administrator
access, hardware virtualization enabled in BIOS/UEFI, and sufficient disk space
for the images and containers you plan to run. Windows Home and Windows Server
are outside this beta's scope.

1. Back up existing workloads and uninstall Docker Desktop. UCD blocks
   coinstallation and does not remove Docker Desktop's data automatically.
2. Download **UnifiedContainerDesktop-Setup.exe** from the
   [beta release](https://github.com/Wes2000ley/unified-container-desktop/releases/tag/v0.4.17).
   It is the only installation download you need. Obtain it and its expected
   SHA-256 through a trusted channel; the current UCD package is unsigned.
3. Run Setup and approve Windows elevation. Setup enables Containers, Hyper-V,
   Windows Subsystem for Linux and VirtualMachinePlatform. It installs bundled
   Microsoft-signed WSL when missing or older than 2.5.4, then provisions UCD's
   own Debian distribution and automatic services. Newer WSL is preserved.
4. If requested, choose **Restart** or **Close** to restart later. Cached Setup
   is designed to continue after restart and sign-in. Windows may need Windows
   Update or its configured feature source. A shared WSL update can affect other
   distributions.
5. Open **Unified Container Desktop** from Start, then open a new terminal for
   the updated system PATH.

Setup adds the installing Windows account to **Container Operators**. If UAC uses
a different administrator account, that account receives membership instead.
SYSTEM deployments require an administrator to grant the intended user's access.
Operators have highly privileged access to the engines; add only trusted users.
Infrastructure settings require an elevated administrator. See [Security](SECURITY.md).

Uninstall removes application services and installed files while retaining the
machine-owned distribution, configuration, projects, images and volumes. See
[installation and recovery details](packaging/README.md) before an upgrade or removal.

## Quick start

Linux is the default engine. Put `--engine windows` before Docker's own options
to select Windows. Both engines can run at once.

```powershell
docker ps
docker compose up -d
docker --engine windows ps
docker --engine windows compose up -d
```

Run Compose commands from the directory containing the intended project. Each
engine has its own images, networks and volumes. Use compatible Windows images;
the engines communicate through published host endpoints rather than a shared
container network or Compose dependency graph.

Use `docker login` or the **Registries** page to sign in. The default UCD CLI
configuration uses the same protected machine credential store as both GUIs;
signing in or out affects all authorized operators. Explicit Docker `--config`
overrides retain that configuration's credential behavior.

UCD also provides commands for unified inventory and managed projects:

```powershell
ucd engine ls
ucd container ls
ucd project ls
ucd diagnostics
ucd --help
ucd web
```

`ucd web` prints a loopback launch URL containing a temporary session token.
Keep the URL private. The adapter runs as your Windows user; Ctrl+C stops the
adapter while the engines keep running. Desktop startup and tray preferences
are separate from the automatic machine services.

See the [operating guide](docs/OPERATIONS.md) for files, networking and DNS,
proxies, Resource Saver, access, registry authentication and recovery.

## Beta status and limits

Version **0.4.17** passed real MSI-to-Setup and Setup-to-Setup upgrades on the
development host, preserving checked settings, container identities and volume
data. Windows/Linux CI, frontend tests, installed browser checks and repeated
exec-output tests passed. Exact commands, retained failed trials and test
boundaries are in [VALIDATION.md](VALIDATION.md).

- UCD executables, MSI and Setup are **unsigned**.
- Fresh-machine feature/WSL installation, the new Setup's reboot/resume, and
  current-bundle uninstall/rollback have not been live-qualified. Older MSI/VM
  evidence is separate and does not qualify these paths.
- Microsoft's documented Windows 11 container support path uses Docker Desktop;
  this standalone Windows engine integration does not inherit that support
  guarantee. See the [platform boundaries](docs/OPERATIONS.md#platform-boundaries).
- The Windows `windowsfilter` engine uses classic Docker builds. Windows BuildKit
  `RUN` is not qualified. Windows container filesystem export is unavailable;
  Windows commit requires a stopped container.
- Windows NAT recovery, VPN/firewall behavior and cross-user WSL interactions
  need qualification for the intended deployment. Back up important data and
  inspect actual port exposure.
- Application updates are installed through Setup; automatic update discovery
  is not implemented.

This is a public beta with documented limits, not a claim of complete Docker
Desktop parity or an independent security certification.

## Build and test

Use Go **1.26.11** and Node.js **24.15 or newer**. Packaging additionally uses
WiX **4.0.6** and a Linux Docker engine. The build script installs locked frontend
dependencies and builds the shared assets before compiling the Go applications.

```powershell
.\scripts\build.ps1
go test ./...
go vet ./...
go mod verify
Push-Location frontend
npm run check
npm run test:e2e
Pop-Location
```

The browser fixture tests are separate from installed dual-engine acceptance.
`npm run test:web` exercises the built production adapter and needs a working
local installation for full service checks. See [Contributing](CONTRIBUTING.md),
the [frontend guide](frontend/README.md), [packaging](packaging/README.md) and
[release instructions](RELEASING.md) for details.

## License

UCD source is licensed under [Apache 2.0](LICENSE). Third-party components retain
their own licenses; see [NOTICE](NOTICE) and
[THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt). Releases include the matching
Debian corresponding-source archive and WiX sources for redistribution.
