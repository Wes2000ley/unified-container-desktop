# Windows installer and Linux runtime

Run the single **UnifiedContainerDesktop-Setup.exe** to install UCD. This guide
explains installer behavior, machine identities, retained data and package builds.
For a quick start, see the [main README](../README.md). For networking, credentials,
file operations and engine limits, see the [operating guide](../docs/OPERATIONS.md).
Qualification results are recorded separately in [VALIDATION.md](../VALIDATION.md).

## Installation

Run **UnifiedContainerDesktop-Setup.exe**. It is a WiX 4 Burn bundle containing the
application MSI, fixed prerequisite scripts, the verified Microsoft WSL MSI and
the Debian rootfs. No adjacent files or elevated shell commands are required.
Approve UAC, then let Setup enable the four Windows features and install WSL when
needed. Existing WSL 2.5.4 or newer is retained. Hardware virtualization must
already be enabled in BIOS/UEFI; unsupported Windows editions and Docker Desktop
coinstallation fail with an actionable prerequisite error.

Features and WSL have separate restart boundaries. Full UI asks whether to restart
now; closing defers it. Burn caches the installation and resumes at the next
package after restart and sign-in. For unattended deployment, explicitly use
`UnifiedContainerDesktop-Setup.exe /quiet /norestart /log C:\path\setup.log`.
Without `/norestart`, WiX's unattended mode permits a required restart. A deferred
prerequisite restart returns a restart status, not completed application setup;
check the resumed installation before deploying workloads. Never request an
unattended restart on a machine whose workloads cannot be interrupted.

The MSI verifies features are enabled and WSL is usable before provisioning; it
no longer enables features itself or schedules an unconditional first-install
restart. Setup adds the account identified by Windows Installer's `UserSID` to
Container Operators when it is a normal local/domain/Entra user SID. SYSTEM
installs do not pick an interactive user. UAC using alternate credentials can
identify the administrator rather than the desktop user. Other memberships are
managed explicitly through UCD's Access settings.

Burn's main log links each prerequisite and MSI log. Provisioning failures also
write `%SystemRoot%\Temp\Ucd-provision-*.log`. Obtain the unsigned beta through a
trusted channel and verify the published SHA-256 independently. MSI payload and
Burn payload hashes validate their contents; they do not replace code signing.
Setup does not uninstall Docker Desktop, unregister another distribution, or
remove shared Windows features/WSL when UCD is uninstalled.

The MSI puts `%ProgramFiles%\UnifiedContainerDesktop` first on the machine PATH. In a new terminal, `docker` defaults to Linux; `docker --engine windows` selects Windows, including Compose and Buildx. Both use the authenticated controller broker. The official Docker executable is installed as `docker-cli.exe` behind this entry point. Existing `ucd` management commands remain available.

The MSI installs binaries under `%ProgramFiles%\UnifiedContainerDesktop` and stages the Linux rootfs and helper under `%ProgramData%\UnifiedContainerDesktop\bootstrap`. `UcdService` is a local Administrators member with service-logon, backup and restore privileges, denied local and remote interactive logon. A random service password is stored in the Windows LSA private-data store and SCM. Its Windows profile and WSL disk live under `%ProgramData%\UnifiedContainerDesktop\service-profile` and `wsl`, respectively. The controller imports the bundled rootfs as `UCD-Linux` under that identity. It writes WSL settings in that service account's profile and manages the Linux helper over mutually authenticated TLS on localhost. The Windows daemon uses `%ProgramData%\UnifiedContainerDesktop\config\docker-daemon.json` and `npipe:////./pipe/UCDDocker`.

The package installs the official Windows Docker CLI with Compose 5.7.1 and Buildx 0.38.2. `ucd docker --engine windows|linux ...` relays the selected engine's Docker API through the authenticated controller, including plugin and stream traffic; Docker `--host` and `--context` overrides are rejected. By default it uses the invoking user's UCD-specific Docker configuration at `%APPDATA%\UnifiedContainerDesktop\docker`, registers the installed plugin directory first among extra directories, and configures the bundled `docker-credential-ucd` helper. This keeps the ordinary Docker Desktop configuration separate. `ucd plugins install` initializes the UCD client configuration explicitly. An explicit Docker `--config` selects that configuration instead: UCD registers its plugin directory there and preserves that configuration's credential store.

The GUI and `ucd registry ls`, `ucd registry login --username NAME --password-stdin [--server HOST]`, and `ucd registry logout [--server HOST]` use one protected machine registry store shared across both engines. The default UCD Docker CLI configuration reaches that store through the credential helper; an explicit `--config` uses the selected configuration's own credential store instead. The machine registry configuration does not support external credential helpers.

The controller checks current local operator-group membership for management requests. Raw Docker pipes are restricted to SYSTEM, administrators and their service owners; the controller periodically reconciles those ACLs. Existing connections retain their granted Windows access until disconnected. The desktop has an application and notification-area icon, with per-user settings for startup, minimized startup and close to tray. Interface preferences, including refresh and log display, are stored in the local app profile, and recent activity supports pagination.

