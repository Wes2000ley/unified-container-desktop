param(
    [Parameter(Mandatory = $true)][string]$Output,
    [Parameter(Mandatory = $true)][string]$DockerConfig,
    [string]$DockerHost = 'npipe:////./pipe/docker_engine'
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$linux = Join-Path $repo 'packaging\linux'
$tag = 'ucd-rootfs-build:' + [guid]::NewGuid().ToString('N')
$container = $null
$built = $false
function Invoke-Docker {
    & docker --config $DockerConfig --host $DockerHost @args
    if ($LASTEXITCODE -ne 0) { throw "docker $($args[0]) failed with exit code $LASTEXITCODE" }
}
try {
    Invoke-Docker build --platform linux/amd64 --file (Join-Path $linux 'Dockerfile.rootfs') --tag $tag $linux
    $built = $true
    $container = Invoke-Docker create $tag
    if (-not $container -or $container -notmatch '^[0-9a-f]{64}$') { throw 'docker create did not return a container ID' }
    New-Item -ItemType Directory -Force (Split-Path -Parent ([IO.Path]::GetFullPath($Output))) | Out-Null
    Invoke-Docker export --output $Output $container
    $hash = (Get-FileHash -Algorithm SHA256 $Output).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$Output.sha256", "$hash  $([IO.Path]::GetFileName($Output))`n", [Text.Encoding]::ASCII)
    Write-Host "Rootfs SHA256: $hash"
} finally {
    if ($container) { & docker --config $DockerConfig --host $DockerHost rm $container | Out-Null }
    if ($built) { & docker --config $DockerConfig --host $DockerHost image rm $tag | Out-Null }
}
