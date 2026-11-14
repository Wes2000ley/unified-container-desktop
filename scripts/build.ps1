param(
    [string]$Node = (Get-Command node.exe -ErrorAction Stop).Source,
    [string]$NpmCli = (Join-Path (Split-Path -Parent (Get-Command npm.cmd -ErrorAction Stop).Source) 'node_modules\npm\bin\npm-cli.js')
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$oldPath = $env:PATH
$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
$oldCgo = $env:CGO_ENABLED
Push-Location $repo
try {
    $nodeText = & $Node --version
    if ($LASTEXITCODE -ne 0 -or $nodeText -notmatch '^v(\d+\.\d+\.\d+)' -or [version]$Matches[1] -lt [version]'24.15.0') { throw 'Node.js 24.15.0 or newer is required. Supply its node.exe with -Node.' }
    if (-not (Test-Path -LiteralPath $NpmCli)) { throw "npm CLI not found at $NpmCli. Supply npm-cli.js with -NpmCli." }
    $env:PATH = (Split-Path -Parent $Node) + [IO.Path]::PathSeparator + $oldPath
    Push-Location frontend
    try {
        & $Node $NpmCli ci
        if ($LASTEXITCODE -ne 0) { throw "npm ci failed: $LASTEXITCODE" }
        & $Node $NpmCli run build
        if ($LASTEXITCODE -ne 0) { throw "frontend build failed: $LASTEXITCODE" }
    } finally { Pop-Location }
    New-Item -ItemType Directory -Force -Path 'bin', 'bin\docker-command', 'bin\linux-amd64' | Out-Null
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    & go build -trimpath -o bin\controller.exe ./cmd/controller
    if ($LASTEXITCODE -ne 0) { throw "controller build failed: $LASTEXITCODE" }
    & go build -trimpath -tags production -ldflags '-H=windowsgui' -o bin\desktop.exe ./cmd/desktop
    if ($LASTEXITCODE -ne 0) { throw "desktop build failed: $LASTEXITCODE" }
    & go build -trimpath -o bin\ucd.exe ./cmd/ucd
    if ($LASTEXITCODE -ne 0) { throw "CLI build failed: $LASTEXITCODE" }
    Copy-Item -LiteralPath bin\ucd.exe -Destination bin\docker-command\docker.exe -Force
    & go build -trimpath -o bin\docker-credential-ucd.exe ./cmd/docker-credential-ucd
    if ($LASTEXITCODE -ne 0) { throw "Docker credential helper build failed: $LASTEXITCODE" }
    & go build -trimpath -o bin\ucd-filetool.exe ./cmd/filetool
    if ($LASTEXITCODE -ne 0) { throw "Windows file tool build failed: $LASTEXITCODE" }
    $env:GOOS = 'linux'
    & go build -trimpath -o bin\linux-amd64\ucd-helper ./cmd/helper
    if ($LASTEXITCODE -ne 0) { throw "Linux helper build failed: $LASTEXITCODE" }
    & go build -trimpath -o bin\linux-amd64\ucd-filetool ./cmd/filetool
    if ($LASTEXITCODE -ne 0) { throw "Linux file tool build failed: $LASTEXITCODE" }
} finally {
    $env:PATH = $oldPath
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
    $env:CGO_ENABLED = $oldCgo
    Pop-Location
}
