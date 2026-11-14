function Get-UcdDockerDesktopUninstallDisplayNames {
    $displayNames = [System.Collections.Generic.List[string]]::new()
    $views = @([Microsoft.Win32.RegistryView]::Registry64, [Microsoft.Win32.RegistryView]::Registry32)

    foreach ($view in $views) {
        foreach ($hive in @([Microsoft.Win32.RegistryHive]::LocalMachine, [Microsoft.Win32.RegistryHive]::CurrentUser)) {
            $baseKey = [Microsoft.Win32.RegistryKey]::OpenBaseKey($hive, $view)
            try {
                Add-UcdDockerDesktopUninstallEntries $baseKey 'SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall' $displayNames
            } finally {
                $baseKey.Dispose()
            }
        }

        $usersKey = [Microsoft.Win32.RegistryKey]::OpenBaseKey([Microsoft.Win32.RegistryHive]::Users, $view)
        try {
            foreach ($sid in $usersKey.GetSubKeyNames() | Where-Object { $_ -match '^S-1-5-21-(\d+-){3}\d+$' }) {
                Add-UcdDockerDesktopUninstallEntries $usersKey "$sid\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall" $displayNames
            }
        } finally {
            $usersKey.Dispose()
        }
    }

    return $displayNames.ToArray()
}

function Add-UcdDockerDesktopUninstallEntries {
    param(
        [Parameter(Mandatory)][Microsoft.Win32.RegistryKey]$BaseKey,
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][AllowEmptyCollection()][System.Collections.Generic.List[string]]$DisplayNames
    )

    $uninstallKey = $BaseKey.OpenSubKey($Path)
    if ($null -eq $uninstallKey) { return }

    try {
        foreach ($name in $uninstallKey.GetSubKeyNames()) {
            $entry = $uninstallKey.OpenSubKey($name)
            if ($null -eq $entry) { continue }
            try {
                $displayName = [string]$entry.GetValue('DisplayName', '')
                if ([string]::Equals($displayName, 'Docker Desktop', [StringComparison]::OrdinalIgnoreCase)) {
                    $DisplayNames.Add($displayName)
                }
            } finally {
                $entry.Dispose()
            }
        }
    } finally {
        $uninstallKey.Dispose()
    }
}

function Test-UcdDockerDesktopConflict {
    param(
        [string[]]$UninstallDisplayNames = (Get-UcdDockerDesktopUninstallDisplayNames),
        [string[]]$RunningProcessNames = @((Get-Process -Name 'Docker Desktop', 'com.docker.backend' -ErrorAction SilentlyContinue).ProcessName)
    )

    $installed = @($UninstallDisplayNames | Where-Object { [string]::Equals($_, 'Docker Desktop', [StringComparison]::OrdinalIgnoreCase) }).Count -gt 0
    $running = @($RunningProcessNames | Where-Object { $_ -in @('Docker Desktop', 'com.docker.backend') }).Count -gt 0
    return $installed -or $running
}

function Assert-UcdDockerDesktopAbsent {
    param(
        [string[]]$UninstallDisplayNames = (Get-UcdDockerDesktopUninstallDisplayNames),
        [string[]]$RunningProcessNames = @((Get-Process -Name 'Docker Desktop', 'com.docker.backend' -ErrorAction SilentlyContinue).ProcessName)
    )

    if (Test-UcdDockerDesktopConflict -UninstallDisplayNames $UninstallDisplayNames -RunningProcessNames $RunningProcessNames) {
        throw 'Docker Desktop is installed or running. Uninstall Docker Desktop and close its processes before installing Unified Container Desktop; both products use the system-wide docker command and cannot share this PC.'
    }
}

Export-ModuleMember -Function Get-UcdDockerDesktopUninstallDisplayNames, Test-UcdDockerDesktopConflict, Assert-UcdDockerDesktopAbsent
