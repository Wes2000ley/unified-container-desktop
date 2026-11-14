//go:build windows

package local

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	dockerclient "github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
)

func TestDockerRelayBrokerPreservesImmediateExecOutput(t *testing.T) {
	const marker = "UCD_EXEC_MARKER\n"
	daemonEOF := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1.45/exec/test-exec/start" {
			t.Errorf("fake daemon request = %s %s", r.Method, r.URL.RequestURI())
			return
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
			t.Errorf("fake daemon upgrade = %q, want tcp", r.Header.Get("Upgrade"))
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack fake daemon response: %v", err)
			return
		}
		defer close(daemonEOF)
		defer conn.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
		frame := make([]byte, 8+len(marker))
		frame[0] = 1 // stdout
		binary.BigEndian.PutUint32(frame[4:8], uint32(len(marker)))
		copy(frame[8:], marker)
		if _, err := rw.Write(frame); err != nil {
			t.Errorf("write immediate exec output: %v", err)
			return
		}
		if err := rw.Flush(); err != nil {
			t.Errorf("flush immediate exec output: %v", err)
			return
		}
		// Model a short command that exits immediately after writing its output.
	}))
	defer daemon.Close()
	daemonURL, err := url.Parse(daemon.URL)
	if err != nil {
		t.Fatal(err)
	}

	daemonTransport := &http.Transport{Proxy: nil}
	t.Cleanup(daemonTransport.CloseIdleConnections)
	engine := dockerproxy.New(daemonURL, daemonTransport)
	ctx, relay, principalCalls := startDockerBrokerTestRelay(t, engine)

	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost(relay.Endpoint()), dockerclient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.ExecAttach(ctx, "test-exec", dockerclient.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("attach to fake exec: %v", err)
	}
	defer response.Close()
	// Model the race where the daemon exits before the CLI's independent stdin
	// copier propagates EOF, keeping output unread until after daemon close.
	select {
	case <-daemonEOF:
	case <-ctx.Done():
		t.Fatal("fake daemon did not exit after writing exec output")
	}
	// Match the bundled Docker CLI's non-interactive exec path: it drains the
	// stream while independently signaling that stdin is complete.
	type streamResult struct {
		stdout string
		stderr string
		err    error
	}
	streamDone := make(chan streamResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		_, err := stdcopy.StdCopy(&stdout, &stderr, response.Reader)
		streamDone <- streamResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	}()
	if err := response.CloseWrite(); err != nil {
		t.Fatalf("signal end of exec input: %v", err)
	}
	var output streamResult
	select {
	case output = <-streamDone:
	case <-ctx.Done():
		t.Fatal("exec output stream did not close")
	}
	if output.err != nil {
		t.Fatalf("read exec stream: %v", output.err)
	}
	if output.stdout != marker || output.stderr != "" {
		t.Fatalf("exec output = stdout %q, stderr %q; want stdout %q", output.stdout, output.stderr, marker)
	}
	if principalCalls.Load() != 1 {
		t.Fatalf("broker authenticated %d requests, want 1", principalCalls.Load())
	}
}

func startDockerBrokerTestRelay(t *testing.T, engine http.Handler) (context.Context, *DockerRelay, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	brokerPipe := fmt.Sprintf(`\\.\pipe\ucd-broker-stream-%d-%d`, os.Getpid(), time.Now().UnixNano())
	// Exercise the same listener configuration used for controller pipes that
	// serve Docker requests, including named-pipe half-close support.
	listener, err := listenControllerPipe(brokerPipe, true)
	if err != nil {
		t.Fatal(err)
	}
	brokerCtx, stopBroker := context.WithCancel(context.Background())
	brokerDone := make(chan error, 1)
	principalCalls := new(atomic.Int32)
	principal := func(context.Context) (api.Principal, error) {
		principalCalls.Add(1)
		return api.Principal{SID: "S-1-test", Role: "operator"}, nil
	}
	broker := handlerWithDocker(nil, principal, NewEngineRouter(engine, nil))
	brokerListener := newPeerListener(listener)
	brokerServer := &http.Server{
		Handler:           broker,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return brokerCtx },
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, peerKey{}, conn.(*peerConn))
		},
	}
	brokerServer.SetKeepAlivesEnabled(false)
	go func() { brokerDone <- brokerServer.Serve(brokerListener) }()
	t.Cleanup(func() {
		stopBroker()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		if err := brokerServer.Shutdown(shutdownCtx); err != nil {
			_ = brokerServer.Close()
			t.Errorf("shut down test Docker broker: %v", err)
		}
		select {
		case err := <-brokerDone:
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("stop test Docker broker: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("test Docker broker did not stop")
		}
	})

	// Match newPipeClient's production dial options and authenticated pipe
	// level. Only controller executable verification is stubbed for this test.
	brokerClient := newPipeClient(brokerPipe, func(net.Conn) error { return nil })
	t.Cleanup(brokerClient.transport.CloseIdleConnections)
	brokerURL := &url.URL{Scheme: "http", Host: "ucd", Path: "/docker/windows"}
	relay, err := startDockerRelay(ctx, dockerproxy.New(brokerURL, brokerClient.transport))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := relay.Close(); err != nil {
			t.Errorf("close test Docker relay: %v", err)
		}
	})
	return ctx, relay, principalCalls
}

