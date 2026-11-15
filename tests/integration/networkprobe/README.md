# Host networking fixture

This small standard-library program runs in scratch Linux and Nano Server Windows images. It checks startup DNS, HTTP, UDP and published endpoints without downloading application dependencies. `serve` listens on TCP 5000 and UDP 5001. The fixture does not belong in the installed payload.

From the repository root in PowerShell, build an isolated fixture directory:

```powershell
$fixture = Join-Path $env:TEMP ('ucd-network-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixture | Out-Null
Copy-Item tests/integration/networkprobe/Dockerfile* $fixture
$savedGoos = $env:GOOS
$savedCgo = $env:CGO_ENABLED
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'linux'
    go build -trimpath -o "$fixture/network-probe" ./tests/integration/networkprobe
    if ($LASTEXITCODE) { throw 'Linux probe build failed' }
    $env:GOOS = 'windows'
    go build -trimpath -o "$fixture/network-probe.exe" ./tests/integration/networkprobe
    if ($LASTEXITCODE) { throw 'Windows probe build failed' }
} finally {
    $env:GOOS = $savedGoos
    $env:CGO_ENABLED = $savedCgo
}
docker build -t ucd-network-probe:latest $fixture
docker --engine windows build --isolation hyperv -f "$fixture/Dockerfile.windows" -t ucd-network-probe:latest $fixture
$env:UCD_WINDOWS_NETWORK_PROBE_IMAGE = 'ucd-network-probe:latest'
go test ./internal/engine -run '^TestWindowsHostAliasesInstalledEngine$' -count=1 -v
```

The Windows test reaches the installed authenticated broker. It creates and removes only its own containers and verifies both aliases during the initial application process, including an explicit host override. Image builds and any newly pulled Nano Server base image remain for the operator to remove after other tests finish. Do not prune unrelated images, networks or volumes.

Use ordinary `docker run --rm ucd-network-probe dns host.docker.internal`, `http URL`, and `udp HOST:PORT` commands for Linux; add `--engine windows` and `--isolation hyperv` for Windows. Cross-engine checks must use deliberately published host endpoints. Windows and Hyper-V firewall policy must permit the chosen test traffic. Restore temporary settings and firewall rules afterward.
