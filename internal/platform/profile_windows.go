//go:build windows

package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	userenv               = windows.NewLazySystemDLL("userenv.dll")
	loadUserProfile       = userenv.NewProc("LoadUserProfileW")
	unloadUserProfile     = userenv.NewProc("UnloadUserProfile")
	adjustTokenPrivileges = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")
)

// profileInfo has the native PROFILEINFOW layout.
type profileInfo struct {
	Size, Flags                         uint32
	UserName, ProfilePath               *uint16
	DefaultPath, ServerName, PolicyPath *uint16
	Profile                             windows.Handle
}

type serviceProfile struct {
	token windows.Token
	hive  windows.Handle
	sid   string
	env   []string
}

func openServiceProfile(account, directory string) (*serviceProfile, error) {
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return nil, fmt.Errorf("identify service session: %w", err)
	}
	if session != 0 {
		return nil, fmt.Errorf("controller must run as the dedicated service account in session 0")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE|windows.TOKEN_ADJUST_PRIVILEGES, &token); err != nil {
		return nil, fmt.Errorf("open service account token: %w", err)
	}
	closeToken := true
	defer func() {
		if closeToken {
			_ = token.Close()
		}
	}()
	// LookupAccountName does not accept the .\ shorthand. Qualify the exact
	// dedicated local account using the machine's NetBIOS computer name.
	expected, err := localAccountSID("UcdService")
	if err != nil {
		return nil, fmt.Errorf("resolve configured service account %q: %w", account, err)
	}
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read service account identity: %w", err)
	}
	if !expected.Equals(user.User.Sid) {
		return nil, fmt.Errorf("controller token does not belong to configured service account %q", account)
	}
	if !token.IsElevated() {
		return nil, fmt.Errorf("service account %q must have an elevated administrator token to load its profile", account)
	}
	for _, privilege := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
		if err := enablePrivilege(token, privilege); err != nil {
			return nil, fmt.Errorf("enable %s: %w", privilege, err)
		}
	}
	name := account
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("invalid service account name: %w", err)
	}
	const piNoUI = 0x00000001 // PI_NOUI prevents profile error messages from being displayed.
	info := profileInfo{Size: uint32(unsafe.Sizeof(profileInfo{})), Flags: piNoUI, UserName: namePtr}
	r, _, callErr := loadUserProfile.Call(uintptr(token), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return nil, fmt.Errorf("load service account profile: %w", callErr)
	}
	unload := true
	defer func() {
		if unload {
			_, _, _ = unloadUserProfile.Call(uintptr(token), uintptr(info.Profile))
		}
	}()
	actual, err := token.GetUserProfileDirectory()
	if err != nil {
		return nil, fmt.Errorf("locate service account profile: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(actual), filepath.Clean(directory)) {
		return nil, fmt.Errorf("service account profile is %q; expected %q", actual, directory)
	}
	env, err := token.Environ(false)
	if err != nil {
		return nil, fmt.Errorf("create service account environment: %w", err)
	}
	// WSL obtains the profile from the token; also ensure child environment agrees.
	env = setEnvironment(env, "USERPROFILE", actual)
	closeToken, unload = false, false
	return &serviceProfile{token: token, hive: info.Profile, sid: expected.String(), env: env}, nil
}

func localAccountSID(name string) (*windows.SID, error) {
	// Windows computer names are at most 15 characters, plus the terminator.
	var computer [16]uint16
	length := uint32(len(computer))
	if err := syscall.GetComputerName(&computer[0], &length); err != nil {
		return nil, fmt.Errorf("get local computer name: %w", err)
	}
	qualified := syscall.UTF16ToString(computer[:length]) + `\` + name
	sid, _, _, err := windows.LookupSID("", qualified)
	if err != nil {
		return nil, fmt.Errorf("lookup local account %q: %w", qualified, err)
	}
	return sid, nil
}

func enablePrivilege(token windows.Token, name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return err
	}
	state := windows.Tokenprivileges{PrivilegeCount: 1, Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
	r, _, callErr := adjustTokenPrivileges.Call(uintptr(token), 0, uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if r == 0 || callErr == windows.ERROR_NOT_ALL_ASSIGNED {
		return callErr
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return nil
}

func setEnvironment(env []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.EqualFold(strings.SplitN(entry, "=", 2)[0], name) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func (p *serviceProfile) close() error {
	r, _, callErr := unloadUserProfile.Call(uintptr(p.token), uintptr(p.hive))
	tokenErr := p.token.Close()
	if r == 0 {
		return fmt.Errorf("unload service account profile: %w", callErr)
	}
	return tokenErr
}
