# Pure prerequisite behavior tests: every host-mutating command is replaced.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\Setup.ps1')
$script:checks = 0
function Assert-Equal($Expected, $Actual, [string]$Name) {
    if ($Expected -ne $Actual) { throw "$Name`: expected [$Expected], got [$Actual]" }
    $script:checks++
}
function Assert-Throws([scriptblock]$Action, [string]$Message) {
    try { & $Action } catch {
        if ($_.Exception.Message -notlike "*$Message*") { throw }
        $script:checks++
        return
    }
    throw "Expected failure containing: $Message"
}
function Get-CimInstance([string]$ClassName) {
    switch ($ClassName) {
        Win32_OperatingSystem { return [pscustomobject]@{ ProductType = 1; BuildNumber = $script:build; OperatingSystemSKU = 48 } }
        Win32_ComputerSystem { return [pscustomobject]@{ HypervisorPresent = $script:hypervisor } }
        Win32_Processor { return [pscustomobject]@{ VirtualizationFirmwareEnabled = $script:firmware; SecondLevelAddressTranslationExtensions = $true } }
        default { throw "Unexpected CIM query $ClassName" }
    }
}
function Assert-UcdDockerDesktopAbsent { if ($script:desktop) { throw 'Uninstall Docker Desktop first.' } }
function Assert-UcdAdministrator { $script:adminChecks++ }
function Get-UcdFeatureState { foreach ($state in $script:states.GetEnumerator()) { [pscustomobject]@{ FeatureName = $state.Key; State = $state.Value } } }
function Enable-WindowsOptionalFeature {
    param([switch]$Online, $FeatureName, [switch]$All, [switch]$NoRestart)
    if (-not $All -or -not $NoRestart) { throw 'Feature enable must include All and NoRestart.' }
    $script:enabled.Add($FeatureName)
    if ($script:featureError) { throw 'DISM test failure' }
    [pscustomobject]@{ RestartNeeded = $script:featureRestart }
}
function Get-UcdWslVersion { return $script:wsl }
function Install-UcdWsl {
    $script:wslInstalls++
    if ($script:wslError) { throw 'WSL MSI test failure' }
    $script:wsl = $script:installedWsl
    return $script:wslCode
}
$script:build = 22631
$script:hypervisor = $true
$script:firmware = $true
$script:desktop = $false
$script:adminChecks = 0
$script:states = @{ Containers = 'Enabled'; VirtualMachinePlatform = 'Enabled' }
$script:enabled = [Collections.Generic.List[string]]::new()
$script:featureRestart = $true
$script:featureError = $false
$script:wsl = [version]'3.1.0'
$script:wslInstalls = 0
$script:wslError = $false
$script:installedWsl = [version]'3.0.5'
$script:wslCode = 0

Assert-Equal 0 (Invoke-UcdPrerequisites Check) 'Read-only check'
Assert-Equal 0 $script:adminChecks 'Check does not demand elevation'
Assert-Equal 0 (Invoke-UcdPrerequisites Features) 'Already enabled features'
Assert-Equal 0 $script:enabled.Count 'Enabled features are unchanged'
$script:states = @{ Containers = 'Disabled'; VirtualMachinePlatform = 'EnablePending' }
Assert-Equal 3010 (Invoke-UcdPrerequisites Features) 'Restart boundary'
Assert-Equal 'Containers' ($script:enabled -join ',') 'Only disabled features enabled'
$script:states = @{ Containers = 'DisablePending' }
Assert-Throws { Invoke-UcdPrerequisites Features } 'pending removal'
$script:states = @{ Containers = 'Disabled' }
$script:featureError = $true
Assert-Throws { Invoke-UcdPrerequisites Features } 'DISM test failure'
$script:featureError = $false
Assert-Equal 0 (Invoke-UcdPrerequisites Wsl) 'Newer WSL preserved'
Assert-Equal 0 $script:wslInstalls 'No downgrade or unnecessary update'
$script:wsl = $null
Assert-Equal 0 (Invoke-UcdPrerequisites Wsl) 'Missing WSL installed'
Assert-Equal 1 $script:wslInstalls 'One WSL install'
$script:wsl = [version]'2.4.0'
$script:wslCode = 3010
Assert-Equal 3010 (Invoke-UcdPrerequisites Wsl) 'WSL restart boundary'
$script:wsl = $null
$script:wslCode = 0
$script:installedWsl = $null
Assert-Throws { Invoke-UcdPrerequisites Wsl } 'unavailable'
$script:wslError = $true
Assert-Throws { Invoke-UcdPrerequisites Wsl } 'WSL MSI test failure'
$script:desktop = $true
Assert-Throws { Invoke-UcdPrerequisites Features } 'Uninstall Docker Desktop'
$script:desktop = $false
$script:build = 19045
Assert-Throws { Invoke-UcdPrerequisites Features } 'Windows 11'
$script:build = 22631
$script:hypervisor = $false
$script:firmware = $false
Assert-Throws { Invoke-UcdPrerequisites Features } 'BIOS/UEFI'
Write-Host "$script:checks setup prerequisite checks passed. No host changes were made."
