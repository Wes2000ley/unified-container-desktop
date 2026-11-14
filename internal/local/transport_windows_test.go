//go:build windows

package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/skyri/ucd/internal/api"
	"golang.org/x/sys/windows"
)

func TestWindowsPipeAuthenticatesAndDispatches(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\ucd-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSecurity})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	principals := make(chan api.Principal, 2)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveListener(ctx, listener, callerFunc(func(ctx context.Context, _ api.Request) (any, error) {
			calls.Add(1)
			principals <- api.CurrentPrincipal(ctx)
			return map[string]string{"message": "real pipe"}, nil
		}))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("server did not stop")
		}
	})

	// The production client must reject this real pipe because its owner is
	// the test process, not the registered controller service process.
	untrusted := newClient(path)
	unauthCtx, stopUnauth := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = untrusted.Call(unauthCtx, api.Request{Method: "engine.list"})
	stopUnauth()
	_ = untrusted.Close()
	if err == nil || !strings.Contains(err.Error(), "authenticate local controller") {
		t.Fatalf("unregistered pipe server was not rejected: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("untrusted server received a request")
	}

	client := newPipeClient(path, func(conn net.Conn) error {
		fd, ok := conn.(interface{ Fd() uintptr })
		if !ok {
			return errors.New("test pipe handle is unavailable")
		}
		var pid uint32
		if err := windows.GetNamedPipeServerProcessId(windows.Handle(fd.Fd()), &pid); err != nil {
			return err
		}
		if pid != uint32(os.Getpid()) {
			return fmt.Errorf("pipe server PID = %d, want %d", pid, os.Getpid())
		}
		return nil
	})
	t.Cleanup(func() { _ = client.Close() })
	callCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	var result map[string]string
	if err := client.CallInto(callCtx, api.Request{Method: "engine.list"}, &result); err != nil {
		t.Fatalf("real pipe call: %v", err)
	}
	if result["message"] != "real pipe" {
		t.Fatalf("unexpected result: %#v", result)
	}
	principal := <-principals
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if principal.SID != user.User.Sid.String() || principal.Name == "" {
		t.Fatalf("pipe principal = %+v, expected SID %s", principal, user.User.Sid.String())
	}
	if principal.Role != "viewer" && principal.Role != "operator" && principal.Role != "administrator" {
		t.Fatalf("unexpected role: %q", principal.Role)
	}
	if principal.Role != "administrator" {
		_, err := client.Call(callCtx, api.Request{Method: "engine.start"})
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != "permission_denied" {
			t.Fatalf("non-administrator request was not denied: %v", err)
		}
	}

	// Identity supplied in JSON must never replace the real Windows token.
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, "http://ucd/v1/call", strings.NewReader(`{"method":"engine.start","principal":{"sid":"S-1-5-18","role":"administrator"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rejected struct {
		Error *api.Error `json:"error"`
	}
	err = json.NewDecoder(resp.Body).Decode(&rejected)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusBadRequest || rejected.Error == nil || rejected.Error.Code != "invalid_argument" {
		t.Fatalf("forged identity response = %d, %+v, %v", resp.StatusCode, rejected, err)
	}
	if calls.Load() != 1 {
		t.Fatal("forged identity reached the caller")
	}

	// A client that offers only an anonymous impersonation token must fail
	// closed even when it can open the pipe.
	anonymous := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeAccessImpLevel(ctx, path, pipeClientAccess, winio.PipeImpLevelAnonymous)
		},
	}}
	req, err = http.NewRequestWithContext(callCtx, http.MethodPost, "http://ucd/v1/call", strings.NewReader(`{"method":"engine.list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = anonymous.Do(req)
	if err != nil {
		t.Fatalf("anonymous pipe call: %v", err)
	}
	rejected.Error = nil
	err = json.NewDecoder(resp.Body).Decode(&rejected)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusUnauthorized || rejected.Error == nil || rejected.Error.Code != "unauthenticated" {
		t.Fatalf("anonymous response = %d, %+v, %v", resp.StatusCode, rejected, err)
	}
	if calls.Load() != 1 {
		t.Fatal("anonymous identity reached the caller")
	}
}

func TestMatchServiceProcess(t *testing.T) {
	running := windows.SERVICE_STATUS_PROCESS{
		CurrentState: windows.SERVICE_RUNNING,
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		ProcessId:    1234,
	}
	if err := matchServiceProcess(running, 1234); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status windows.SERVICE_STATUS_PROCESS
		pid    uint32
	}{
		{"different PID", running, 1235},
		{"zero PID", running, 0},
		{"stopped service", windows.SERVICE_STATUS_PROCESS{CurrentState: windows.SERVICE_STOPPED, ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, ProcessId: 1234}, 1234},
		{"shared process", windows.SERVICE_STATUS_PROCESS{CurrentState: windows.SERVICE_RUNNING, ServiceType: windows.SERVICE_WIN32_SHARE_PROCESS, ProcessId: 1234}, 1234},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := matchServiceProcess(test.status, test.pid); err == nil {
				t.Fatal("untrusted service process accepted")
			}
		})
	}
}

