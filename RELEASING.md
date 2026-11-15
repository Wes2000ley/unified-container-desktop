# Building a beta release

Use the pinned toolchains and commands in [packaging/README.md](packaging/README.md).
The application version in `internal/api/api.go`, frontend package/lock versions,
MSI and bundle must agree. A release tag must be `v` followed by that version;
packaging rejects a mismatched tag. Mark experimental releases as GitHub
**pre-releases** and retain the beta qualification limits in the release notes.

CI checks pull requests and master. The **Build release** workflow can be run
manually or by a version tag. It uploads build artifacts without publishing a
release or changing repository visibility. Verify the workflow's exact commit,
its test results and the package sidecars before preparing a draft release.

Publish these assets together:

- `UnifiedContainerDesktop-Setup.exe` and `.exe.sha256`: the single installation
  download and its checksum.
- `UnifiedContainerDesktop-rootfs-sources.tar.gz`, its `.sha256`, and
  `rootfs-sources.json`: exact Debian sources and runtime/source association.
- `wix-v4.0.6-source.tar.gz` and its `.sha256`: the bundled Burn/WixStdBA sources.
- `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.txt` and `INSTALL.md`.

UCD's tagged Git source contains the modified installer localization and the
application/build sources. Do not replace a release's source archives with
another build's sources. Retain sources while distributing the binaries.
The internal MSI is not required for users of Setup; do not tell users to fetch
the WSL MSI or a PowerShell script separately.

Verify live installation on the authorized target. Record feature/WSL changes,
restart behavior, service identities/startup, both engines, CLI/GUI access and
preservation of existing settings/data. Keep fresh installation and restart/resume
results separate from upgrade results. Do not run destructive acceptance scripts
against unrelated workloads. Existing historical evidence is in VALIDATION.md.

Current UCD packages are unsigned. Do not label them signed or suppress Windows
publisher warnings. SHA-256 files require a trusted distribution channel. The
release notes must describe the supported Windows editions, the standalone
Windows-engine support boundary, and any unqualified installer paths.

The production frontend dependency audit and the full developer-tool audit are
different checks. At 0.4.16 preparation, the production audit reported no
vulnerabilities. Nine moderate developer-tool reports trace to minimatch's glob
amplification advisory, [GHSA-g3hv-75wg-m249](https://github.com/advisories/GHSA-g3hv-75wg-m249).
The pinned typescript-eslint release still requires minimatch 10; no dependency
downgrade or incompatible override is applied. Review upstream fixes before the
next release. This observation is not a full security assessment.
