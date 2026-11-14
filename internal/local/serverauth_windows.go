//go:build windows

package local

import (
	"errors"
	"fmt"
	"net"
	"unsafe"

	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows"
)

// verifyController binds the connected pipe server to the service process
// reported by Windows SCM. Service configuration is administrator-controlled.
func verifyController(conn net.Conn) error {
	return verifyPipeServer(conn, api.ServiceName)
}

func verifyPipeServer(conn net.Conn, serviceName string) error {
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("named pipe handle is unavailable")
	}
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return fmt.Errorf("open service control manager: %w", err)
	}
	defer windows.CloseServiceHandle(manager)
	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		return fmt.Errorf("encode service name: %w", err)
	}
	service, err := windows.OpenService(manager, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return fmt.Errorf("open controller service: %w", err)
	}
	defer windows.CloseServiceHandle(service)

	// Windows writes the fixed-size status value synchronously and does not
	// retain the pointer after the call.
	query := func() (windows.SERVICE_STATUS_PROCESS, error) {
		var status windows.SERVICE_STATUS_PROCESS
		var needed uint32
		err := windows.QueryServiceStatusEx(service, windows.SC_STATUS_PROCESS_INFO,
			(*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed)
		return status, err
	}
	first, err := query()
	if err != nil {
		return fmt.Errorf("query controller service: %w", err)
	}
	var pipePID uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fd.Fd()), &pipePID); err != nil {
		return fmt.Errorf("get pipe server process: %w", err)
	}
	second, err := query()
	if err != nil {
		return fmt.Errorf("recheck controller service: %w", err)
	}
	if err := matchServiceProcess(first, pipePID); err != nil {
		return err
	}
	return matchServiceProcess(second, pipePID)
}

func matchServiceProcess(status windows.SERVICE_STATUS_PROCESS, pipePID uint32) error {
	if status.CurrentState != windows.SERVICE_RUNNING || status.ServiceType&windows.SERVICE_WIN32_OWN_PROCESS == 0 || status.ServiceType&windows.SERVICE_WIN32_SHARE_PROCESS != 0 {
		return errors.New("controller service is not running in its own process")
	}
	if pipePID == 0 || status.ProcessId != pipePID {
		return errors.New("named pipe server is not the controller service process")
	}
	return nil
}
