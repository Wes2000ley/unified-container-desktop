# Operating UCD

This reference covers permissions, container workflows, networking, credentials,
data preservation and engine-specific limits. Start with the [installation and
quick start](../README.md) if you are new to UCD. Historical test results and
qualification limits are recorded in [VALIDATION.md](../VALIDATION.md).

## Core workflows

The desktop and `ucd web` use the same React interface and machine service. The following is the implemented core workflow coverage; it is not a claim of full Docker Desktop parity or qualification on other host configurations.

| Area | Available workflows |
| --- | --- |
| Containers | Create or run, start/stop/restart/pause/kill/remove, inspect raw JSON and mounts, follow logs, view reported statistics, and open a PTY exec terminal. Browse, edit, upload and download files; rename, update limits, commit and export where supported by the engine. |
| Images | Pull, inspect and view history, tag, push, build, save, load, import and remove. |
| Volumes | Create and inspect, browse and edit files, back up, restore, clone, empty and remove. |
| Networks and storage | Create, inspect and remove networks; connect or disconnect containers. View disk usage and preview and confirm scoped pruning. |
| Registries | Sign in once for GUI and UCD Docker CLI pulls, pushes and builds on both engines; sign out from the same shared store. |
| Compose | Create/import and edit projects, select engines and services, deploy or manage them, and follow progress and logs. |
| Docker CLI | Use `docker`, `docker compose` and `docker buildx` for Linux; add a leading `--engine windows` for Windows. Existing `ucd docker --engine ...` commands remain available. |

The interface exposes common options; use the Docker CLI for additional Docker commands and flags. Actions require the appropriate operator permissions. File browsing requires a running container and a writable, executable location for the packaged utility. On Windows, open Files and select **Prepare file access** when the utility is missing; confirm the prompt. Preparing a running container stops and restarts it, interrupting its services. UCD attempts to restart it even if installation fails and reports a restart failure. A stopped container remains stopped until you start it. A paused container must be unpaused before preparation; UCD rejects preparation while it is paused. Windows file operations run the utility as `ContainerAdministrator`. A versioned utility remains at `/.ucd-filetool-<16-hex-hash>` on Linux or `C:\Windows\Temp\.ucd-filetool-<16-hex-hash>.exe` on Windows; an observed different file at that path is rejected. Utilities from earlier versions remain in the container, so a new version may require preparation again.

Volume file operations use a temporary utility container and may need to pull its engine-compatible image. Backups and clones are live snapshots and may be inconsistent while applications write; restoring replaces the target's contents, and an error can leave it empty or partially restored. Windows volume archives handle regular files, directories, links, basic modes and modification times where supported, but Windows modes express read-only/writable behavior rather than full Unix permissions. NTFS ACLs and extended attributes are not fully preserved; an external or dangling directory symlink may be restored as a file-type symlink. Verify other metadata required by your workload separately.

In **Storage**, select an engine and cleanup scope. Optional Docker filters use one `key=value` per line. Containers, images and networks accept `label`, `label!` and `until`; volumes accept `label` and `label!`. Positive `label` filters may be repeated; only one `label!` exclusion is accepted. Build cache accepts `id`, `type`, `description`, `until` and `unused-for`, with one value per key; it rejects label filters and unsupported keys. A build-cache `id` is an unanchored regular expression: use `^...$` to match the full ID. Docker does not accept `until` and `unused-for` together. The preview's counts and space estimates cover the whole scope even when filters are set. Review the target engine and exact filters in the confirmation before pruning. Including all eligible volumes permits removal of unused named volumes and their data.

## Use

Open **Unified Container Desktop** from the Start menu. The Windows notification-area icon opens the window and has an **Exit** command. Closing the window keeps the desktop in the tray by default; **Settings → Desktop behavior** controls that behavior, starting the desktop when you sign in, and starting it minimized. The container services already start with Windows independently of these desktop preferences. Closing or exiting either client does not stop the engines or containers.

**Settings → Interface preferences** sets the data refresh interval, default log tail and timestamps for the current app profile. Recent events show 10 rows per page by default, with 25- and 50-row options. Linux resource limits, Resource Saver, Docker and container proxies, networking defaults, registry mirrors, operators and Virtual file shares are managed from Settings. Desktop startup and tray controls are available in the native app; the browser displays where to change them.

Resource Saver is off by default. When enabled and the Linux engine has been idle without running containers or active operations for the selected period, it stops the dedicated Linux Docker service and socket while retaining the WSL distribution and helper. Work through the controller wakes it; supported inventory reads can use the last cached data while it sleeps. A manually stopped engine remains stopped until explicitly started.