func TestBundledDockerCLIExecThroughBroker(t *testing.T) {
	dockerPath := os.Getenv("UCD_TEST_DOCKER_CLI")
	if dockerPath == "" {
		dockerPath = `C:\Program Files\UnifiedContainerDesktop\docker-cli.exe`
	}
	if _, err := os.Stat(dockerPath); err != nil {
		if os.Getenv("UCD_TEST_DOCKER_CLI") != "" {
			t.Fatalf("explicit Docker CLI test fixture is unavailable at %s: %v", dockerPath, err)
		}
		t.Skipf("bundled Docker CLI is unavailable at %s: %v", dockerPath, err)
	}

	type execConfig struct {
		AttachStdin  bool     `json:"AttachStdin"`
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		TTY          bool     `json:"Tty"`
		Command      []string `json:"Cmd"`
		ExitCode     int
	}
	type dockerRequest struct {
		method          string
		uri             string
		headers         http.Header
		body            []byte
		status          int
		responseHeaders http.Header
	}
	var (
		requestsMu sync.Mutex
		requests   []dockerRequest
		execsMu    sync.Mutex
		execs      = make(map[string]execConfig)
		nextExec   atomic.Int32
	)
	appendFrame := func(dst []byte, stream byte, payload string) []byte {
		frame := make([]byte, 8+len(payload))
		frame[0] = stream
		binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
		copy(frame[8:], payload)
		return append(dst, frame...)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fake daemon request body: %v", err)
			return
		}
		record := dockerRequest{method: r.Method, uri: r.URL.RequestURI(), headers: r.Header.Clone(), body: body}
		defer func() {
			requestsMu.Lock()
			requests = append(requests, record)
			requestsMu.Unlock()
		}()
		writeJSON := func(status int, value any) {
			record.status = status
			w.Header().Set("Content-Type", "application/json")
			record.responseHeaders = w.Header().Clone()
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Errorf("write fake daemon JSON response: %v", err)
			}
		}
		switch {
		case r.URL.Path == "/_ping" || r.URL.Path == "/v1.45/_ping":
			record.status = http.StatusOK
			w.Header().Set("Api-Version", "1.45")
			record.responseHeaders = w.Header().Clone()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "OK")
		case r.URL.Path == "/version" || r.URL.Path == "/v1.45/version":
			writeJSON(http.StatusOK, map[string]string{
				"Version": "28.0.0", "ApiVersion": "1.45", "MinAPIVersion": "1.24",
				"Os": "windows", "Arch": "amd64",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/containers/test-container/json":
			writeJSON(http.StatusOK, map[string]any{
				"Id": "container-id", "Name": "/test-container",
				"State":  map[string]any{"Running": true, "Status": "running"},
				"Config": map[string]any{"Tty": false},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/containers/test-container/exec":
			var config execConfig
			if err := json.Unmarshal(body, &config); err != nil {
				t.Errorf("decode fake exec-create request: %v", err)
				writeJSON(http.StatusBadRequest, map[string]string{"message": "invalid exec config"})
				return
			}
			config.ExitCode = 0
			if strings.Contains(strings.Join(config.Command, " "), "exit-seven") {
				config.ExitCode = 7
			}
			id := fmt.Sprintf("exec-%d", nextExec.Add(1))
			execsMu.Lock()
			execs[id] = config
			execsMu.Unlock()
			writeJSON(http.StatusCreated, map[string]string{"Id": id})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1.45/exec/") && strings.HasSuffix(r.URL.Path, "/start"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/exec/"), "/start")
			execsMu.Lock()
			config, ok := execs[id]
			execsMu.Unlock()
			if !ok {
				writeJSON(http.StatusNotFound, map[string]string{"message": "exec not found"})
				return
			}
			var start struct {
				Detach bool `json:"Detach"`
				TTY    bool `json:"Tty"`
			}
			if err := json.Unmarshal(body, &start); err != nil {
				t.Errorf("decode fake exec-start request: %v", err)
				writeJSON(http.StatusBadRequest, map[string]string{"message": "invalid exec start"})
				return
			}
			if start.Detach {
				record.status = http.StatusOK
				record.responseHeaders = w.Header().Clone()
				w.WriteHeader(http.StatusOK)
				return
			}
			if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), "tcp") {
				t.Errorf("attached exec upgrade headers = Connection:%q Upgrade:%q", r.Header.Get("Connection"), r.Header.Get("Upgrade"))
			}
			record.status = http.StatusSwitchingProtocols
			record.responseHeaders = http.Header{
				"Connection":   {"Upgrade"},
				"Upgrade":      {"tcp"},
				"Content-Type": {"application/vnd.docker.multiplexed-stream"},
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack fake daemon exec response: %v", err)
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
			if err := rw.Flush(); err != nil {
				t.Errorf("flush fake exec upgrade handshake: %v", err)
				return
			}
			// Docker CLI sends a write-half-close after starting every attached
			// stream, even when `exec -i` did not attach stdin. Check the interactive
			// payload and wait for EOF before producing output, without a timing
			// sleep.
			input, err := io.ReadAll(rw.Reader)
			if err != nil {
				t.Errorf("read fake exec stdin through EOF: %v", err)
				return
			}
			wantInput := ""
			if config.AttachStdin {
				wantInput = "docker-cli-stdin\n"
			}
			if string(input) != wantInput {
				t.Errorf("fake exec stdin = %q, want %q", input, wantInput)
			}
			output := []byte(nil)
			if config.AttachStdout {
				marker := "docker-cli-stdout\n"
				if config.AttachStdin {
					marker = "docker-cli-stdin-eof-stdout\n"
				}
				output = appendFrame(output, 1, marker)
			}
			if config.AttachStderr {
				marker := "docker-cli-stderr\n"
				if config.AttachStdin {
					marker = "docker-cli-stdin-eof-stderr\n"
				}
				output = appendFrame(output, 2, marker)
			}
			if _, err := rw.Write(output); err != nil {
				t.Errorf("write fake exec output frames: %v", err)
				return
			}
			if err := rw.Flush(); err != nil {
				t.Errorf("flush fake exec output frames: %v", err)
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1.45/exec/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.45/exec/"), "/json")
			execsMu.Lock()
			config, ok := execs[id]
			execsMu.Unlock()
			if !ok {
				writeJSON(http.StatusNotFound, map[string]string{"message": "exec not found"})
				return
			}
			writeJSON(http.StatusOK, map[string]any{
				"ID": id, "ContainerID": "container-id", "Running": false, "ExitCode": config.ExitCode,
			})
		default:
			record.status = http.StatusNotFound
			record.responseHeaders = w.Header().Clone()
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	daemonURL, err := url.Parse(daemon.URL)
	if err != nil {
		t.Fatal(err)
	}
	daemonTransport := &http.Transport{Proxy: nil}
	t.Cleanup(daemonTransport.CloseIdleConnections)
	ctx, relay, principalCalls := startDockerBrokerTestRelay(t, dockerproxy.New(daemonURL, daemonTransport))
	configDir := t.TempDir()
	env := []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"PATH=" + os.Getenv("PATH"),
		"TEMP=" + configDir,
		"TMP=" + configDir,
		"DOCKER_CONFIG=" + configDir,
		"DOCKER_API_VERSION=1.45",
	}
	runDocker := func(stdin []byte, args ...string) (int, string, string, error) {
		t.Helper()
		commandCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		commandArgs := []string{"--config", configDir, "--host", relay.Endpoint(), "exec"}
		commandArgs = append(commandArgs, args...)
		cmd := osexec.CommandContext(commandCtx, dockerPath, commandArgs...)
		cmd.Env = env
		cmd.Stdin = bytes.NewReader(stdin)
		cmd.Stdout = new(bytes.Buffer)
		cmd.Stderr = new(bytes.Buffer)
		stdout := cmd.Stdout.(*bytes.Buffer)
		stderr := cmd.Stderr.(*bytes.Buffer)
		err := cmd.Run()
		if err == nil {
			return 0, stdout.String(), stderr.String(), nil
		}
		if commandCtx.Err() != nil {
			return -1, stdout.String(), stderr.String(), fmt.Errorf("Docker CLI command context ended: %w", commandCtx.Err())
		}
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return -1, stdout.String(), stderr.String(), err
	}

	t.Run("non-interactive 101 stream", func(t *testing.T) {
		code, stdout, stderr, err := runDocker(nil, "test-container", "cmd.exe", "/c", "fast-exit")
		if err != nil || code != 0 || stdout != "docker-cli-stdout\n" || stderr != "docker-cli-stderr\n" {
			t.Fatalf("docker exec output = code %d stdout %q stderr %q, err %v", code, stdout, stderr, err)
		}
	})
	t.Run("interactive stdin EOF 101 stream and nonzero exit", func(t *testing.T) {
		code, stdout, stderr, err := runDocker([]byte("docker-cli-stdin\n"), "-i", "test-container", "cmd.exe", "/c", "stdin-eof", "exit-seven")
		if err != nil || code != 7 || stdout != "docker-cli-stdin-eof-stdout\n" || stderr != "docker-cli-stdin-eof-stderr\n" {
			t.Fatalf("docker exec -i output = code %d stdout %q stderr %q, err %v", code, stdout, stderr, err)
		}
	})
	t.Run("detached 200 response", func(t *testing.T) {
		code, stdout, stderr, err := runDocker(nil, "-d", "test-container", "cmd.exe", "/c", "detached")
		if err != nil || code != 0 || stdout != "" || stderr != "" {
			t.Fatalf("docker exec -d output = code %d stdout %q stderr %q, err %v", code, stdout, stderr, err)
		}
	})

	requestsMu.Lock()
	recorded := append([]dockerRequest(nil), requests...)
	requestsMu.Unlock()
	if principalCalls.Load() != int32(len(recorded)) {
		t.Fatalf("broker authorization count = %d, daemon request count = %d", principalCalls.Load(), len(recorded))
	}
	var (
		createRequests []dockerRequest
		attachedStart  *dockerRequest
		detachedStart  *dockerRequest
		foundInspect   bool
	)
	for i := range recorded {
		record := &recorded[i]
		switch {
		case record.method == http.MethodGet && strings.HasSuffix(record.uri, "/containers/test-container/json") && record.status == http.StatusOK:
			foundInspect = true
		case record.method == http.MethodPost && strings.HasSuffix(record.uri, "/containers/test-container/exec") && record.status == http.StatusCreated:
			createRequests = append(createRequests, *record)
		case record.method == http.MethodPost && strings.HasPrefix(record.uri, "/v1.45/exec/") && strings.HasSuffix(record.uri, "/start"):
			if record.status == http.StatusSwitchingProtocols {
				attachedStart = record
			} else if record.status == http.StatusOK {
				detachedStart = record
			}
		}
	}
	if !foundInspect || len(createRequests) != 3 || attachedStart == nil || detachedStart == nil {
		t.Fatalf("fake Docker API coverage: inspect=%v creates=%d attached-start=%v detached-start=%v", foundInspect, len(createRequests), attachedStart != nil, detachedStart != nil)
	}
	if !strings.Contains(strings.ToLower(attachedStart.headers.Get("Connection")), "upgrade") || attachedStart.headers.Get("Upgrade") != "tcp" || attachedStart.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("stock CLI 101 request headers = %#v", attachedStart.headers)
	}
	if attachedStart.responseHeaders.Get("Connection") != "Upgrade" || attachedStart.responseHeaders.Get("Upgrade") != "tcp" || attachedStart.responseHeaders.Get("Content-Type") != "application/vnd.docker.multiplexed-stream" {
		t.Fatalf("fake 101 response headers = %#v", attachedStart.responseHeaders)
	}
	if detachedStart.headers.Get("Upgrade") != "" || detachedStart.responseHeaders.Get("Connection") != "" || detachedStart.responseHeaders.Get("Upgrade") != "" {
		t.Fatalf("stock CLI detached 200 request/response unexpectedly upgraded: request=%#v response=%#v", detachedStart.headers, detachedStart.responseHeaders)
	}
	var stdinExec, noStdinExec execConfig
	if err := json.Unmarshal(createRequests[0].body, &noStdinExec); err != nil {
		t.Fatalf("decode no-i create request: %v", err)
	}
	if err := json.Unmarshal(createRequests[1].body, &stdinExec); err != nil {
		t.Fatalf("decode -i create request: %v", err)
	}
	if noStdinExec.AttachStdin || !noStdinExec.AttachStdout || !noStdinExec.AttachStderr || noStdinExec.TTY {
		t.Fatalf("stock no-i exec create options = %+v", noStdinExec)
	}
	if !stdinExec.AttachStdin || !stdinExec.AttachStdout || !stdinExec.AttachStderr || stdinExec.TTY {
		t.Fatalf("stock -i exec create options = %+v", stdinExec)
	}
	var detachedBody struct {
		Detach bool `json:"Detach"`
		TTY    bool `json:"Tty"`
	}
	if err := json.Unmarshal(detachedStart.body, &detachedBody); err != nil || !detachedBody.Detach || detachedBody.TTY {
		t.Fatalf("stock detached exec start body = %s (decode error %v)", detachedStart.body, err)
	}
}
