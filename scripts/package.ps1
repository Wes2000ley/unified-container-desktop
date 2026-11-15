param(
    [Parameter(Mandatory = $true)][string]$Rootfs,
    [Parameter(Mandatory = $true)][string]$RootfsSources,
    [Parameter(Mandatory = $true)][string]$CacheDirectory,
    [Parameter(Mandatory = $true)][string]$Wix,
    [string]$OutputDirectory = (Join-Path (Split-Path -Parent $PSScriptRoot) 'dist')
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$repo = Split-Path -Parent $PSScriptRoot
$apiSource = Get-Content -Raw (Join-Path $repo 'internal\api\api.go')
if ($apiSource -notmatch '(?m)^\s*Version\s*=\s*"(\d+)\.(\d+)\.(\d+)"') { throw 'internal/api/api.go must define a three-part numeric Version.' }
$version = "$($Matches[1]).$($Matches[2]).$($Matches[3])"
if ([int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) { throw "Version $version exceeds Windows Installer version limits." }
if ($env:GITHUB_REF_TYPE -eq 'tag' -and $env:GITHUB_REF_NAME -ne "v$version") { throw "Release tag $env:GITHUB_REF_NAME does not match application version v$version." }
$cache = [IO.Path]::GetFullPath($CacheDirectory)
$output = [IO.Path]::GetFullPath($OutputDirectory)
$stage = Join-Path $cache 'msi-payload'
New-Item -ItemType Directory -Force -Path $cache, $stage, $output | Out-Null
function Get-Verified([string]$Url, [string]$File, [string]$Hash) {
    if (-not (Test-Path -LiteralPath $File)) { Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $File }
    if ((Get-FileHash -LiteralPath $File -Algorithm SHA256).Hash -ne $Hash) { throw "SHA-256 mismatch for $File; remove it and obtain the pinned release again." }
}
$dockerZip = Join-Path $cache 'docker-29.9.3.zip'
Get-Verified 'https://download.docker.com/win/static/stable/x86_64/docker-29.9.3.zip' $dockerZip '7C4FF7BBB31579046CD34DD8B91F7CBD3AB3482A1A85330170BE726EF792D751'
$compose = Join-Path $cache 'docker-compose-5.7.1-windows-amd64.exe'
Get-Verified 'https://github.com/docker/compose/releases/download/v5.7.1/docker-compose-windows-x86_64.exe' $compose 'E4316597452650AE92FE9719A2A7C2FCE8F56C5CEAFED88D2AED6E39B2B2B2D0'
$buildx = Join-Path $cache 'docker-buildx-0.38.2-windows-amd64.exe'
Get-Verified 'https://github.com/docker/buildx/releases/download/v0.38.2/buildx-v0.38.2.windows-amd64.exe' $buildx '9A6ACA9C84ADDA0391D1D98D8B74B49359CAD1BCDADD196A0A5D7D5DC4CF15E0'
$linuxBuildx = Join-Path $cache 'docker-buildx-0.39.0-linux-amd64'
Get-Verified 'https://github.com/docker/buildx/releases/download/v0.39.0/buildx-v0.39.0.linux-amd64' $linuxBuildx '5FB31A097304FBF6DD4038649A0DECF32C2B9510E4047ACD7D68B9D700C46194'
$wsl = Join-Path $cache 'wsl.3.0.5.0.x64.msi'
Get-Verified 'https://github.com/microsoft/WSL/releases/download/3.0.5/wsl.3.0.5.0.x64.msi' $wsl '2313EFC5873E3F8552F42E77F950FABD913C0289078FA89D2100F3421A525EAA'
$signature = Get-AuthenticodeSignature -LiteralPath $wsl
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch '(^|,\s*)O=Microsoft Corporation(,|$)') { throw 'WSL package Microsoft signature verification failed.' }
$wixSource = Join-Path $cache 'wix-v4.0.6-source.tar.gz'
Get-Verified 'https://codeload.github.com/wixtoolset/wix/tar.gz/refs/tags/v4.0.6' $wixSource '6374C3112890DC310723B95B170866EECAC255E55678E5A6077FE3DA35E4880B'
$checksum = "$Rootfs.sha256"
if (-not (Test-Path -LiteralPath $checksum)) { throw "Rootfs checksum not found: $checksum" }
$expected = (Get-Content -Raw -LiteralPath $checksum).Split(' ', [StringSplitOptions]::RemoveEmptyEntries)[0]
if ($expected -notmatch '^[a-fA-F0-9]{64}$' -or (Get-FileHash -LiteralPath $Rootfs -Algorithm SHA256).Hash -ne $expected) { throw 'Rootfs SHA-256 verification failed.' }
if (-not $RootfsSources.EndsWith('.tar.gz')) { throw 'RootfsSources must name the .sources.tar.gz archive produced with the rootfs.' }
$sourceManifestPath = $RootfsSources -replace '\.tar\.gz$', '.json'
$sourceManifest = Get-Content -Raw -LiteralPath $sourceManifestPath | ConvertFrom-Json
$sourceHash = (Get-FileHash -LiteralPath $RootfsSources -Algorithm SHA256).Hash.ToLowerInvariant()
if ($sourceManifest.rootfsSha256 -ne $expected -or $sourceManifest.sourcesSha256 -ne $sourceHash) { throw 'Corresponding-source hashes do not match this rootfs and source archive.' }
Expand-Archive -LiteralPath $dockerZip -DestinationPath (Join-Path $cache 'docker-extracted') -Force
Copy-Item -Force (Join-Path $cache 'docker-extracted\docker\dockerd.exe') (Join-Path $stage 'dockerd.exe')
Copy-Item -Force (Join-Path $cache 'docker-extracted\docker\docker.exe') (Join-Path $stage 'docker-cli.exe')
Copy-Item -Force $compose (Join-Path $stage 'docker-compose.exe')
Copy-Item -Force $buildx (Join-Path $stage 'docker-buildx.exe')
Copy-Item -Force $linuxBuildx (Join-Path $stage 'docker-buildx')
foreach ($file in @('controller.exe', 'desktop.exe', 'ucd.exe', 'ucd-filetool.exe', 'docker-credential-ucd.exe')) {
    $source = Join-Path $repo "bin\$file"
    if (-not (Test-Path -LiteralPath $source)) { throw "Build artifact missing: $source" }
    Copy-Item -Force $source (Join-Path $stage $file)
}
Copy-Item -LiteralPath (Join-Path $repo 'bin\docker-command\docker.exe') -Destination (Join-Path $stage 'docker.exe') -Force
Copy-Item -Force (Join-Path $repo 'packaging\windows\docker-desktop-guard.psm1') (Join-Path $stage 'docker-desktop-guard.psm1')
$helper = Join-Path $repo 'bin\linux-amd64\ucd-helper'
if (-not (Test-Path -LiteralPath $helper)) { throw "Build artifact missing: $helper" }
Copy-Item -Force $helper (Join-Path $stage 'ucd-helper')
$linuxFileTool = Join-Path $repo 'bin\linux-amd64\ucd-filetool'
if (-not (Test-Path -LiteralPath $linuxFileTool)) { throw "Build artifact missing: $linuxFileTool" }
Copy-Item -Force $linuxFileTool (Join-Path $stage 'ucd-filetool')
Copy-Item -Force $Rootfs (Join-Path $stage 'debian-rootfs.tar')
[IO.File]::WriteAllText((Join-Path $stage 'debian-rootfs.tar.sha256'), "$($expected.ToLowerInvariant())  debian-rootfs.tar`n", [Text.Encoding]::ASCII)
$msi = Join-Path $output 'UnifiedContainerDesktop.msi'
& $Wix build -arch x64 -d "PayloadDir=$stage" -d "SourceDir=$repo" -d "ProductVersion=$version" -o $msi (Join-Path $repo 'packaging\windows\installer.wxs')
if ($LASTEXITCODE -ne 0) { throw "WiX build failed with exit code $LASTEXITCODE" }
$hash = (Get-FileHash -LiteralPath $msi -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$msi.sha256", "$hash  UnifiedContainerDesktop.msi`n", [Text.Encoding]::ASCII)
$setup = Join-Path $output 'UnifiedContainerDesktop-Setup.exe'
if (-not (Test-Path -LiteralPath (Join-Path $repo 'bin\ucd-prerequisites.exe'))) { throw 'Build artifact missing: bin\ucd-prerequisites.exe. Run scripts/build.ps1 first.' }
Push-Location $repo
try {
    & $Wix extension add WixToolset.Bal.wixext/4.0.6
    if ($LASTEXITCODE -ne 0) { throw "WiX bootstrapper extension installation failed: $LASTEXITCODE" }
    & $Wix build -arch x64 -ext WixToolset.Bal.wixext/4.0.6 -d "SourceDir=$repo" -d "ProductVersion=$version" -d "ProductMsi=$msi" -d "WslMsi=$wsl" -o $setup (Join-Path $repo 'packaging\windows\bundle.wxs')
    if ($LASTEXITCODE -ne 0) { throw "Setup executable build failed: $LASTEXITCODE" }
} finally { Pop-Location }
$setupHash = (Get-FileHash -LiteralPath $setup -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$setup.sha256", "$setupHash  UnifiedContainerDesktop-Setup.exe`n", [Text.Encoding]::ASCII)
$releaseSources = Join-Path $output 'UnifiedContainerDesktop-rootfs-sources.tar.gz'
Copy-Item -LiteralPath $RootfsSources -Destination $releaseSources -Force
[IO.File]::WriteAllText("$releaseSources.sha256", "$sourceHash  UnifiedContainerDesktop-rootfs-sources.tar.gz`n", [Text.Encoding]::ASCII)
Copy-Item -LiteralPath $sourceManifestPath -Destination (Join-Path $output 'rootfs-sources.json') -Force
Copy-Item -LiteralPath $wixSource -Destination (Join-Path $output 'wix-v4.0.6-source.tar.gz') -Force
[IO.File]::WriteAllText((Join-Path $output 'wix-v4.0.6-source.tar.gz.sha256'), "6374c3112890dc310723b95b170866eecac255e55678e5a6077fe3da35e4880b  wix-v4.0.6-source.tar.gz`n", [Text.Encoding]::ASCII)
Copy-Item -Force (Join-Path $repo 'packaging\README.md') (Join-Path $output 'INSTALL.md')
foreach ($file in @('ACCEPTANCE.md', 'VALIDATION.md', 'LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.txt')) {
    Copy-Item -Force (Join-Path $repo $file) (Join-Path $output $file)
}
Write-Host "MSI SHA256: $hash"
Write-Host "Setup SHA256: $setupHash"