func TestWindowsPipeShutdownCancelsInFlightCall(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\ucd-stop-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSecurity})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveListener(ctx, listener, callerFunc(func(ctx context.Context, _ api.Request) (any, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}))
	}()
	client := newPipeClient(path, func(net.Conn) error { return nil })
	defer client.Close()
	callDone := make(chan error, 1)
	callCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	go func() {
		_, err := client.Call(callCtx, api.Request{Method: "engine.list"})
		callDone <- err
	}()
	select {
	case <-started:
	case <-callCtx.Done():
		t.Fatal("call did not reach server")
	}
	cancel()
	select {
	case <-canceled:
	case <-callCtx.Done():
		t.Fatal("shutdown did not cancel the in-flight call")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-callCtx.Done():
		t.Fatal("server did not shut down")
	}
	if err := <-callDone; err == nil {
		t.Fatal("canceled call unexpectedly succeeded")
	}
}

// TestPipeACLForNonOwner uses AccessCheck with a SYSTEM-owned descriptor and
// the current non-administrator token. It must not exercise Owner Rights.
func TestPipeACLForNonOwner(t *testing.T) {
	var process windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &process); err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	var token windows.Token
	if err := windows.DuplicateTokenEx(process, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	admin, err := token.IsMember(adminSID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := process.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if admin || user.User.Sid.String() == "S-1-5-18" {
		t.Skip("requires a non-administrator, non-SYSTEM token")
	}
	sd, err := windows.SecurityDescriptorFromString("O:SYG:SY" + pipeSecurity)
	if err != nil {
		t.Fatal(err)
	}
	type genericMapping struct{ Read, Write, Execute, All uint32 }
	mapping := genericMapping{Read: 0x00120089, Write: 0x00120116, Execute: 0x001200a0, All: 0x001f01ff}
	accessCheck := windows.NewLazySystemDLL("advapi32.dll").NewProc("AccessCheck")
	check := func(desired uint32) bool {
		t.Helper()
		var privileges [1024]byte
		size := uint32(len(privileges))
		var granted uint32
		var allowed int32
		r, _, err := accessCheck.Call(uintptr(unsafe.Pointer(sd)), uintptr(token), uintptr(desired), uintptr(unsafe.Pointer(&mapping)), uintptr(unsafe.Pointer(&privileges[0])), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&granted)), uintptr(unsafe.Pointer(&allowed)))
		if r == 0 {
			t.Fatalf("AccessCheck: %v", err)
		}
		return allowed != 0
	}
	if !check(pipeClientAccess) {
		t.Fatal("non-owner authenticated user lacks client access")
	}
	if check(windows.FILE_APPEND_DATA) {
		t.Fatal("non-owner can create pipe instances")
	}
}
