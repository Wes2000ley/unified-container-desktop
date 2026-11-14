//go:build windows

package cli

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protectConfigCopy applies the existing DACL before writing credentials into
// the temporary file. New config files are private to the user and admins.
func protectConfigCopy(source, target string) error {
	var descriptor *windows.SECURITY_DESCRIPTOR
	var err error
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if source != "" {
		descriptor, err = windows.GetNamedSecurityInfo(source, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		control, _, controlErr := descriptor.Control()
		if controlErr != nil {
			return controlErr
		}
		if control&windows.SE_DACL_PROTECTED == 0 {
			flags = windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
		}
	} else {
		tokenUser, tokenErr := windows.GetCurrentProcessToken().GetTokenUser()
		if tokenErr != nil {
			return tokenErr
		}
		descriptor, err = windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)", tokenUser.User.Sid.String()))
		if err != nil {
			return err
		}
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, flags, nil, nil, dacl, nil)
}

func replaceDockerConfig(source, target string, exists bool) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if !exists {
		// Do not replace a config that another Docker process just created.
		return windows.MoveFile(from, to)
	}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")
	result, _, callErr := proc.Call(uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)), 0, 0, 0, 0)
	if result == 0 {
		if callErr == syscall.Errno(0) {
			return syscall.EINVAL
		}
		return callErr
	}
	return nil
}
