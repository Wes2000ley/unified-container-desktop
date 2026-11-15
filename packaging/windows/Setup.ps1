param([ValidateSet('Check', 'Features', 'Wsl')][string]$Mode = 'Check')
$ErrorActionPreference = 'Stop'

# Internal Burn prerequisite payload. The public entry point is Setup.exe.
Import-Module (Join-Path $PSScriptRoot 'docker-desktop-guard.psm1') -Force

function Assert-UcdPlatform {
    $os = Get-CimInstance Win32_OperatingSystem
    if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -ne 'AMD64' -or $os.ProductType -ne 1 -or [int]$os.BuildNumber -lt 22621 -or $os.OperatingSystemSKU -notin @(4,27,48,49,72,125,126,129,130,161,162,164,165,175)) {
        throw 'Windows 11 Pro or Enterprise x64 build 22621 or later is required.'
    }
    $computer = Get-CimInstance Win32_ComputerSystem
    if (-not $computer.HypervisorPresent) {
        $processors = @(Get-CimInstance Win32_Processor)
        if (-not $processors.Count -or @($processors | Where-Object { -not $_.VirtualizationFirmwareEnabled -or -not $_.SecondLevelAddressTranslationExtensions }).Count) {
            throw 'Enable Intel VT-x / AMD-V virtualization in BIOS/UEFI before installing UCD. A VM also requires nested virtualization.'
        }
    }
    Assert-UcdDockerDesktopAbsent
}

function Get-UcdFeatureState {
    foreach ($feature in @('Containers', 'Microsoft-Hyper-V-All', 'Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform')) {
        Get-WindowsOptionalFeature -Online -FeatureName $feature
    }
}

function Get-UcdWslVersion {
    # Missing/disabled WSL is expected before prerequisites are installed.
    $ErrorActionPreference = 'Continue'
    $value = (& "$env:SystemRoot\System32\wsl.exe" --version 2>&1 | Out-String) -replace "`0", ''
    if ($LASTEXITCODE -ne 0 -or $value -notmatch '(\d+\.\d+\.\d+)') { return $null }
    return [version]$Matches[1]
}

function Assert-UcdAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    try {
        $principal = New-Object Security.Principal.WindowsPrincipal $identity
        if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Setup must be allowed to make administrator changes. Run Setup.exe and approve its Windows prompt.' }
    } finally { $identity.Dispose() }
}

function Install-UcdWsl {
    $msi = Join-Path $PSScriptRoot 'wsl.3.0.5.0.x64.msi'
    if ((Get-FileHash -LiteralPath $msi -Algorithm SHA256).Hash -ne '2313EFC5873E3F8552F42E77F950FABD913C0289078FA89D2100F3421A525EAA') { throw 'The bundled WSL package failed SHA-256 verification.' }
    $signature = Get-AuthenticodeSignature -LiteralPath $msi
    if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch '(^|,\s*)O=Microsoft Corporation(,|$)') { throw 'The WSL MSI does not have a valid Microsoft signature.' }
    $log = Join-Path ([IO.Path]::GetTempPath()) ('Ucd-WSL-' + [guid]::NewGuid().ToString('N') + '.log')
    Write-Host "Installing Microsoft WSL. Windows Installer log: $log"
    $process = Start-Process -FilePath "$env:SystemRoot\System32\msiexec.exe" -ArgumentList @('/i', ('"' + $msi + '"'), '/qn', '/norestart', '/L*v', ('"' + $log + '"')) -WindowStyle Hidden -Wait -PassThru
    if ($process.ExitCode -notin @(0, 3010)) { throw "WSL installation failed with exit code $($process.ExitCode). See $log" }
    return $process.ExitCode
}

function Invoke-UcdPrerequisites([string]$Action) {
    Assert-UcdPlatform
    if ($Action -eq 'Check') {
        [pscustomobject]@{
            Features = @(Get-UcdFeatureState | Select-Object FeatureName, State)
            WslVersion = [string](Get-UcdWslVersion)
        } | ConvertTo-Json -Depth 4 | Write-Host
        return 0
    }
    Assert-UcdAdministrator
    if ($Action -eq 'Features') {
        $states = @(Get-UcdFeatureState)
        if (@($states | Where-Object { $_.State -eq 'DisablePending' }).Count) { throw 'A required Windows feature is pending removal. Restart Windows, then run Setup.exe again.' }
        $restart = $false
        foreach ($state in $states) {
            if ($state.State -eq 'EnablePending') { $restart = $true; continue }
            if ($state.State -ne 'Enabled') {
                Write-Host "Enabling Windows feature: $($state.FeatureName)"
                $result = Enable-WindowsOptionalFeature -Online -FeatureName $state.FeatureName -All -NoRestart
                if ($result.RestartNeeded) { $restart = $true }
            }
        }
        if ($restart) { Write-Host 'Windows features need a restart. Setup will resume after restart; choose Restart or Close in Setup.'; return 3010 }
        return 0
    }
    $version = Get-UcdWslVersion
    if ($version -and $version -ge [version]'2.5.4') {
        Write-Host "Using installed WSL $version. No WSL update is necessary."
        return 0
    }
    $code = Install-UcdWsl
    if ($code -eq 3010) { Write-Host 'WSL needs a restart. Setup will resume afterward.'; return 3010 }
    $version = Get-UcdWslVersion
    if (-not $version -or $version -lt [version]'2.5.4') { throw 'WSL installation returned success, but WSL 2.5.4 or later is unavailable. See the WSL installer log above, restart Windows, then retry Setup.exe.' }
    return 0
}

if ($MyInvocation.InvocationName -ne '.') {
    try { exit (Invoke-UcdPrerequisites $Mode) }
    catch { Write-Error -Message ($_.Exception.Message + [Environment]::NewLine + $_.ScriptStackTrace) -ErrorAction Continue; exit 1603 }
}
