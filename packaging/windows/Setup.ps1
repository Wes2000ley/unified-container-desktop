#Requires -RunAsAdministrator
param()
$ErrorActionPreference = 'Stop'
$wslMsi = Join-Path $PSScriptRoot 'wsl.3.0.5.0.x64.msi'
$productMsi = Join-Path $PSScriptRoot 'UnifiedContainerDesktop.msi'
$expectedWsl = '2313EFC5873E3F8552F42E77F950FABD913C0289078FA89D2100F3421A525EAA'

Import-Module (Join-Path $PSScriptRoot 'docker-desktop-guard.psm1') -Force
Assert-UcdDockerDesktopAbsent

function Test-Hash([string]$Path, [string]$Expected) {
    if (-not (Test-Path -LiteralPath $Path)) { throw "Required release file is missing: $Path" }
    if ((Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -ne $Expected) { throw "SHA-256 verification failed: $Path" }
}
function Invoke-Msi([string]$Path) {
    $log = Join-Path ([IO.Path]::GetTempPath()) ('Ucd-' + [IO.Path]::GetFileNameWithoutExtension($Path) + '-' + [DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss') + '.log')
    Write-Host "Installing $([IO.Path]::GetFileName($Path)). Installer log: $log"
    $process = Start-Process -FilePath "$env:SystemRoot\System32\msiexec.exe" -ArgumentList @('/i', ('"' + $Path + '"'), '/qn', '/norestart', '/L*v', ('"' + $log + '"')) -WindowStyle Hidden -Wait -PassThru
    if ($process.ExitCode -notin @(0, 3010)) { throw "Installer failed with exit code $($process.ExitCode): $Path. See $log and $env:SystemRoot\Temp\Ucd-provision-*.log" }
    return $process.ExitCode
}
function Get-WslVersion {
    # Windows PowerShell 5 turns redirected native stderr into an error record.
    # An absent or disabled WSL installation is an expected prerequisite state.
    $ErrorActionPreference = 'Continue'
    $value = (& "$env:SystemRoot\System32\wsl.exe" --version 2>&1 | Out-String) -replace "`0", ''
    if ($LASTEXITCODE -ne 0 -or $value -notmatch '(\d+\.\d+\.\d+)') { return $null }
    return [version]$Matches[1]
}

$os = Get-CimInstance Win32_OperatingSystem
if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -ne 'AMD64' -or $os.ProductType -ne 1 -or [int]$os.BuildNumber -lt 22621 -or $os.OperatingSystemSKU -notin @(4,27,48,49,72,125,126,129,130,161,162,164,165,175)) { throw 'Windows 11 Pro or Enterprise x64 build 22621 or later is required.' }
$hashFile = "$productMsi.sha256"
if (-not (Test-Path -LiteralPath $hashFile)) { throw "Release checksum is missing: $hashFile" }
$productHash = (Get-Content -Raw -LiteralPath $hashFile).Split(' ', [StringSplitOptions]::RemoveEmptyEntries)[0]
if ($productHash -notmatch '^[a-fA-F0-9]{64}$') { throw 'The product MSI checksum is invalid.' }
Test-Hash $productMsi $productHash
Test-Hash $wslMsi $expectedWsl
$signature = Get-AuthenticodeSignature -LiteralPath $wslMsi
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch '(^|,\s*)O=Microsoft Corporation(,|$)') { throw 'The WSL MSI does not have a valid Microsoft signature.' }

$restart = $false
foreach ($feature in @('Containers', 'Microsoft-Hyper-V-All', 'Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform')) {
    $state = Get-WindowsOptionalFeature -Online -FeatureName $feature
    if ($state.State -ne 'Enabled') {
        $result = Enable-WindowsOptionalFeature -Online -FeatureName $feature -All -NoRestart
        if ($result.RestartNeeded) { $restart = $true }
    }
}
if ($restart) { Write-Host 'Windows features were enabled. Restart Windows, then run Setup.ps1 again.'; exit 3010 }

$wslVersion = Get-WslVersion
if (-not $wslVersion -or $wslVersion -lt [version]'2.5.4') {
    $code = Invoke-Msi $wslMsi
    if ($code -eq 3010) { Write-Host 'WSL was installed. Restart Windows, then run Setup.ps1 again.'; exit 3010 }
    $wslVersion = Get-WslVersion
    if (-not $wslVersion -or $wslVersion -lt [version]'2.5.4') { Write-Host 'WSL was installed. Restart Windows, then run Setup.ps1 again.'; exit 3010 }
}
$code = Invoke-Msi $productMsi
if ($code -eq 3010) { Write-Host 'Installation completed. Restart Windows to finish enabling the services.'; exit 3010 }
Write-Host 'Unified Container Desktop installation completed.'
