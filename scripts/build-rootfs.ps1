param(
    [Parameter(Mandatory = $true)][string]$Output,
    [Parameter(Mandatory = $true)][string]$DockerConfig,
    [string]$DockerHost = ''
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$linux = Join-Path $repo 'packaging\linux'
$tag = 'ucd-rootfs-build:' + [guid]::NewGuid().ToString('N')
$container = $null
$built = $false
$sourceContainer = $null
$sourceTag = "$tag-sources"
$sourceBuilt = $false
$dockerArguments = @('--config', $DockerConfig)
if ($DockerHost) { $dockerArguments += @('--host', $DockerHost) }
function Invoke-Docker {
    & docker @dockerArguments @args
    if ($LASTEXITCODE -ne 0) { throw "docker $($args[0]) failed with exit code $LASTEXITCODE" }
}
try {
    Invoke-Docker build --platform linux/amd64 --target runtime --file (Join-Path $linux 'Dockerfile.rootfs') --tag $tag $linux
    $built = $true
    $container = Invoke-Docker create $tag
    if (-not $container -or $container -notmatch '^[0-9a-f]{64}$') { throw 'docker create did not return a container ID' }
    New-Item -ItemType Directory -Force (Split-Path -Parent ([IO.Path]::GetFullPath($Output))) | Out-Null
    Invoke-Docker export --output $Output $container
    $hash = (Get-FileHash -Algorithm SHA256 $Output).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$Output.sha256", "$hash  $([IO.Path]::GetFileName($Output))`n", [Text.Encoding]::ASCII)
    Write-Host "Rootfs SHA256: $hash"
    Invoke-Docker build --platform linux/amd64 --target sources --file (Join-Path $linux 'Dockerfile.rootfs') --tag $sourceTag $linux
    $sourceBuilt = $true
    $sourceContainer = Invoke-Docker create $sourceTag
    if (-not $sourceContainer -or $sourceContainer -notmatch '^[0-9a-f]{64}$') { throw 'Source container creation did not return an ID' }
    $archive = "$Output.sources.tar.gz"
    Invoke-Docker cp "${sourceContainer}:/ucd-rootfs-sources.tar.gz" $archive
    $sourceHash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$Output.sources.json", (@{ rootfsSha256 = $hash; sourcesSha256 = $sourceHash } | ConvertTo-Json), [Text.Encoding]::ASCII)
    Write-Host "Corresponding sources SHA256: $sourceHash"
} finally {
    if ($sourceContainer) { & docker @dockerArguments rm $sourceContainer | Out-Null }
    if ($sourceBuilt) { & docker @dockerArguments image rm $sourceTag | Out-Null }
    if ($container) { & docker @dockerArguments rm $container | Out-Null }
    if ($built) { & docker @dockerArguments image rm $tag | Out-Null }
}
