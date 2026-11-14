//go:build windows

package local

import (
	"errors"
	"fmt"
	"sort"
	"syscall"
	"unsafe"

	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows"
)

var netLocalGroupMembers = windows.NewLazySystemDLL("netapi32.dll").NewProc("NetLocalGroupGetMembers")

// operatorMembers reads the current local group, rather than the group snapshot
// captured at Windows sign-in. Copy SIDs before releasing NetAPI's native buffer.
func operatorMembers() ([]*windows.SID, error) {
	group, _ := windows.UTF16PtrFromString(api.OperatorGroup)
	var result []*windows.SID
	var resume uintptr
	for {
		var buffer *byte
		var read, total uint32
		code, _, _ := netLocalGroupMembers.Call(0, uintptr(unsafe.Pointer(group)), 0,
			uintptr(unsafe.Pointer(&buffer)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&read)),
			uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume)))
		if code != 0 && syscall.Errno(code) != windows.ERROR_MORE_DATA {
			if buffer != nil {
				_ = windows.NetApiBufferFree(buffer)
			}
			// An unprovisioned machine has no operators yet.
			if code == 2220 || syscall.Errno(code) == windows.ERROR_NO_SUCH_ALIAS {
				return nil, nil
			}
			return nil, fmt.Errorf("read current %s membership: %w", api.OperatorGroup, syscall.Errno(code))
		}
		if read > 0 && buffer == nil {
			return nil, errors.New("Windows returned group members without a buffer")
		}
		var copyErr error
		for _, sid := range unsafe.Slice((**windows.SID)(unsafe.Pointer(buffer)), int(read)) {
			if sid == nil || !sid.IsValid() {
				copyErr = errors.New("Windows returned an invalid group member SID")
				break
			}
			member, err := sid.Copy()
			if err != nil {
				copyErr = fmt.Errorf("copy operator SID: %w", err)
				break
			}
			result = append(result, member)
		}
		if buffer != nil {
			_ = windows.NetApiBufferFree(buffer)
		}
		if copyErr != nil {
			return nil, copyErr
		}
		if code == 0 {
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result, nil
}

func isCurrentOperator(token windows.Token, members []*windows.SID) (bool, error) {
	for _, member := range members {
		// CheckTokenMembership respects disabled/deny-only groups and restricted
		// tokens. Account lookup alone would incorrectly elevate those callers.
		enabled, err := token.IsMember(member)
		if err != nil {
			return false, fmt.Errorf("check effective operator membership: %w", err)
		}
		if enabled {
			return true, nil
		}
	}
	return false, nil
}
