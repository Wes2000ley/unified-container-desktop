param([ValidateSet('install', 'rollback', 'restore-services')][string]$Action = 'install', [switch]$RemoveServices)
$ErrorActionPreference = 'Stop'
trap {
    $failure = $_.Exception.ToString() + [Environment]::NewLine + $_.InvocationInfo.PositionMessage + [Environment]::NewLine + $_.ScriptStackTrace
    $log = Join-Path $env:SystemRoot ('Temp\Ucd-provision-' + $Action + '-' + [guid]::NewGuid().ToString('N') + '.log')
    try { [IO.File]::WriteAllText($log, $failure, (New-Object Text.UTF8Encoding $false)) }
    catch { Write-Error -Message ('Could not write provisioning failure log: ' + $_.Exception.Message) -ErrorAction Continue }
    Write-Error -Message ($failure + [Environment]::NewLine + 'Provisioning log: ' + $log) -ErrorAction Continue
    exit 1
}

if ($Action -eq 'install') {
    Import-Module (Join-Path $env:ProgramFiles 'UnifiedContainerDesktop\docker-desktop-guard.psm1') -Force
    Assert-UcdDockerDesktopAbsent
}

$data = Join-Path $env:ProgramData 'UnifiedContainerDesktop'
$install = Join-Path $env:ProgramFiles 'UnifiedContainerDesktop'
$account = 'UcdService'
$group = 'Container Operators'

# Password material stays in the process and in the LSA private-data store/SCM.
# It is never written to a file, process argument, or MSI property.
Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Security.AccessControl;
using System.Security.Principal;
using System.Text;

