//go:build windows

package local

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/skyri/ucd/internal/dockerproxy"
	"golang.org/x/sys/windows"
)

func TestDockerPipeAcceptsStockDockerClient(t *testing.T) {
	var err error
	path := fmt.Sprintf(`\\.\pipe\ucd-docker-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: dockerPipeSecurity(), MessageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveDockerListener(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.URL.RequestURI())
		}))
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop Docker pipe: %v", err)
		}
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		// Moby's stock Windows client uses this exact dialer, including its
		// anonymous impersonation level and generic read/write access.
		return winio.DialPipeContext(ctx, path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	resp, err := client.Get("http://docker/v1.45/containers/json?all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "/v1.45/containers/json?all=1" {
		t.Fatalf("pipe response = %d %q, %v", resp.StatusCode, body, err)
	}
}

func TestDockerPipeUpgradeSupportsHalfClose(t *testing.T) {
	backendEOF := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseBackend := func() { once.Do(func() { close(release) }) }
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		input, err := io.ReadAll(rw)
		if err != nil || string(input) != "input" {
			t.Errorf("upgraded input = %q, %v", input, err)
			return
		}
		close(backendEOF)
		<-release
		_, _ = rw.WriteString("after EOF")
		_ = rw.Flush()
	}))
	defer docker.Close()
	target, err := url.Parse(docker.URL)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf(`\\.\pipe\ucd-docker-upgrade-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: dockerPipeSecurity(), MessageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxyDone := make(chan struct{})
	serverDone := make(chan error, 1)
	proxy := dockerproxy.New(target, nil)
	go func() {
		serverDone <- serveDockerListener(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxy.ServeHTTP(w, r)
			close(proxyDone)
		}))
	}()
	defer func() {
		releaseBackend()
		cancel()
		if err := <-serverDone; err != nil {
			t.Errorf("stop Docker pipe: %v", err)
		}
	}()
	conn, err := winio.DialPipeContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(conn, "POST /v1.45/exec/id/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response = %v, %v", response, err)
	}
	_, _ = io.WriteString(conn, "input")
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-backendEOF:
	case <-time.After(5 * time.Second):
		t.Fatal("Docker did not receive stdin EOF")
	}
	select {
	case <-proxyDone:
		t.Fatal("proxy closed before Docker could finish writing")
	case <-time.After(100 * time.Millisecond):
	}
	releaseBackend()
	output, err := io.ReadAll(reader)
	if err != nil || string(output) != "after EOF" {
		t.Fatalf("output after stdin close = %q, %v", output, err)
	}
}

func TestDockerPipeAcceptsDockerCLI(t *testing.T) {
	// This tests direct protocol access to a fixture pipe. The system-wide
	// docker dispatcher intentionally rejects caller-supplied --host values.
	docker, err := exec.LookPath("docker-cli")
	if err != nil {
		t.Skip("bundled upstream Docker CLI is not installed")
	}
	name := fmt.Sprintf("ucd-docker-cli-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(`\\.\pipe\`+name, &winio.PipeConfig{SecurityDescriptor: dockerPipeSecurity(), MessageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveDockerListener(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/_ping", "/v1.45/_ping":
				w.Header().Set("Api-Version", "1.45")
				_, _ = io.WriteString(w, "OK")
			case "/v1.45/version":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"Version":"28.0.0","ApiVersion":"1.45","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`)
			default:
				http.NotFound(w, r)
			}
		}))
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop Docker pipe: %v", err)
		}
	}()
	dir := t.TempDir()
	commandCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	cmd := exec.CommandContext(commandCtx, docker, "--host", "npipe:////./pipe/"+name, "version", "--format", "{{.Server.Version}}")
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"PATH=" + os.Getenv("PATH"),
		"TEMP=" + dir,
		"TMP=" + dir,
		"DOCKER_CONFIG=" + dir,
		"DOCKER_API_VERSION=1.45",
	}
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "28.0.0" {
		t.Fatalf("Docker CLI version = %q, %v", output, err)
	}
	dial := exec.CommandContext(commandCtx, docker, "--host", "npipe:////./pipe/"+name, "system", "dial-stdio")
	dial.Env = cmd.Env
	dial.Stdin = strings.NewReader("GET /_ping HTTP/1.0\r\nHost: docker\r\n\r\n")
	output, err = dial.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "200 OK") || !strings.HasSuffix(string(output), "OK") {
		t.Fatalf("Docker CLI dial-stdio = %q, %v", output, err)
	}
}

func TestDockerPipeDACLChecksEffectiveToken(t *testing.T) {
	var process windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &process); err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	var token windows.Token
	if err := windows.DuplicateTokenEx(process, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	type genericMapping struct{ Read, Write, Execute, All uint32 }
	mapping := genericMapping{Read: 0x00120089, Write: 0x00120116, Execute: 0x001200a0, All: 0x001f01ff}
	accessCheck := windows.NewLazySystemDLL("advapi32.dll").NewProc("AccessCheck")
	check := func(descriptor string) bool {
		t.Helper()
		sd, err := windows.SecurityDescriptorFromString("O:SYG:SY" + descriptor)
		if err != nil {
			t.Fatal(err)
		}
		var privileges [1024]byte
		size := uint32(len(privileges))
		var granted uint32
		var allowed int32
		// AccessCheck expects generic rights to be mapped before the call.
		desired := mapping.Read | mapping.Write
		r, _, err := accessCheck.Call(uintptr(unsafe.Pointer(sd)), uintptr(token), uintptr(desired), uintptr(unsafe.Pointer(&mapping)), uintptr(unsafe.Pointer(&privileges[0])), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&granted)), uintptr(unsafe.Pointer(&allowed)))
		if r == 0 {
			t.Fatalf("AccessCheck: %v", err)
		}
		return allowed != 0
	}
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := token.IsMember(adminSID)
	if err != nil {
		t.Fatal(err)
	}
	allowed := check(dockerPipeSecurity())
	wantAllowed := admin || user.User.Sid.String() == "S-1-5-18"
	if allowed != wantAllowed {
		t.Fatalf("raw Docker pipe access = %v, want administrator/SYSTEM only (%v)", allowed, wantAllowed)
	}
	if !admin && user.User.Sid.String() != "S-1-5-18" {
		members, err := operatorMembers()
		if err != nil {
			t.Fatal(err)
		}
		operator, err := isCurrentOperator(token, members)
		if err != nil {
			t.Fatal(err)
		}
		if operator && allowed {
			t.Fatal("Container Operators member was granted raw Docker pipe access")
		}
	}
	if strings.Contains(dockerPipeSecurity(), ";;;AU") {
		t.Fatal("Docker pipe grants access to all authenticated users")
	}
	if strings.Contains(dockerPipeSecurity(), user.User.Sid.String()) && !admin {
		t.Fatal("Docker pipe grants access directly to the current non-administrator")
	}
}