On upgrade the existing service-owned distribution is preserved: the controller refreshes its helper, file utility and Linux Buildx 0.39.0 plugin. If process tools are missing or unusable, bootstrap installs `procps` and required libraries offline from the verified staged rootfs; it reports dependency or version conflicts. It does not re-import the staged rootfs or upgrade the existing Linux Docker Engine. A fresh distribution receives the rootfs's pinned engine. Plan an engine upgrade separately and verify the actual installed version.

Setup refuses to replace an existing `UcdService` without the matching LSA secret or an existing product-named service pointing elsewhere. It also refuses installation while another Windows `dockerd.exe` is running. It never changes existing daemon services, NAT networks or unrelated WSL distributions. Setup completes any required optional-feature restart before the main MSI is installed. MSI major upgrades restore application files when a transaction rolls back. Uninstall removes application services and installed files but preserves the account, LSA secret, profile, distribution, projects, images and volumes in ProgramData. Administrators should back up this data before upgrading. Current Setup upgrade checks passed on the development host. Fresh installation, current-bundle uninstall/rollback and reboot/resume are not live-qualified; older MSI/VM results remain separate in VALIDATION.md.

## Build

Prerequisites: Go 1.26.11, Node.js 24.15.0, Docker with a Linux engine, and WiX 4.0.6. WiX 4 is used under its permissive license; WiX 7 has separate EULA acceptance requirements. Docker's Windows binary archive and the Windows Compose/Buildx and Linux Buildx plugins are downloaded from their official releases and checked against pinned SHA-256 values. The Debian image is pinned by its OCI digest; Docker Engine and CLI 29.9.3, containerd 2.4.2 and Buildx 0.39.0 are pinned and installed from Docker's signed Debian repository. The resulting rootfs and the final MSI have SHA-256 files. System package dependencies come from signed current Debian/Docker package repositories, so rebuilding later can produce a different rootfs hash even when the pinned base and Docker version are unchanged. Windows images remain subject to host compatibility and isolation rules. See the [Windows build limitations](../docs/OPERATIONS.md#platform-boundaries) before selecting a builder.

Run from the repository root, using a task-specific Docker configuration if desired:

```powershell
./scripts/build.ps1
./scripts/build-rootfs.ps1 -Output C:\build\debian-rootfs.tar -DockerConfig C:\build\docker-config
./scripts/package.ps1 -Rootfs C:\build\debian-rootfs.tar -RootfsSources C:\build\debian-rootfs.tar.sources.tar.gz -CacheDirectory C:\build\ucd-cache -Wix C:\tools\wix4\wix.exe
```

The desktop includes a committed Windows resource object, so ordinary builds do not require icon-generation tools. After changing `frontend/public/ucd-icon.svg`, regenerate the transparent PNG, multi-size ICO and resource object with ImageMagick 7 and GNU windres:

```powershell
magick -background none -density 384 frontend/public/ucd-icon.svg -resize 256x256 PNG32:cmd/desktop/assets/ucd-icon.png
magick -background none -density 384 frontend/public/ucd-icon.svg -define icon:auto-resize=256,128,64,48,32,24,16 cmd/desktop/assets/ucd-icon.ico
Push-Location cmd/desktop
windres -i ucd.rc -o ucd_windows_amd64.syso -O coff
Pop-Location
```

`dist` receives the single-file Setup executable, internal MSI, SHA-256 sidecars,
corresponding Debian source archive and rootfs/source hash association, and the
license/install/validation documents. `build-rootfs.ps1` builds the runtime target,
then collects each exact Debian source package into a separate archive. The
source stage fails if an exact package is unavailable; it never substitutes a
newer source. `package.ps1` requires `-RootfsSources` and verifies that the archive
and runtime match the source manifest. Release both the Setup and corresponding
sources together. Docker/Compose/Buildx and WSL keep their own licenses and
versioned upstream sources; see NOTICE and THIRD_PARTY_NOTICES.txt.

CI runs Windows/Linux Go checks, prerequisite behavior checks and frontend/browser
fixture tests. The release workflow builds the rootfs and matching source archive
on Linux, then binaries, MSI and Setup on Windows. Artifacts are not automatically
published as a public release. Code signing requires a separately supplied
identity; these beta packages are unsigned. Fresh-install, reboot/resume,
representative upgrade, repair and removal checks need a Windows 11 target with
hardware virtualization and administrator privileges. A build or extracted
manifest does not establish those live results.

When the default `node.exe` is older than 24.15.0, pass a newer executable with `-Node`; if npm is installed separately, pass its `npm-cli.js` with `-NpmCli`. The build script temporarily prepends that Node directory to PATH and restores its process environment on exit. Major upgrades share stable component identities and use Windows Installer rollback for application files. A failed upgrade or repair attempts to restore and start both services; it does not preserve an intentional prior stopped state.