public static class UcdNative {
    const string SecretName = "UCDServicePassword";
    const uint PolicyAllAccess = 0x000F0FFF;
    [StructLayout(LayoutKind.Sequential)] struct UnicodeString { public ushort Length, MaximumLength; public IntPtr Buffer; }
    [StructLayout(LayoutKind.Sequential)] struct ObjectAttributes { public uint Length; public IntPtr RootDirectory, ObjectName; public uint Attributes; public IntPtr SecurityDescriptor, SecurityQualityOfService; }
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] struct UserInfo1 {
        public string Name, Password; public uint PasswordAge, Privilege; public string HomeDirectory, Comment; public uint Flags; public string ScriptPath;
    }
    [DllImport("netapi32.dll", CharSet=CharSet.Unicode)] static extern int NetUserAdd(string server, int level, ref UserInfo1 info, out uint parameterError);
    [DllImport("netapi32.dll", CharSet=CharSet.Unicode)] static extern int NetUserDel(string server, string user);
    [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr system, ref ObjectAttributes attributes, uint access, out IntPtr policy);
    [DllImport("advapi32.dll")] static extern uint LsaStorePrivateData(IntPtr policy, ref UnicodeString key, ref UnicodeString value);
    [DllImport("advapi32.dll")] static extern uint LsaRetrievePrivateData(IntPtr policy, ref UnicodeString key, out IntPtr value);
    [DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr policy, IntPtr sid, UnicodeString[] rights, int count);
    [DllImport("advapi32.dll")] static extern uint LsaNtStatusToWinError(uint status);
    [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr policy);
    [DllImport("advapi32.dll")] static extern uint LsaFreeMemory(IntPtr memory);
    [DllImport("userenv.dll", CharSet=CharSet.Unicode)] static extern int CreateProfile(string sid, string user, StringBuilder path, uint size);
    [DllImport("advapi32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr OpenSCManager(string machine, string database, uint access);
    [DllImport("advapi32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr OpenService(IntPtr manager, string name, uint access);
    [DllImport("advapi32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern IntPtr CreateService(IntPtr manager, string name, string display, uint access, uint type, uint start, uint error, string path, string group, IntPtr tag, string dependencies, string user, string password);
    [DllImport("advapi32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool ChangeServiceConfig(IntPtr service, uint type, uint start, uint error, string path, string group, IntPtr tag, string dependencies, string user, string password, string display);
    [StructLayout(LayoutKind.Sequential)] struct ServiceAction { public uint Type, Delay; }
    [StructLayout(LayoutKind.Sequential)] struct ServiceFailureActions { public uint ResetPeriod; public IntPtr RebootMessage, Command; public uint Count; public IntPtr Actions; }
    [DllImport("advapi32.dll", CharSet=CharSet.Unicode, SetLastError=true)] static extern bool ChangeServiceConfig2(IntPtr service, uint infoLevel, ref ServiceFailureActions info);
    [DllImport("advapi32.dll")] static extern bool CloseServiceHandle(IntPtr handle);
    [DllImport("advapi32.dll", SetLastError=true)] static extern bool QueryServiceObjectSecurity(IntPtr service, uint information, byte[] descriptor, uint size, out uint needed);
    [DllImport("advapi32.dll", SetLastError=true)] static extern bool SetServiceObjectSecurity(IntPtr service, uint information, byte[] descriptor);

    static UnicodeString MakeUnicode(string value) {
        var s = new UnicodeString();
        s.Length = checked((ushort)(value.Length * 2)); s.MaximumLength = checked((ushort)(s.Length + 2));
        s.Buffer = Marshal.StringToHGlobalUni(value); return s;
    }
    static void Free(ref UnicodeString s) { if (s.Buffer != IntPtr.Zero) { Marshal.ZeroFreeGlobalAllocUnicode(s.Buffer); s.Buffer = IntPtr.Zero; } }
    static void Check(uint status, string action) { if (status != 0) throw new Win32Exception((int)LsaNtStatusToWinError(status), action); }
    static IntPtr Policy() { var a = new ObjectAttributes(); a.Length = (uint)Marshal.SizeOf(a); IntPtr p; Check(LsaOpenPolicy(IntPtr.Zero, ref a, PolicyAllAccess, out p), "Open LSA policy"); return p; }
    static string ReadSecret() {
        IntPtr policy = Policy(), result = IntPtr.Zero; var key = MakeUnicode(SecretName);
        try {
            uint status = LsaRetrievePrivateData(policy, ref key, out result);
            if (status == 0xC0000034) return null;
            Check(status, "Read service account secret");
            var value = (UnicodeString)Marshal.PtrToStructure(result, typeof(UnicodeString));
            return Marshal.PtrToStringUni(value.Buffer, value.Length / 2);
        } finally { if (result != IntPtr.Zero) LsaFreeMemory(result); Free(ref key); LsaClose(policy); }
    }
    static void StoreSecret(string password) {
        IntPtr policy = Policy(); var key = MakeUnicode(SecretName); var value = MakeUnicode(password);
        try { Check(LsaStorePrivateData(policy, ref key, ref value), "Store service account secret"); }
        finally { Free(ref value); Free(ref key); LsaClose(policy); }
    }
    public static string EnsureAccount(bool exists) {
        string password = ReadSecret();
        if (exists && password == null) throw new InvalidOperationException("UcdService already exists but its UCD LSA secret is absent; refuse to take over this account.");
        if (!exists && password != null) throw new InvalidOperationException("UcdService is missing but its UCD LSA secret remains; restore the account or remove the stale secret deliberately.");
        if (!exists) {
            byte[] random = new byte[48]; using (var rng = RandomNumberGenerator.Create()) rng.GetBytes(random);
            password = "UcD4!" + Convert.ToBase64String(random); Array.Clear(random, 0, random.Length);
            var info = new UserInfo1 { Name="UcdService", Password=password, Privilege=1, Flags=0x10241, Comment="Unified Container Desktop service identity" };
            uint bad; int rc = NetUserAdd(null, 1, ref info, out bad);
            if (rc != 0) throw new Win32Exception(rc, "Create UcdService");
            try { StoreSecret(password); } catch { NetUserDel(null, "UcdService"); throw; }
        }
        return ((SecurityIdentifier)new NTAccount(Environment.MachineName, "UcdService").Translate(typeof(SecurityIdentifier))).Value;
    }
    public static void GrantRights(string sidText) {
        var sid = new SecurityIdentifier(sidText); byte[] bytes = new byte[sid.BinaryLength]; sid.GetBinaryForm(bytes, 0);
        IntPtr ptr = Marshal.AllocHGlobal(bytes.Length), policy = Policy(); Marshal.Copy(bytes, 0, ptr, bytes.Length);
        string[] names = { "SeServiceLogonRight", "SeDenyInteractiveLogonRight", "SeDenyRemoteInteractiveLogonRight", "SeBackupPrivilege", "SeRestorePrivilege" };
        var rights = new UnicodeString[names.Length];
        try { for (int i=0;i<names.Length;i++) rights[i] = MakeUnicode(names[i]); Check(LsaAddAccountRights(policy, ptr, rights, rights.Length), "Grant service account rights"); }
        finally { for (int i=0;i<rights.Length;i++) Free(ref rights[i]); Marshal.FreeHGlobal(ptr); LsaClose(policy); }
    }
    public static string NewProfile(string sid) {
        // The profile service RPC accepts a MAX_PATH output buffer.
        var path = new StringBuilder(260); int hr = CreateProfile(sid, "UcdService", path, (uint)path.Capacity);
        if (hr < 0) Marshal.ThrowExceptionForHR(hr); return path.ToString();
    }
    // Add only SERVICE_QUERY_STATUS for authenticated users; preserve all other ACEs.
    public static byte[] AddControllerQueryAce(byte[] descriptor) {
        var sd = new RawSecurityDescriptor(descriptor, 0);
        var acl = sd.DiscretionaryAcl;
        if (acl == null) throw new InvalidOperationException("Controller service has no discretionary ACL");
        var authenticated = new SecurityIdentifier(WellKnownSidType.AuthenticatedUserSid, null);
        int insert = acl.Count;
        for (int i = 0; i < acl.Count; i++) {
            var ace = acl[i] as CommonAce;
            if (ace != null && ace.AceQualifier == AceQualifier.AccessAllowed &&
                (ace.AceFlags & AceFlags.InheritOnly) == 0 && ace.SecurityIdentifier.Equals(authenticated) && (ace.AccessMask & 0x0004) != 0)
                return descriptor;
            if (insert == acl.Count && (acl[i].AceFlags & AceFlags.Inherited) != 0) insert = i;
        }
        acl.InsertAce(insert, new CommonAce(AceFlags.None, AceQualifier.AccessAllowed, 0x0004, authenticated, false, null));
        byte[] updated = new byte[sd.BinaryLength]; sd.GetBinaryForm(updated, 0); return updated;
    }
    public static void EnsureControllerQueryAccess() {
        const uint DaclSecurityInformation = 0x00000004;
        IntPtr manager = OpenSCManager(null, null, 0x0001); if (manager == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error(), "Open SCM");
        IntPtr service = IntPtr.Zero;
        try {
            service = OpenService(manager, "UnifiedContainerDesktop", 0x00060000); // READ_CONTROL | WRITE_DAC
            if (service == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error(), "Open controller service security");
            uint needed;
            if (!QueryServiceObjectSecurity(service, DaclSecurityInformation, null, 0, out needed) && Marshal.GetLastWin32Error() != 122)
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Size controller service DACL");
            byte[] original = new byte[needed];
            if (!QueryServiceObjectSecurity(service, DaclSecurityInformation, original, needed, out needed))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Read controller service DACL");
            byte[] updated = AddControllerQueryAce(original);
            if (!Object.ReferenceEquals(original, updated) && !SetServiceObjectSecurity(service, DaclSecurityInformation, updated))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Update controller service DACL");
        } finally { if (service != IntPtr.Zero) CloseServiceHandle(service); CloseServiceHandle(manager); }
    }
    public static void EnsureService(string name, string display, string path, bool asAccount) {
        IntPtr manager = OpenSCManager(null, null, 0x0003); if (manager == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error(), "Open SCM");
        IntPtr service = IntPtr.Zero;
        try {
            string user = asAccount ? @".\UcdService" : null, password = asAccount ? ReadSecret() : null;
            if (asAccount && password == null) throw new InvalidOperationException("UcdService LSA secret is missing.");
            // SERVICE_START is required to configure a restart recovery action.
            uint access = name == "UCDDocker" ? 0x0012u : 0x0002u;
            service = OpenService(manager, name, access);
            if (service == IntPtr.Zero) {
                int err = Marshal.GetLastWin32Error(); if (err != 1060) throw new Win32Exception(err, "Open " + name);
                service = CreateService(manager, name, display, access, 0x10, 2, 1, path, null, IntPtr.Zero, null, user, password);
                if (service == IntPtr.Zero) throw new Win32Exception(Marshal.GetLastWin32Error(), "Create " + name);
            } else if (!ChangeServiceConfig(service, 0xFFFFFFFF, 2, 0xFFFFFFFF, path, null, IntPtr.Zero, null, user, password, display))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Configure " + name);
            if (name == "UCDDocker") ConfigureDockerRecovery(service);
        } finally { if (service != IntPtr.Zero) CloseServiceHandle(service); CloseServiceHandle(manager); }
    }
    static void ConfigureDockerRecovery(IntPtr service) {
        // Match dockerd --register-service: restart twice, then stop retrying.
        int size = Marshal.SizeOf(typeof(ServiceAction));
        IntPtr actions = Marshal.AllocHGlobal(size * 3);
        try {
            Marshal.StructureToPtr(new ServiceAction { Type = 1, Delay = 15000 }, actions, false);
            Marshal.StructureToPtr(new ServiceAction { Type = 1, Delay = 15000 }, IntPtr.Add(actions, size), false);
            Marshal.StructureToPtr(new ServiceAction { Type = 0, Delay = 0 }, IntPtr.Add(actions, size * 2), false);
            var info = new ServiceFailureActions { ResetPeriod = 86400, Count = 3, Actions = actions };
            if (!ChangeServiceConfig2(service, 2, ref info)) throw new Win32Exception(Marshal.GetLastWin32Error(), "Configure UCDDocker recovery");
        } finally { Marshal.FreeHGlobal(actions); }
    }
}
'@

function Grant-DataAccess([string]$Path, [string]$Sid) {
    New-Item -ItemType Directory -Force -Path $Path | Out-Null
    & icacls.exe $Path /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' "*$($Sid):(OI)(CI)F" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not secure $Path (icacls exit $LASTEXITCODE)" }
}
function Assert-OwnedService([string]$Name, [string]$Executable) {
    $service = Get-CimInstance Win32_Service -Filter "Name='$Name'"
    if ($service -and $service.PathName -notlike "`"$Executable`"*") { throw "Service $Name already exists with another executable; refusing to replace it." }
}

if ($Action -eq 'restore-services') {
    $dockerExe = Join-Path $install 'dockerd.exe'
    $controllerExe = Join-Path $install 'controller.exe'
    Assert-OwnedService 'UCDDocker' $dockerExe
    Assert-OwnedService 'UnifiedContainerDesktop' $controllerExe
    $daemonFile = Join-Path $data 'config\docker-daemon.json'
    if (-not (Test-Path $dockerExe) -or -not (Test-Path $controllerExe) -or -not (Test-Path $daemonFile)) { throw 'Cannot restore services: program files or Docker configuration are missing.' }
    $dockerCommand = '"' + $dockerExe + '" --run-service --service-name UCDDocker --config-file "' + $daemonFile + '"'
    [UcdNative]::EnsureService('UCDDocker', 'Unified Container Desktop Windows Engine', $dockerCommand, $false)
    [UcdNative]::EnsureService('UnifiedContainerDesktop', 'Unified Container Desktop', ('"' + $controllerExe + '"'), $true)
    [UcdNative]::EnsureControllerQueryAccess()
    Start-Service UCDDocker
    Start-Service UnifiedContainerDesktop
    exit 0
}
if ($Action -eq 'rollback') {
    # Windows Installer restores files; leave the durable identity and data in place.
    foreach ($name in @('UnifiedContainerDesktop', 'UCDDocker')) {
        $executable = if ($name -eq 'UCDDocker') { Join-Path $install 'dockerd.exe' } else { Join-Path $install 'controller.exe' }
        Assert-OwnedService $name $executable
        $service = Get-Service -Name $name -ErrorAction SilentlyContinue
        if ($service -and $service.Status -ne 'Stopped') { Stop-Service -Name $name -Force -ErrorAction Stop }
        if ($service -and $RemoveServices) {
            & sc.exe delete $name | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "Could not remove $name during rollback (sc.exe exit $LASTEXITCODE)" }
        }
    }
    exit 0
}

if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -ne 'AMD64') { throw 'Windows 11 x64 is required.' }
$os = Get-CimInstance Win32_OperatingSystem
if ($os.ProductType -ne 1 -or [int]$os.BuildNumber -lt 22621 -or $os.OperatingSystemSKU -notin @(4, 27, 48, 49, 72, 125, 126, 129, 130, 161, 162, 164, 165, 175)) { throw 'Windows 11 Pro or Enterprise build 22621 or later is required.' }
$ErrorActionPreference = 'Continue'
try {
    $wslVersion = (& "$env:SystemRoot\System32\wsl.exe" --version 2>&1 | Out-String) -replace "`0", ''
    $wslExit = $LASTEXITCODE
} finally { $ErrorActionPreference = 'Stop' }
if ($wslExit -ne 0 -or $wslVersion -notmatch '(\d+\.\d+\.\d+)') { throw 'WSL 2.5.4 or later must be installed before this MSI. Run the supplied Setup.ps1 bootstrapper.' }
if ([version]$Matches[1] -lt [version]'2.5.4') { throw 'WSL 2.5.4 or later must be installed before this MSI. Run the supplied Setup.ps1 bootstrapper.' }

$dockerExe = Join-Path $install 'dockerd.exe'
$controllerExe = Join-Path $install 'controller.exe'
Assert-OwnedService 'UCDDocker' $dockerExe
Assert-OwnedService 'UnifiedContainerDesktop' $controllerExe
$ourDocker = Get-CimInstance Win32_Service -Filter "Name='UCDDocker'"
$competing = @(Get-CimInstance Win32_Process -Filter "Name='dockerd.exe'" | Where-Object { -not $ourDocker -or $_.ProcessId -ne $ourDocker.ProcessId })
if ($competing.Count) { throw 'Another native Windows Docker daemon is running. Stop it before installing Unified Container Desktop.' }

$restart = $false
foreach ($feature in @('Containers', 'Microsoft-Hyper-V-All', 'Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform')) {
    $state = Get-WindowsOptionalFeature -Online -FeatureName $feature
    if ($state.State -ne 'Enabled') {
        $result = Enable-WindowsOptionalFeature -Online -FeatureName $feature -All -NoRestart
        if ($result.RestartNeeded) { $restart = $true }
    }
}
if (-not (Get-LocalGroup -Name $group -ErrorAction SilentlyContinue)) { New-LocalGroup -Name $group -Description 'Privileged Unified Container Desktop operators' | Out-Null }
$exists = [bool](Get-LocalUser -Name $account -ErrorAction SilentlyContinue)
$sid = [UcdNative]::EnsureAccount($exists)
[UcdNative]::GrantRights($sid)
$admins = ([Security.Principal.SecurityIdentifier]'S-1-5-32-544').Translate([Security.Principal.NTAccount]).Value.Split('\')[-1]
if (-not (Get-LocalGroupMember -Group $admins | Where-Object { $_.SID.Value -eq $sid })) { Add-LocalGroupMember -Group $admins -Member (Get-LocalUser -Name $account) }
Grant-DataAccess $data $sid

$profile = Join-Path $data 'service-profile'
$profileKey = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\$sid"
if (-not (Test-Path $profileKey)) {
    $created = [UcdNative]::NewProfile($sid)
    if (Test-Path $profile) { throw "Profile destination already exists without registration: $profile" }
    $created = (Resolve-Path -LiteralPath $created).ProviderPath
    $registeredSource = [Environment]::ExpandEnvironmentVariables((Get-ItemProperty -Path $profileKey -Name ProfileImagePath).ProfileImagePath)
    $registeredSource = (Resolve-Path -LiteralPath $registeredSource).ProviderPath
    $destination = [IO.Path]::GetFullPath($profile)
    $destinationParent = (Resolve-Path -LiteralPath (Split-Path -Parent $destination)).ProviderPath
    $expectedParent = (Resolve-Path -LiteralPath $data).ProviderPath
    if ($created -ine $registeredSource -or $created -ieq [IO.Path]::GetPathRoot($created) -or $destinationParent -ine $expectedParent -or $destination -ine (Join-Path $expectedParent 'service-profile') -or $created -ieq $destination) { throw 'Created profile registration or destination is unexpected; refusing to move it.' }
    $moved = $false
    try {
        Move-Item -LiteralPath $created -Destination $destination
        $moved = $true
        Set-ItemProperty -Path $profileKey -Name ProfileImagePath -Value $destination -Type ExpandString
    } catch {
        if ($moved -and (Test-Path -LiteralPath $destination) -and -not (Test-Path -LiteralPath $created)) { Move-Item -LiteralPath $destination -Destination $created }
        throw
    }
} else {
    $registered = [Environment]::ExpandEnvironmentVariables((Get-ItemProperty -Path $profileKey -Name ProfileImagePath).ProfileImagePath)
    if ($registered -ine $profile -or -not (Test-Path (Join-Path $profile 'NTUSER.DAT'))) { throw 'UcdService has an unexpected or incomplete profile registration.' }
}
Grant-DataAccess $profile $sid
$daemonDir = Join-Path $data 'config'
Grant-DataAccess $daemonDir $sid
$daemonFile = Join-Path $daemonDir 'docker-daemon.json'
$dockerGroup = (Get-LocalGroup -SID 'S-1-5-32-544').Name
if (-not (Test-Path $daemonFile)) {
    $daemon = @{ hosts = @('npipe:////./pipe/UCDDocker'); group = $dockerGroup; 'data-root' = (Join-Path $data 'windows'); 'exec-root' = (Join-Path $data 'windows-exec'); pidfile = (Join-Path $data 'windows\docker.pid') }
    $writeDaemon = $true
} else {
    $daemon = Get-Content -Raw -LiteralPath $daemonFile -Encoding UTF8 | ConvertFrom-Json
    if ($daemon.group -notin @($group, $dockerGroup) -or $daemon.hosts.Count -ne 1 -or $daemon.hosts[0] -ne 'npipe:////./pipe/UCDDocker' -or $daemon.'data-root' -ine (Join-Path $data 'windows') -or $daemon.'exec-root' -ine (Join-Path $data 'windows-exec') -or $daemon.pidfile -ine (Join-Path $data 'windows\docker.pid')) { throw "Existing Docker configuration at $daemonFile does not match this installation." }
    $writeDaemon = $daemon.group -ne $dockerGroup
    $daemon.group = $dockerGroup
}
if ($writeDaemon) {
    # Operators use the controller broker. Restrict every newly created native
    # daemon pipe immediately, including before the controller starts.
    $temporaryDaemon = Join-Path $daemonDir ('.docker-daemon-' + [guid]::NewGuid().ToString('N') + '.json')
    try {
        [IO.File]::WriteAllText($temporaryDaemon, ($daemon | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding $false))
        Move-Item -LiteralPath $temporaryDaemon -Destination $daemonFile -Force
    } finally {
        if (Test-Path -LiteralPath $temporaryDaemon) { Remove-Item -LiteralPath $temporaryDaemon -Force }
    }
}
$dockerCommand = '"' + $dockerExe + '" --run-service --service-name UCDDocker --config-file "' + $daemonFile + '"'
[UcdNative]::EnsureService('UCDDocker', 'Unified Container Desktop Windows Engine', $dockerCommand, $false)
[UcdNative]::EnsureService('UnifiedContainerDesktop', 'Unified Container Desktop', ('"' + $controllerExe + '"'), $true)
[UcdNative]::EnsureControllerQueryAccess()
if (-not $restart) {
    Start-Service UCDDocker
    Start-Service UnifiedContainerDesktop
} else { Write-Host 'Windows features require a reboot. Both services will start after restart.' }
