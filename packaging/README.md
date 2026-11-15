# Windows installer and Linux rootfs

Version 0.4.17 is installed on the development host through the single Setup executable. MSI-to-bundle and subsequent bundle-to-bundle upgrades returned 0; all 21 installed program/bootstrap hashes matched. The final upgrade preserved container identities, named-volume contents and settings. The Linux fixture kept running; the Windows fixture resumed under its restart policy after the native engine restarted. Both machine services are automatic and running. Repeated desktop and Start menu launches retain one desktop process; Windows shows one visible product entry. Both-engine Docker/Compose/Buildx checks, 50 immediate exec commands, stdin/log checks and the production browser interface passed. No Windows restart occurred. The user declined another test VM, so fresh installation and actual restart/resume of the new bundle remain unqualified. See [VALIDATION.md](../VALIDATION.md) for receipts, exact scope and retained failed trials.

Installed 0.4.14 acceptance passed 59 workflow groups across networking, published ports, Resource Saver, virtual file shares, HTTP/SOCKS5 proxies and normal-user core GUI/CLI workflows. Both engine inventories and original settings were restored. The earlier 0.4.9 installed run passed 69 general normal-user CLI checks; that general suite was not repeated for this release. Docker, Compose, Buildx and UCD help explain UCD and engine selection. The acceptance VM has been retired at the user's request; earlier packages, evidence and failed trials remain in the validation record.

The standard command is installed at `C:\Program Files\UnifiedContainerDesktop\docker-command\docker.exe`. The installer adds that directory and the installation root to machine PATH. `ucd.exe` and the Docker command share one bundled official client, credential helper and plugin directory. The distinct dispatcher path lets the MSI remove the previous root-level official `docker.exe` during upgrade without treating UCD's unversioned dispatcher as an older Docker client.

Version 0.4.7 sets the standard MSI `DISABLEADVTSHORTCUTS` property so the installed shortcut is regular; the service-only bootstrap ACLs remain intact. Shortcut creation runs after the old product is removed and before transaction finalization, preserving the Start menu entry during upgrades. The actual normal-user shortcut launch passed with both Compose containers running: both service process IDs stayed unchanged, no UCD installer event occurred, and both containers remained running. The upgrade did not request another Windows restart. The current test scope and retained failed trials are recorded in [VALIDATION.md](../VALIDATION.md) and [ACCEPTANCE.md](../ACCEPTANCE.md).

The prior 0.3.1 WSL startup fix separates diagnostic stderr from command results. Before the user's 2026-11-14 restart, Windows process- and Hyper-V-isolated test starts on the development host failed with HCS `0x80070032`; tracing identified `OrchestratorRegistry.h(63)`. After that restart, `CCGLaunchPad.dll` was present and Hyper-V-isolated Windows workloads passed alongside Linux workloads. A fresh installation or shared WSL update can require a reboot. Interactive Setup offers Restart or Close; use `/norestart` for unattended deployment that must not restart Windows.

The 0.3.0 core workflows passed scoped checks on one disposable Windows 11 VM. This does not establish universal Windows qualification. Earlier failures and test boundaries are preserved in [VALIDATION.md](../VALIDATION.md) and [ACCEPTANCE.md](../ACCEPTANCE.md).

| Check | Observed result |
| --- | --- |
| Upgrade | MSI exit 0 and all 12 installed payload hashes matched; the report's recovery-policy text assertion failed, and a separate native read-back confirmed the intended policy. |
| Real engines | The installed 0.3.0 API regression passed 42 checks; all 24 recorded fixtures were absent afterward. |
| Import and cleanup | A compatible Windows image import passed. Filtered prune passed for all five scopes on both engines; the Windows cache test removed one 0-byte record. |
| Interface | The live browser and the native desktop opened both engines' original workloads; native UI Automation ran as a nonadministrator operator. |
| Boot and preservation | Normal restart and cold boot passed without login, preserving the checked original data and log prefixes and producing fresh startup lines. |

