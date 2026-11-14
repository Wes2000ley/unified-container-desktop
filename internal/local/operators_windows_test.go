//go:build windows

package local

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func operatorTestToken(t *testing.T) windows.Token {
	t.Helper()
	var process windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &process); err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	var token windows.Token
	if err := windows.DuplicateTokenEx(process,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE, nil,
		windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = token.Close() })
	return token
}

func TestCurrentOperatorMembershipUsesLiveMemberList(t *testing.T) {
	token := operatorTestToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := windows.StringToSid("S-1-5-21-10-20-30-98765")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		members []*windows.SID
		want    bool
	}{
		{"added directly", []*windows.SID{unrelated, user.User.Sid}, true},
		{"removed", []*windows.SID{unrelated}, false},
		{"empty group", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := isCurrentOperator(token, test.members)
			if err != nil || got != test.want {
				t.Fatalf("current membership = %v, %v; want %v", got, err, test.want)
			}
		})
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := token.IsMember(admin)
	if err != nil {
		t.Fatal(err)
	}
	got, err := isCurrentOperator(token, []*windows.SID{admin})
	if err != nil || got != enabled {
		t.Fatalf("deny-only group membership = %v, %v; effective membership = %v", got, err, enabled)
	}
}

func TestCurrentOperatorMembershipRespectsRestrictedToken(t *testing.T) {
	token := operatorTestToken(t)
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	restriction, err := windows.StringToSid("S-1-5-21-10-20-30-98765")
	if err != nil {
		t.Fatal(err)
	}
	entry := windows.SIDAndAttributes{Sid: restriction}
	var restricted windows.Token
	create := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	r, _, callErr := create.Call(uintptr(token), 0, 0, 0, 0, 0, 1,
		uintptr(unsafe.Pointer(&entry)), uintptr(unsafe.Pointer(&restricted)))
	if r == 0 {
		t.Fatal(callErr)
	}
	defer restricted.Close()
	got, err := isCurrentOperator(restricted, []*windows.SID{user.User.Sid})
	if err != nil || got {
		t.Fatalf("restricted caller was elevated by its account SID: %v, %v", got, err)
	}
}

func TestDockerPipeAccessUpdatesExistingAndFutureInstances(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\ucd-live-access-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: dockerPipeSecurity(), MessageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Let the client complete its security calls before disconnecting.
			go func() { defer conn.Close(); var b [1]byte; _, _ = conn.Read(b[:]) }()
		}
	}()
	defer func() { _ = listener.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	verified := false
	if err := updateDockerPipeAccess(ctx, path, dockerPipeSecurity(), func(net.Conn) error { verified = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Fatal("updated an unauthenticated pipe")
	}
	// Open a fresh instance after the updating connection has closed.
	conn, err := winio.DialPipeAccessImpLevel(ctx, path,
		windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, winio.PipeImpLevelAnonymous)
	if err != nil {
		t.Fatal(err)
	}
	_, err = windows.GetSecurityInfo(windows.Handle(conn.(interface{ Fd() uintptr }).Fd()),
		windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dockerPipeSecurity(), ";;;AU") {
		t.Fatalf("Docker pipe descriptor includes all authenticated users: %s", dockerPipeSecurity())
	}
}
