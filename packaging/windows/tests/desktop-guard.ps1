$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot '..\docker-desktop-guard.psm1') -Force

Get-UcdDockerDesktopUninstallDisplayNames | Out-Null

$falsePositiveCases = @(
    @{ UninstallDisplayNames = @('Unified Container Desktop'); RunningProcessNames = @() },
    @{ UninstallDisplayNames = @(); RunningProcessNames = @('ucd', 'docker', 'docker-cli') }
)
foreach ($case in $falsePositiveCases) {
    if (Test-UcdDockerDesktopConflict @case) { throw 'A UCD or Docker CLI name caused a false Docker Desktop conflict.' }
}

$conflictCases = @(
    @{ UninstallDisplayNames = @('Docker Desktop'); RunningProcessNames = @() },
    @{ UninstallDisplayNames = @('dOcKeR dEsKtOp'); RunningProcessNames = @() },
    @{ UninstallDisplayNames = @(); RunningProcessNames = @('Docker Desktop') },
    @{ UninstallDisplayNames = @(); RunningProcessNames = @('com.docker.backend') }
)
foreach ($case in $conflictCases) {
    if (-not (Test-UcdDockerDesktopConflict @case)) { throw 'A Docker Desktop install or process was not detected.' }
}

$rejected = $false
try {
    Assert-UcdDockerDesktopAbsent -UninstallDisplayNames @('Docker Desktop') -RunningProcessNames @()
} catch {
    if ($_.Exception.Message -notmatch 'Uninstall Docker Desktop') { throw }
    $rejected = $true
}
if (-not $rejected) { throw 'The installer guard did not reject Docker Desktop.' }

Write-Output 'Docker Desktop exclusion checks passed.'
