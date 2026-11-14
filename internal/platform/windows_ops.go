//go:build windows

package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// WindowsCall handles operations that do not require the Linux distribution or profile.
// The controller must authenticate and authorize requests before calling it.
func WindowsCall(ctx context.Context, req api.Request) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch req.Method {
	case "engine.start", "engine.stop":
		if req.Engine != "windows" {
			return nil, api.Fail("invalid_argument", "Windows engine is required")
		}
		return nil, controlWindowsEngine(ctx, strings.TrimPrefix(req.Method, "engine."))
	case "operator.list":
		return listOperators()
	case "operator.add", "operator.remove":
		p, err := api.Decode[struct {
			Account string `json:"account"`
		}](req)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Account) == "" {
			return nil, api.Fail("invalid_argument", "account is required")
		}
		return nil, changeOperator(p.Account, req.Method == "operator.add")
	default:
		return nil, api.Fail("not_found", "unknown Windows infrastructure method")
	}
}

var (
	netapi                  = windows.NewLazySystemDLL("netapi32.dll")
	netLocalGroupGetMembers = netapi.NewProc("NetLocalGroupGetMembers")
	netLocalGroupAddMembers = netapi.NewProc("NetLocalGroupAddMembers")
	netLocalGroupDelMembers = netapi.NewProc("NetLocalGroupDelMembers")
)

// engineControls serializes start/stop operations, including their state waits.
var engineControls = make(chan struct{}, 1)

func controlWindowsEngine(ctx context.Context, action string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	select {
	case engineControls <- struct{}{}:
		defer func() { <-engineControls }()
	case <-ctx.Done():
		return fmt.Errorf("wait to %s UCDDocker service: %w", action, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wait to %s UCDDocker service: %w", action, err)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService("UCDDocker")
	if err != nil {
		return fmt.Errorf("open UCDDocker service: %w", err)
	}
	defer s.Close()
	return setWindowsEngineState(ctx, s, action, 250*time.Millisecond)
}

type windowsEngineService interface {
	Query() (svc.Status, error)
	Start(...string) error
	Control(svc.Cmd) (svc.Status, error)
}

func setWindowsEngineState(ctx context.Context, service windowsEngineService, action string, interval time.Duration) error {
	// Complete an earlier transition before requesting another one. In
	// particular, Start on STOP_PENDING can report already running.
	status, err := waitWindowsEngineState(ctx, service, interval, func(state svc.State) bool {
		return state != svc.StartPending && state != svc.StopPending
	})
	if err != nil {
		return fmt.Errorf("inspect UCDDocker before %s: %w", action, err)
	}
	var target svc.State
	switch action {
	case "start":
		target = svc.Running
		if status.State == target {
			return nil
		}
		if status.State != svc.Stopped {
			return fmt.Errorf("cannot start UCDDocker service from state %d", status.State)
		}
		err = service.Start()
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			err = nil
		}
	case "stop":
		target = svc.Stopped
		if status.State == target {
			return nil
		}
		if status.State != svc.Running {
			return fmt.Errorf("cannot stop UCDDocker service from state %d", status.State)
		}
		_, err = service.Control(svc.Stop)
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			err = nil
		}
	default:
		return fmt.Errorf("unsupported UCDDocker action %q", action)
	}
	if err != nil {
		return fmt.Errorf("%s UCDDocker service: %w", action, err)
	}
	status, err = waitWindowsEngineState(ctx, service, interval, func(state svc.State) bool {
		return state == target
	})
	if err != nil {
		return fmt.Errorf("UCDDocker service did not %s: last state %d, Windows exit code %d, service exit code %d: %w", action, status.State, status.Win32ExitCode, status.ServiceSpecificExitCode, err)
	}
	return nil
}

func waitWindowsEngineState(ctx context.Context, service windowsEngineService, interval time.Duration, ready func(svc.State) bool) (svc.Status, error) {
	var status svc.Status
	for {
		if err := ctx.Err(); err != nil {
			return status, err
		}
		next, err := service.Query()
		if err != nil {
			return status, fmt.Errorf("query service status: %w", err)
		}
		status = next
		if ready(status.State) {
			return status, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, ctx.Err()
		case <-timer.C:
		}
	}
}

type groupMember struct{ Name *uint16 }

func listOperators() ([]string, error) {
	group, _ := windows.UTF16PtrFromString(api.OperatorGroup)
	result := []string{}
	var resume uintptr
	for {
		var buffer *byte
		var read, total uint32
		code, _, _ := netLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(group)), 3, uintptr(unsafe.Pointer(&buffer)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume)))
		if buffer != nil {
			for _, member := range unsafe.Slice((*groupMember)(unsafe.Pointer(buffer)), int(read)) {
				result = append(result, windows.UTF16PtrToString(member.Name))
			}
			_ = windows.NetApiBufferFree(buffer)
		}
		if code == 0 {
			break
		}
		if syscall.Errno(code) != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("list %s group members: %w", api.OperatorGroup, syscall.Errno(code))
		}
	}
	sort.Strings(result)
	return result, nil
}

func changeOperator(account string, add bool) error {
	account = strings.TrimSpace(account)
	name, err := windows.UTF16PtrFromString(account)
	if err != nil {
		return api.Fail("invalid_argument", "account contains an invalid character")
	}
	group, _ := windows.UTF16PtrFromString(api.OperatorGroup)
	member := groupMember{Name: name}
	proc, action := netLocalGroupDelMembers, "remove"
	if add {
		proc, action = netLocalGroupAddMembers, "add"
	}
	code, _, _ := proc.Call(0, uintptr(unsafe.Pointer(group)), 3, uintptr(unsafe.Pointer(&member)), 1)
	if code != 0 {
		return fmt.Errorf("%s %q in %s: %w", action, account, api.OperatorGroup, syscall.Errno(code))
	}
	return nil
}

type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  uint8
	Directory      uint8
	_              [2]byte
}

// allocatedFileBytes reports physical allocation, including sparse VHDX files.
func allocatedFileBytes(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var info fileStandardInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, err
	}
	return info.AllocationSize, nil
}