The boot checks followed a one-time repair of a 160-byte NUL gap in the tested VM’s Linux Docker log. The cause is unproven; UCD does not automatically repair corrupted Docker logs.

Earlier 0.1.0 VM trials covered service-owned WSL, installer rollback, repair, uninstall and reinstall, including a deliberate Windows NAT recovery. Their scope and failed trials remain in the validation record. After installing, run `ucd web` for the same interface in a browser; the printed localhost launch URL authorizes that foreground session using the invoking Windows user's permissions.

The machine-wide package targets Windows 11 Pro or Enterprise x64, build 22621 or newer. It uses one `UcdService` local service account and an isolated `UCD-Linux` WSL 2 distribution. Windows images and containers use the separate `UCDDocker` service. Authenticated users can read settings; changing them requires an administrator. Container Operators use the authenticated controller broker rather than direct raw Docker pipe access. Docker-level management remains highly privileged and may provide administrator or SYSTEM-equivalent access; add only trusted operators. Administrators retain operating-system control of the machine and direct pipe access.

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

Resource Saver can stop the idle Linux Docker service and socket while leaving the dedicated WSL distribution and helper available; it is disabled by default and controller operations wake it. Docker outbound proxy and container/build proxy defaults are configured separately. The published-port binding default applies to newly created containers and managed Compose projects on both engines, while explicit host addresses are preserved. Other networking settings cover the Linux bridge subnet, IPv4/IPv6 modes, DNS inhibition and per-network override; Linux host networking requires an administrator opt-in. Address-family defaults apply to new custom Linux bridge and Compose networks created through the GUI/API, managed Compose or brokered Docker CLI, unless the caller explicitly selects a family. Docker’s built-in default bridge can enable IPv6 but cannot disable IPv4, including in IPv6-only mode; see [Docker’s bridge documentation](https://docs.docker.com/engine/network/drivers/bridge/#ipv6-only-bridge-networks). Some daemon changes require restarting the indicated engines. Existing custom networks are not rewritten; the built-in bridge is reconfigured when its subnet or IPv6 setting changes. Virtual file shares mount administrator-approved local Windows directories directly through DrvFS under `/mnt/ucd`; UNC paths, drive roots and UCD infrastructure directories are excluded. Existing share cleanup remains compatible.

On upgrade the existing service-owned distribution is preserved: the controller refreshes its helper, file utility and Linux Buildx 0.39.0 plugin. If process tools are missing or unusable, bootstrap installs `procps` and required libraries offline from the verified staged rootfs; it reports dependency or version conflicts. It does not re-import the staged rootfs or upgrade the existing Linux Docker Engine. A fresh distribution receives the rootfs's pinned engine. Plan an engine upgrade separately and verify the actual installed version.

For UCD-created Windows containers and managed Windows Compose projects, the controller forwards published TCP/UDP ports from the requested host address to the container's single usable NAT endpoint; it preserves the original Compose YAML. Linux public ports are Windows controller TCP/UDP listeners forwarding to distinct private Docker loopback endpoints, with configurable defaults for new Linux networks and containers. The controller service SID is enabled as UNRESTRICTED; installer-owned inbound rules are scoped to its installed executable and Windows service. Repair preserves administrator rule restrictions. Uninstall stages those rules for removal, restores them if the MSI transaction rolls back, and removes them on commit. `ucd docker` reaches the selected engine through the authenticated controller, where UCD policies also apply. In the 0.2.0 VM trial, native Windows Docker NAT rejected an explicit host-IP publish; an unqualified publish worked and reported a `0.0.0.0` host address. Check the resulting exposure on the target host. A 0.1.0 reboot check found one Windows TCP listener missing after a bind error despite a responsive direct container endpoint. Its port lay in the observed Linux ephemeral range; a separate Windows bind sample failed for sampled ports inside that range and succeeded for sampled ports outside it. This supports an inference involving [WSL mirrored networking's host reservation](https://github.com/microsoft/WSL/issues/12228); the reservation handle was not inspected directly. A new outside-range fixture passed TCP/UDP checks and a controller restart in that VM. Actual ranges and availability can differ by host and boot; deployment-specific firewall and remote-access behavior remain to be tested where needed.

If a Windows workload remains stopped after reboot, inspect its container error, Docker events and host network ranges before planning recovery; preserve project source and named volumes. A controlled subnet probe reproduced the HNS error with the former workload prefix while a different test subnet was created and removed successfully; the original event did not name the failing network. For that VM, an administrator configured a nonoverlapping Compose subnet and reconnected the existing container; a later reboot passed. This is not automatic repair or a guarantee for other hosts. Choose any explicit `networks.default.ipam.config` subnet and gateway only after checking existing host networks and routes. The pinned Windows NAT driver uses HNS/windowsIPAM rather than Docker `default-address-pools`. Microsoft documents a NAT persistence boundary for Windows Server 2019 and later; the observed test failure was on Windows 11. See [Microsoft's Windows container network driver documentation](https://learn.microsoft.com/en-us/virtualization/windowscontainers/container-networking/network-drivers-topologies) and [Docker Compose network IPAM](https://docs.docker.com/reference/compose-file/networks/).

The VM used the following Windows Compose configuration. The addresses are an example, not a range reserved for every deployment; select a nonoverlapping subnet and gateway for the actual host.

```yaml
networks:
  default:
    driver: nat
    ipam:
      config:
        - subnet: 10.203.240.0/24
          gateway: 10.203.240.1
```

In that recovery, Compose up still failed on the missing old network. The administrator connected the stopped container to the verified replacement network with its aliases and started it. Review the specific container, network membership and data before planning any comparable intervention.

Setup refuses to replace an existing `UcdService` without the matching LSA secret or an existing product-named service pointing elsewhere. It also refuses installation while another Windows `dockerd.exe` is running. It never changes existing daemon services, NAT networks or unrelated WSL distributions. Setup completes any required optional-feature restart before the main MSI is installed. MSI major upgrades restore application files when a transaction rolls back. Uninstall removes application services and installed files but preserves the account, LSA secret, profile, distribution, projects, images and volumes in ProgramData. Administrators should back up this data before upgrading. A VM suite passed upgrade to a 0.1.1 test MSI, an injected failure and rollback, repair, uninstall and reinstall after reboot with preservation checks. The restored 0.1.0 package, helper, ports and browser then passed their recorded checks on that VM.

## Build

Prerequisites: Go 1.26.11, Node.js 24.15.0, Docker with a Linux engine, and WiX 4.0.6. WiX 4 is used under its permissive license; WiX 7 has separate EULA acceptance requirements. Docker's Windows binary archive and the Windows Compose/Buildx and Linux Buildx plugins are downloaded from their official releases and checked against pinned SHA-256 values. The Debian image is pinned by its OCI digest; Docker Engine and CLI 29.9.3, containerd 2.4.2 and Buildx 0.39.0 are pinned and installed from Docker's signed Debian repository. The resulting rootfs and the final MSI have SHA-256 files. System package dependencies come from signed current Debian/Docker package repositories, so rebuilding later can produce a different rootfs hash even when the pinned base and Docker version are unchanged. Windows images remain subject to host compatibility and isolation rules. The tested `windowsfilter` engine uses Docker's classic Windows builder for ordinary builds. Its Docker-driver Buildx `RUN` fixture failed. A later scoped prune removed two damaged fixture-attributed records and restored Windows BuildKit cache reporting, although its reclaimed-space response was implausible. A separate exact-ID prune removed one healthy 0-byte cache record; it did not test nonzero reclamation or Windows BuildKit `RUN`. Docker's initial Windows BuildKit support uses the [containerd image store](https://docs.docker.com/engine/release-notes/28/#2820), and BuildKit cache is separate from classic image cache.

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