The two proxy configurations separately cover Docker/controller outbound traffic and defaults for new containers and builds on both engines. Each supports HTTP, HTTPS, SOCKS5 and a bypass list; proxy URLs cannot contain credentials. Container applications must honor the standard proxy environment variables. Explicit container or build proxy settings take precedence. Daemon proxy and mirror changes require the indicated engine restart.

The default published-port binding applies to new containers and managed Compose projects on both engines, preserving explicit host addresses. The other networking settings configure Linux: a bridge subnet, IPv4-only, IPv6-only or dual-stack network defaults, and DNS record inhibition with a per-network override. The address-family default applies to new custom Linux bridge and Compose networks created through the GUI/API, managed Compose or brokered Docker CLI when no explicit family is supplied. Docker’s built-in default bridge can enable IPv6 but always retains IPv4, even when the selected default is IPv6-only; see [Docker’s bridge documentation](https://docs.docker.com/engine/network/drivers/bridge/#ipv6-only-bridge-networks). Windows continues to use its native networking and HNS settings. Enabling Linux host networking is an explicit administrator opt-in; it shares the Linux host network namespace inside the dedicated WSL environment. Existing custom networks are not rewritten when defaults change; changing the built-in bridge subnet or IPv6 configuration requires an engine restart.

Linux bridge and Windows NAT containers can resolve `host.docker.internal` to a Windows host address and `gateway.docker.internal` to their selected network's gateway. Linux host DNS refreshes when Windows routing or addresses change; WSL uses mirrored networking, DNS tunneling and host-address loopback. Windows NAT aliases are written into a stopped container before its first process starts, including explicit Docker `--add-host` and Compose `extra_hosts` overrides; recreate those containers if the host address or their gateway changes. Windows transparent and overlay drivers retain their native DNS setup. Ordinary custom-network service DNS, `--dns`, `dns`, network aliases and explicit address overrides retain Docker/Compose behavior. Explicit DNS servers and Linux host/container network modes bypass UCD's Linux resolver; use `extra_hosts` when those resolvers do not provide the private Docker names. The host alias currently provides IPv4, including on dual-stack networks; it does not provide an IPv6-only host-access path.

Linux and Windows retain separate networks and communicate through published host ports. In 0.4.14 the Windows controller owns Linux public TCP/UDP listeners and forwards them to distinct private Docker endpoints. Ordinary Docker/Compose port syntax and public port inspection remain intact, including dynamic ports, duplicate publishes and IPv6-only Linux bridges. The installed host tests passed both cross-engine directions over TCP and UDP, both engines' access to native Windows host services, and Linux host networking access to Windows localhost. The controller's service SID is enabled so its program-and-service-scoped firewall rules apply. Localhost-only bindings remain private; broader access requires an explicit host binding and the applicable host firewall policy. Windows containers use native Windows network drivers. UCD does not add a VPN traffic proxy or promise connectivity through every corporate VPN/split-DNS policy.

The container services start automatically with Windows and keep running when the desktop closes. The desktop has one tray/window instance per Windows user session, independent of its executable path or version. Launching it again opens the existing window; startup in the background preserves its visibility. An administrator launch replaces a normal desktop instance before opening its elevated window. Other signed-in users can have their own desktop while managing the same two machine engines.

For the same interface in a browser, run:

```powershell
ucd web
```

Open the localhost URL printed by the command. The web adapter runs in that foreground process and uses that Windows user's identity to access the same machine service. It serves the same compiled React application as the desktop and forwards calls to the same named-pipe API. Stop it with Ctrl+C. A launch URL contains a temporary access token; keep it private. The adapter binds only to `127.0.0.1` and is not a remotely hosted administration interface.

Administrators configure infrastructure. Trusted members of the local **Container Operators** group deploy and manage applications and resources. Other authenticated Windows users can view permitted status, logs and settings; infrastructure changes require an elevated administrator client. Personal interface and desktop preferences are available without elevation. The controller checks current local operator membership for management requests; enabled groups already present in the caller's Windows token can also be members of Container Operators, while changes to those external groups follow Windows token refresh rules. UAC-filtered administrator membership does not grant the administrator role.

In 0.4.0, GUI and `ucd docker` access use the authenticated controller broker, and ordinary operators are denied direct access to the raw Docker pipes. System, service owners and administrators retain direct pipe access. This is an application access boundary, not a restriction on administrators: Docker-level container management remains highly privileged and may provide host administrative control. Add only trusted operators.

Create or import a Compose project in **Projects**, select its engine, and deploy it. Attach the second operating system's Compose file to the same logical application when needed. Each side remains an ordinary Compose project with its own networks and volumes. Partial failures report results for each engine. Cross-engine dependencies use explicit startup order and published endpoints; the engines do not share Compose's dependency graph.

Import copies the selected source directory, including dotfiles and build contexts, into machine-managed storage. Select a source directory containing only files intended for the project. Replacing an import synchronizes previously imported files, preserves untracked runtime files, and rejects conflicting runtime edits. Normal project removal preserves files and named volumes. Explicit volume removal requires a separate choice. A retained project directory can be imported again using its original project ID and the replacement option.

Linux bind mounts use Linux paths. An administrator can create a Virtual file share for an approved existing directory on a local Windows drive; its Linux path is `/mnt/ucd/<name>`. Drive roots, UNC paths and UCD machine infrastructure directories are excluded. Existing shares under the UCD shares directory remain supported, including cleanup of mounts created by earlier releases. New shares use direct WSL DrvFS mounts of the selected directory. UCD does not automatically translate arbitrary Windows paths in Linux Compose documents.

Linux published ports use Docker and WSL mirrored networking; the Linux network settings select the default binding for new networks and containers. Specify host addresses deliberately and check exposure on the target host. For UCD-created Windows containers and managed Windows Compose projects, the controller listens on the requested Windows host address and forwards TCP or UDP to the container's single NAT endpoint. It preserves the original Compose YAML and reconciles listeners with container lifecycle events; an unavailable or ambiguous endpoint or listener error is reported. The `ucd docker` CLI reaches Docker through the authenticated controller broker, where UCD policies also apply. In the separate 0.2.0 VM trial, native Windows Docker NAT rejected a raw CLI publish with an explicit host IP; an unqualified publish worked and reported `0.0.0.0:20080`.

A 0.1.0 VM reboot test found one Windows TCP listener missing after a bind error even though the container endpoint responded directly. The port lay in the observed Linux ephemeral range, and a separate Windows bind sample failed for sampled ports inside that range while succeeding for sampled ports outside it. This supports an inference involving [WSL mirrored networking's host reservation](https://github.com/microsoft/WSL/issues/12228); the reservation handle was not inspected directly. A new outside-range fixture passed simultaneous TCP/UDP checks and a controller restart in that VM. The usable range can differ between hosts and boots.

The installer adds UCD's installation root and its `docker-command` directory first on the machine PATH. The standard command is installed as `C:\Program Files\UnifiedContainerDesktop\docker-command\docker.exe`; it shares the client, plugins and credential helper used by `ucd.exe`. Docker Desktop must be uninstalled before installation; Setup reports a conflict before enabling features or changing WSL. The MSI also checks standard Docker Desktop uninstall registrations, and provisioning checks for running Desktop processes. Open a new terminal after installation to receive the updated PATH. The normal Docker command defaults to Linux and accepts `--engine windows` before Docker's own options. Both command names use the authenticated controller broker:

```powershell
ucd engine ls
ucd project ls
ucd compose --engine linux -f .\compose.linux.yaml -p example up -d
ucd project import --engine windows --id example -f .\compose.windows.yaml .
ucd project up --id example
ucd container ls
docker ps
docker compose up -d
docker buildx version
docker --engine windows ps
docker --engine windows compose version
ucd docker --engine linux ps
ucd docker --engine windows compose version
ucd docker --engine linux buildx version
ucd diagnostics
ucd --help
```

Run Compose commands from the intended build-context directory. The native `ucd` management commands return JSON; `docker` and `ucd docker` pass arguments, console streams and exit status through to Docker. A partially failed project action or incomplete unified inventory exits with a nonzero status while retaining successful engine results in the output.

`docker` and `ucd docker --engine windows|linux` start the bundled official Docker client, `docker-cli.exe`, and relay its API traffic through the authenticated controller to the selected engine. Compose 5.7.1 and Buildx 0.38.2 remain available for either engine; the Linux environment also has Buildx 0.39.0. Streams and exit status are forwarded. Docker `--host` and `--context` overrides are rejected because UCD selects the broker and engine.

By default, UCD uses its own client configuration at `%AppData%\UnifiedContainerDesktop\docker`, registers the bundled plugins, and selects `credsStore: ucd`. The installed `docker-credential-ucd.exe` reads and updates the same protected machine credential store used by the GUI. `ucd plugins install` prepares that UCD client configuration explicitly. Passing Docker's `--config` selects that configuration and its credential store; plugin registration preserves its existing authentication and context settings.

The **Registries** page and normal `docker login` or `ucd docker --engine linux|windows login` commands share one machine credential store under `%ProgramData%\UnifiedContainerDesktop\docker-config`. A sign-in or sign-out affects both engines and all authorized operators. The controller supplies Linux image and Compose operations with an operation-specific credential snapshot over authenticated TLS. Registry passwords and tokens are available only to authorized operators and elevated administrators, and are excluded from inventory responses and progress messages. Existing Linux-only sign-ins from earlier versions should be entered once in the shared Registries page.

Amazon ECR uses the normal Docker registry login and push protocol. A token supplied with `docker login --username AWS --password-stdin REGISTRY` is shared with the GUI and both engines while it remains valid; [ECR authorization tokens expire after 12 hours](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html). UCD does not obtain or renew AWS tokens. A registry-specific `credHelpers` entry for an installed ECR credential helper can provide automatic CLI authentication; it remains a per-user CLI credential source and is not copied into the GUI's shared store. Explicit `--config` and client authentication environment overrides retain their own credential behavior. Do not point an explicit config at Docker Desktop's removed credential helper.

## Operation and recovery

- Both Windows services start automatically. The controller loads the dedicated account profile, imports the bundled distro on first startup, installs the helper, and supervises an explicit WSL keepalive. Linux Docker and the helper run under systemd.
- An existing `UCD-Linux` distribution is retained during upgrade. The staged helper, file utility and Linux Buildx plugin are refreshed. If process tools are missing or unusable, bootstrap installs `procps` and required libraries offline from the verified staged rootfs; dependency or version conflicts are reported. The rootfs is not re-imported and does not silently upgrade the installed Linux Docker Engine. Plan and validate engine updates separately.
- Docker restart policies remain authoritative. Use `restart: unless-stopped` or the appropriate native policy in Compose for applications that should restart with their engine. The controller does not silently recreate projects.
- A Windows workload may remain stopped if its Docker NAT network cannot be restored after reboot, even with a restart policy. Inspect the container error, Windows Docker events and existing network address ranges before planning recovery. In the VM trial, an administrator selected a nonoverlapping subnet and reconnected the stopped container to the replacement network; this was a deliberate, fixture-specific recovery, not automatic repair. Preserve project source and volumes, and review network membership and aliases before any intervention.
- An offline engine is reported independently. Windows operations remain available if Linux provisioning fails. Examine `logs\controller.log` under ProgramData and run diagnostics. Correct a provisioning failure and restart the `UnifiedContainerDesktop` service to retry setup.
- CPU, memory and disk limits apply to the dedicated account's WSL VM. Changes are recorded as pending; a Windows restart is the normal application path. Disk expansion is attempted while that account's WSL VM is stopped. Shrinking is rejected. The application does not issue a global `wsl --shutdown` that would stop unrelated work.
- Docker proxy and registry mirror changes update daemon configuration while preserving unrelated keys. Restart the indicated engines to apply them. Container proxy defaults apply to new containers and builds. A failed update attempts to restore previous configuration and reports any rollback failure.
- Uninstall preserves the account, LSA secret, distribution, profile, projects, images and volumes. Back up these machine-owned data locations before upgrades. There is no automatic destructive cleanup or purge command.
- Upgrades use MSI major-upgrade file rollback and service restoration. A VM suite passed an upgrade to a 0.1.1 test MSI, an injected failure and rollback, repair, uninstall and reinstall after a real reboot, checking retained workloads and data. The 0.1.0 release was then restored through normal uninstall and setup; package, helper, port and browser checks passed. These results apply to that tested VM.

## Platform boundaries

Microsoft's documented Windows 11 container support path uses Docker Desktop. A standalone native Windows Engine on Windows 11 does not inherit that support guarantee. Docker publishes Windows engine binaries, but this product's integration requires its own acceptance evidence. See [Microsoft's support policy](https://learn.microsoft.com/en-us/troubleshoot/windows-server/containers/support-for-windows-containers-docker-on-premises-scenarios) and [Docker's binary installation documentation](https://docs.docker.com/engine/install/binaries/).

Windows NAT network recovery needs particular attention. Microsoft documents that NAT networks created on Windows Server 2019 and later are not persisted after reboot; the separate Windows 11 VM test encountered an HNS restoration error followed by a missing network. A controlled probe reproduced the error using the former workload subnet, which the Hyper-V Default Switch also used, while a different subnet was created and removed successfully. The original event did not name the failed network. Explicit subnet recovery and a later reboot passed on that VM; the controller does not automatically repair this failure. The pinned Windows NAT driver uses HNS address management, so Docker's `default-address-pools` setting does not control its allocation. See [Microsoft's Windows container network documentation](https://learn.microsoft.com/en-us/virtualization/windowscontainers/container-networking/network-drivers-topologies) and [Docker Compose network IPAM](https://docs.docker.com/reference/compose-file/networks/).

An administrator can specify the Windows project's default NAT network in its Compose source. This example uses the VM's test range; select a subnet and gateway that do not overlap this host's existing HNS, Hyper-V, WSL, VPN or other networks and routes:

```yaml
networks:
  default:
    driver: nat
    ipam:
      config:
        - subnet: 10.203.240.0/24
          gateway: 10.203.240.1
```

Changing Compose source alone did not recover the stopped VM fixture: its first Compose up still referenced the missing network. The administrator reconnected that existing container to the verified replacement network with its aliases and then started it. Inspect the specific project and preserve its data when planning such a recovery; the VM result does not guarantee the procedure or example range on another host.

Service-owned WSL is an integration using WSL's per-user registration model, not a Microsoft machine-wide WSL mode. Loopback TCP/UDP and boot without login passed in the disposable VM; a negative exposure probe from its Hyper-V host also passed. Firewall policy, VPN interactions and behavior alongside other users' WSL sessions depend on the intended deployment and have not been broadly qualified. No unrelated HNS/NAT networks or WSL distributions are modified by the product.

Other current boundaries:

- Imports are bounded to 512 files, 32 MiB per file, 56 MiB total source content and 4 MiB Compose YAML. Symlinks and paths escaping the project are rejected. Windows source imports cannot reliably preserve POSIX executable bits; set them in the Dockerfile when needed.
- Replacing multiple imported files uses staged validation, individual atomic file replacements and rollback on observed errors. It is not a crash-atomic directory transaction, and applications writing to their own source concurrently can still conflict.
- UCD-managed published ports must resolve to individual numeric ports; dynamic long-form published ranges are rejected. Windows forwarding accepts TCP and UDP, up to 1,024 published port entries per container. Each UDP listener tracks up to 1,024 client sessions, which expire after 60 seconds of inactivity; new sessions beyond that limit are dropped until capacity is available.
- Existing external Compose projects are discovered through Docker labels. Import a managed copy to edit or deploy their source through this application.
- Windows images must match the host's supported container compatibility and isolation rules. A genuinely built Windows image passed save/load in the 0.2.1 VM trial. In 0.3.0, a genuine modified Nano Server base-layer archive was imported and a Hyper-V container from it read the test content.
- The tested Windows engine uses `windowsfilter` and its ordinary image builds use Docker's classic Windows builder. Its Docker-driver Buildx `RUN` failed in the pinned [Windows graphdriver executor](https://github.com/moby/moby/blob/70efc0e/daemon/internal/builder-next/executor_nolinux.go); Docker's initial Windows BuildKit support is for the [containerd image store](https://docs.docker.com/engine/release-notes/28/#2820). The [BuildKit cache API](https://github.com/moby/moby/blob/70efc0e/daemon/builder/backend/backend.go) is separate from the classic builder's [image cache](https://github.com/moby/moby/blob/70efc0e/daemon/images/cache.go). After the failed Buildx fixture, Windows cache-usage requests reported missing layer paths; exact-ID prune attempts and a normal Docker service restart did not resolve them. A subsequent scoped `type=source.local` prune removed two fixture-attributed damaged records and restored cache reporting. The recovered-space value was implausible in that recovery response. A separate exact-ID prune removed one healthy, unused shared 0-byte cache record and restored the original empty cache inventory; it did not test nonzero byte reclamation or Windows BuildKit `RUN`.
- Confirmed 0.3.0 filtered-prune tests passed for Linux and Windows containers, images, networks, volumes and BuildKit cache. The Windows cache case removed a single 0-byte record. Tests used owned fixtures and checked preservation of original resources; they do not qualify unfiltered or broader destructive pruning.
- Windows Docker requires a stopped container for commit. The VM daemon also rejected filesystem diff on a running Windows container; stop it before attempting that operation. The native Windows daemon rejected container filesystem export even for a stopped container; the UI disables that operation for Windows. Image save and load are separate operations.
- CPU, memory and volume size values are displayed only when the engine reports usable statistics. An unavailable statistic is not presented as zero.
- Automatic online update discovery is not implemented; application updates are supplied through the Setup executable.
