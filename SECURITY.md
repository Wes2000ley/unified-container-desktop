# Security

UCD is a local, privileged container-management application. The current beta is
supported on a best-effort basis; there is no promised response time or completed
independent security certification.

## Reporting

Use GitHub's **Security → Report a vulnerability** when private reporting is
available. If it is unavailable, open an issue requesting a private reporting
channel without including exploit details or secrets. Do not put credentials,
private source, service-account secrets or browser launch tokens in public issues.
Include the affected version, Windows build, required caller permissions,
reproduction, expected control and observed impact in the private report.

## Privileges and boundaries

The controller runs under the noninteractive local administrator `UcdService`;
the native Windows Docker service runs as LocalSystem. Both engines and registry
credentials are shared machine resources. Container Operators are trusted
administrators of those resources: Docker management can lead to host-level
control. Grant that membership only to trusted users. This is not a hostile-user
or hostile-container isolation product, and administrators retain OS control.

The controller must authenticate named-pipe callers and authorize operations.
Ordinary operators must use its broker rather than the raw engine pipes.
Read-only users must not acquire mutation rights through another API path.
Infrastructure settings require an elevated administrator. Registry credentials
and service-account passwords must remain protected and absent from logs.

The browser adapter binds to loopback and requires its session token. Keep its
launch URL private. Local browser origins and unprivileged local processes must
not bypass the adapter's authentication or the controller's authorization.
Project imports, file/archive operations and helper requests must respect their
documented path boundaries and size limits. Treat failures of these controls as
security bugs, including when reachable only by another local user.

The product does not automatically uninstall Docker Desktop or remove unrelated
WSL distributions, HNS networks or data. Installation and removal must preserve
unrelated resources. Review source and obtain packages through a trusted channel:
the current UCD executable and MSI are unsigned. Checksums establish integrity
against a trusted expected hash; they do not authenticate the publisher.
