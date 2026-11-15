package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

func writeDockerFrame(w io.Writer, stream byte, data []byte) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	_, _ = w.Write(header)
	_, _ = w.Write(data)
}

func dockerTestPath(w http.ResponseWriter, r *http.Request) string {
	if r.URL.Path == "/_ping" {
		w.Header().Set("API-Version", "1.45")
		w.Header().Set("OSType", "linux")
		w.WriteHeader(http.StatusOK)
		return ""
	}
	return strings.TrimPrefix(r.URL.Path, "/v1.45")
}

func TestContainerCreateUpdateAndLogs(t *testing.T) {
	var createBody map[string]json.RawMessage
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		path := dockerTestPath(w, r)
		w.Header().Set("Content-Type", "application/json")
		switch path {
		case "":
		case "/containers/create":
			if r.URL.Query().Get("name") != "web" {
				t.Errorf("create name %q", r.URL.RawQuery)
			}
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"Id":"abc","Warnings":["notice"]}`)
		case "/containers/abc/update":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["Memory"] != float64(1024) {
				t.Errorf("update body: %#v", body)
			}
			fmt.Fprint(w, `{"Warnings":[]}`)
		case "/containers/abc/json":
			fmt.Fprint(w, `{"Id":"abc","Config":{"Tty":false}}`)
		case "/containers/abc/logs":
			q := r.URL.Query()
			if q.Get("stdout") != "1" || q.Get("stderr") != "" || q.Get("timestamps") != "1" || q.Get("since") != "1767225600.000000000" || q.Get("tail") != "25" {
				t.Errorf("log options: %v", q)
			}
			writeDockerFrame(w, 1, []byte("hello\n"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	got, err := call(t, b, "container.create", map[string]any{"name": "web", "config": map[string]any{"Image": "example:1", "Cmd": []string{"echo", "hello"}}, "hostConfig": map[string]any{"Memory": 1024}, "networkingConfig": map[string]any{"EndpointsConfig": map[string]any{"test": map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.(api.ContainerCreateResult).ID != "abc" || len(createBody["HostConfig"]) == 0 || len(createBody["NetworkingConfig"]) == 0 {
		t.Fatalf("create result %#v body %#v", got, createBody)
	}
	if _, err = call(t, b, "container.update", map[string]any{"id": "abc", "resources": map[string]any{"Memory": 1024}}); err != nil {
		t.Fatal(err)
	}
	got, err = call(t, b, "container.logs", map[string]any{"id": "abc", "tail": 25, "timestamps": true, "since": "2026-01-01T00:00:00Z", "stdout": true, "stderr": false})
	if err != nil || got.(api.LogResult).Text != "hello\n" {
		t.Fatalf("logs %#v: %v", got, err)
	}
}

func readSessionUntil(t *testing.T, b *Backend, method, id string, wantEOF bool, wantBytes int) api.ExecReadResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var result api.ExecReadResult
	var data []byte
	for time.Now().Before(deadline) {
		got, err := call(t, b, method, api.ExecParams{SessionID: id, MaxBytes: 32768})
		if err != nil {
			t.Fatal(err)
		}
		result = got.(api.ExecReadResult)
		data = append(data, result.Data...)
		if (wantEOF && result.EOF) || (!wantEOF && len(data) >= wantBytes) {
			result.Data = data
			return result
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session timed out after %d bytes (eof=%v)", len(data), result.EOF)
	return result
}

func TestContainerFollowStreamsWithoutLoss(t *testing.T) {
	resume := make(chan struct{})
	large := bytes.Repeat([]byte("log-line\n"), 200000)
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		path := dockerTestPath(w, r)
		switch path {
		case "":
		case "/containers/c/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Id":"c","Config":{"Tty":false}}`)
		case "/containers/c/logs":
			if r.URL.Query().Get("follow") != "1" {
				t.Error("follow was not set")
			}
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
			writeDockerFrame(w, 1, []byte("first\n"))
			w.(http.Flusher).Flush()
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
			writeDockerFrame(w, 2, large)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	got, err := call(t, b, "container.logs.open", map[string]any{"id": "c"})
	if err != nil {
		t.Fatal(err)
	}
	id := got.(api.ExecOpenResult).SessionID
	first := readSessionUntil(t, b, "container.logs.read", id, false, 6)
	if string(first.Data) != "first\n" || first.EOF {
		t.Fatalf("first part: %#v", first)
	}
	close(resume)
	rest := readSessionUntil(t, b, "container.logs.read", id, true, 0)
	if !bytes.Equal(rest.Data, large) {
		t.Fatalf("received %d bytes, want %d", len(rest.Data), len(large))
	}
	if _, err := call(t, b, "container.logs.close", api.ExecParams{SessionID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestContainerLogSnapshotRejectsOversizedAndTruncatedStreams(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprintf("truncated=%v", truncated), func(t *testing.T) {
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				switch p := dockerTestPath(w, r); p {
				case "":
				case "/containers/c/json":
					fmt.Fprint(w, `{"Id":"c","Config":{"Tty":false}}`)
				case "/containers/c/logs":
					if truncated {
						_, _ = w.Write([]byte{1, 0, 0})
						return
					}
					writeDockerFrame(w, 1, bytes.Repeat([]byte("x"), (4<<20)+1))
				default:
					t.Errorf("unexpected %s", r.URL)
					http.Error(w, "unexpected", 500)
				}
			})
			if _, err := call(t, b, "container.logs", map[string]any{"id": "c"}); err == nil {
				t.Fatal("invalid stream was accepted")
			}
		})
	}
}

func TestCanceledSessionOpenClosesStream(t *testing.T) {
	b := &Backend{}
	request, cancelRequest := context.WithCancel(context.Background())
	stream, cancelStream, detach := newStreamContext(request)
	defer detach()
	id, err := b.addOutputSession(func() {}, cancelStream, func(io.Writer) error { <-stream.Done(); return stream.Err() }, nil, "", nil, "container.logs")
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if err := b.finishSessionOpen(request, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("open after cancel: %v", err)
	}
	b.sessionMu.Lock()
	remaining := len(b.sessions)
	b.sessionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("canceled open left %d sessions", remaining)
	}
}

func TestCanceledTerminalWriteUnblocks(t *testing.T) {
	b := &Backend{}
	client, peer := net.Pipe()
	defer peer.Close()
	stream, cancelStream := context.WithCancel(context.Background())
	id, err := b.addOutputSession(func() { _ = client.Close() }, cancelStream, func(io.Writer) error { <-stream.Done(); return stream.Err() }, nil, "exec", client, "container.exec")
	if err != nil {
		t.Fatal(err)
	}
	defer b.discardOutputSession(id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := api.NewRequest("container.exec.write", "linux", api.ExecParams{SessionID: id, Data: []byte("ab")})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := b.outputSessionCall(ctx, request); result <- err }()
	if _, err := io.ReadFull(peer, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-result; err == nil {
		t.Fatal("canceled blocked write succeeded")
	}
}

func TestContainerExecInteractivePTY(t *testing.T) {
	input := make(chan string, 1)
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		path := dockerTestPath(w, r)
		switch path {
		case "":
		case "/containers/c/exec":
			var body struct {
				Cmd              []string
				Tty, AttachStdin bool
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Cmd) != 2 || body.Cmd[0] != "sh" || body.Cmd[1] != "-i" || !body.Tty || !body.AttachStdin {
				t.Errorf("exec create: %#v", body)
			}
			fmt.Fprint(w, `{"Id":"exec1"}`)
		case "/exec/exec1/start":
			_, _ = io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\nready")
			if err := rw.Flush(); err != nil {
				t.Error(err)
				return
			}
			data := make([]byte, 5)
			if _, err := io.ReadFull(rw, data); err != nil {
				t.Error(err)
				return
			}
			input <- string(data)
			_, _ = conn.Write([]byte("complete"))
		case "/exec/exec1/resize":
			if r.URL.Query().Get("h") != "40" || r.URL.Query().Get("w") != "100" {
				t.Errorf("resize: %v", r.URL.Query())
			}
			w.WriteHeader(http.StatusOK)
		case "/exec/exec1/json":
			fmt.Fprint(w, `{"ID":"exec1","Running":false,"ExitCode":7}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	got, err := call(t, b, "container.exec.open", api.ExecParams{ID: "c", Command: []string{"sh", "-i"}, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	id := got.(api.ExecOpenResult).SessionID
	ready := readSessionUntil(t, b, "container.exec.read", id, false, 5)
	if string(ready.Data) != "ready" {
		t.Fatalf("terminal output %q", ready.Data)
	}
	if _, err := call(t, b, "container.logs.read", api.ExecParams{SessionID: id}); err == nil {
		t.Fatal("log reader accessed a terminal session")
	}
	if _, err := call(t, b, "container.logs.close", api.ExecParams{SessionID: id}); err == nil {
		t.Fatal("log closer accessed a terminal session")
	}
	if _, err := call(t, b, "container.exec.resize", api.ExecParams{SessionID: id, Rows: 40, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "container.exec.write", api.ExecParams{SessionID: id, Data: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	if got := <-input; got != "hello" {
		t.Fatalf("terminal input %q", got)
	}
	last := readSessionUntil(t, b, "container.exec.read", id, true, 0)
	if string(last.Data) != "complete" || last.ExitCode == nil || *last.ExitCode != 7 {
		t.Fatalf("terminal final result %#v", last)
	}
	if _, err := call(t, b, "container.exec.close", api.ExecParams{SessionID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestContainerCreateWindowsPorts(t *testing.T) {
	b := testBackend(t, windowsAliasFixture(t, func(w http.ResponseWriter, r *http.Request) {
		p := dockerTestPath(w, r)
		if p == "" {
			return
		}
		if p != "/containers/create" {
			t.Errorf("unexpected %s", r.URL)
			http.Error(w, "unexpected", 500)
			return
		}
		var body struct {
			Labels       map[string]string
			ExposedPorts map[string]json.RawMessage
			HostConfig   struct{ PortBindings map[string]json.RawMessage }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var ports []api.Port
		if err := json.Unmarshal([]byte(body.Labels[windowsPortsLabel]), &ports); err != nil {
			t.Error(err)
		}
		if len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || ports[0].Published != 8080 || ports[0].Target != 80 || ports[0].Protocol != "tcp" {
			t.Errorf("published metadata: %#v", ports)
		}
		if _, ok := body.ExposedPorts["80/tcp"]; !ok {
			t.Errorf("exposed ports: %#v", body.ExposedPorts)
		}
		if len(body.HostConfig.PortBindings) != 0 {
			t.Errorf("native Windows bindings were retained: %#v", body.HostConfig.PortBindings)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"Id":"win"}`)
	}, nil))
	b.id = "windows"
	_, err := call(t, b, "container.create", map[string]any{"config": map[string]any{"Image": "example"}, "hostConfig": map[string]any{"PortBindings": map[string]any{"80/tcp": []any{map[string]any{"HostPort": "8080"}}}}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContainerCreateAllExposedPortsDefaultToLoopback(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		p := dockerTestPath(w, r)
		switch p {
		case "":
		case "/images/example/json":
			fmt.Fprint(w, `{"Id":"sha256:test","Config":{"ExposedPorts":{"80/tcp":{},"53/udp":{}}}}`)
		case "/containers/create":
			var body struct {
				HostConfig struct {
					PublishAllPorts bool
					PortBindings    map[string][]struct {
						HostIP   string `json:"HostIp"`
						HostPort string
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.HostConfig.PublishAllPorts {
				t.Error("publish-all should be expressed as loopback bindings")
			}
			for _, key := range []string{"80/tcp", "53/udp"} {
				binding := body.HostConfig.PortBindings[key]
				if len(binding) != 2 || binding[0].HostIP != "127.0.0.1" || binding[0].HostPort != "0" ||
					binding[1].HostIP != "::1" || binding[1].HostPort != "0" {
					t.Errorf("binding %s: %#v", key, binding)
				}
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"Id":"linux"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	_, err := call(t, b, "container.create", map[string]any{"config": map[string]any{"Image": "example"}, "hostConfig": map[string]any{"PublishAllPorts": true}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContainerStatsDistinguishesMissingAndZero(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		p := dockerTestPath(w, r)
		if p == "" {
			return
		}
		if p != "/containers/c/stats" {
			t.Errorf("unexpected %s", r.URL)
			http.Error(w, "unexpected", 500)
			return
		}
		fmt.Fprint(w, `{"preread":"2026-01-01T00:00:00Z","read":"2026-01-01T00:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":200},"system_cpu_usage":1200,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000},"memory_stats":{"usage":1000,"limit":0},"pids_stats":{"current":0},"networks":{"eth0":{"rx_bytes":0}},"blkio_stats":{"io_service_bytes_recursive":[{"op":"Read","value":0},{"op":"Write","value":42}]}}`)
	})
	got, err := call(t, b, "container.stats", api.ContainerParams{ID: "c"})
	if err != nil {
		t.Fatal(err)
	}
	s := got.(api.Container)
	if s.MemoryLimitBytes == nil || *s.MemoryLimitBytes != 0 || s.Pids == nil || *s.Pids != 0 || s.NetworkRxBytes == nil || *s.NetworkRxBytes != 0 || s.NetworkTxBytes != nil || s.BlockReadBytes == nil || *s.BlockReadBytes != 0 || s.BlockWriteBytes == nil || *s.BlockWriteBytes != 42 {
		t.Fatalf("stats %#v", s)
	}
}

func TestContainerFileArchiveTransfers(t *testing.T) {
	var uploaded string
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		path := dockerTestPath(w, r)
		switch path {
		case "":
		case "/containers/c/archive":
			if r.Method == http.MethodPut {
				if r.URL.Query().Get("path") != "/tmp" {
					t.Errorf("destination: %v", r.URL.Query())
				}
				if r.URL.Query().Get("copyUIDGID") != "true" {
					t.Errorf("ownership preservation not requested: %v", r.URL.Query())
				}
				tr := tar.NewReader(r.Body)
				h, err := tr.Next()
				if err != nil || h.Name != "file.txt" {
					t.Errorf("tar header %#v: %v", h, err)
					return
				}
				if h.Mode != 0751 || h.Uid != 101 || h.Gid != 202 {
					t.Errorf("existing file metadata changed: mode=%o uid=%d gid=%d", h.Mode, h.Uid, h.Gid)
				}
				data, err := io.ReadAll(tr)
				if err != nil {
					t.Error(err)
				}
				uploaded = string(data)
				w.WriteHeader(http.StatusOK)
				return
			}
			stat := base64.StdEncoding.EncodeToString([]byte(`{"name":"file.txt","size":5,"mode":420,"mtime":"2026-01-01T00:00:00Z"}`))
			w.Header().Set("X-Docker-Container-Path-Stat", stat)
			tw := tar.NewWriter(w)
			_ = tw.WriteHeader(&tar.Header{Name: "file.txt", Mode: 0751, Uid: 101, Gid: 202, Size: 5})
			_, _ = tw.Write([]byte("hello"))
			_ = tw.Close()
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	if _, err := call(t, b, "container.files.write", api.ContainerFileParams{ID: "c", Path: "/tmp/file.txt", Data: []byte("upload")}); err != nil {
		t.Fatal(err)
	}
	if uploaded != "upload" {
		t.Fatalf("uploaded %q", uploaded)
	}
	got, err := call(t, b, "container.files.download", api.ContainerFileParams{ID: "c", Path: "/tmp/file.txt"})
	if err != nil || string(got.(api.BinaryResult).Data) != "hello" {
		t.Fatalf("download %#v: %v", got, err)
	}
	if _, err := call(t, b, "container.files.remove", api.ContainerFileParams{ID: "c", Path: "/", Recursive: true}); err == nil {
		t.Fatal("removed root")
	}
	if _, err := call(t, b, "container.files.upload", api.ContainerFileParams{ID: "c", Path: "/tmp/large", Data: make([]byte, maxContainerTransfer+1)}); err == nil {
		t.Fatal("accepted oversized upload")
	}
}

func TestContainerFileToolUsesNativeExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	toolName, engineID, sourcePath := "ucd-filetool", "linux", "/files/a $(literal)"
	destination := "/"
	if runtime.GOOS == "windows" {
		toolName, engineID, sourcePath = "ucd-filetool.exe", "windows", `C:/files/a $(literal)`
		destination = "C:/Windows/Temp"
	}
	toolPath := filepath.Join(filepath.Dir(executable), toolName)
	if _, err := os.Stat(toolPath); err == nil {
		t.Skip("packaged file utility already exists next to test binary")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	packaged := []byte("test utility")
	if err := os.WriteFile(toolPath, packaged, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(toolPath) })
	digest := sha256.Sum256(packaged)
	filename := fmt.Sprintf(".ucd-filetool-%x", digest[:8])
	if engineID == "windows" {
		filename += ".exe"
	}
	inside := strings.TrimSuffix(destination, "/") + "/" + filename
	type fileToolCase struct {
		name               string
		mode               os.FileMode
		content            []byte
		link               string
		missing, wantError bool
		wantUploads        int32
	}
	cases := []fileToolCase{
		{name: "missing", missing: true, wantUploads: 1},
		{name: "matching", mode: 0755, content: packaged},
		{name: "different content", mode: 0755, content: []byte("evil utility"), wantError: true},
		{name: "symlink", mode: os.ModeSymlink | 0777, link: "/other", wantError: true},
		{name: "directory", mode: os.ModeDir | 0755, wantError: true},
		{name: "special file", mode: os.ModeNamedPipe | 0600, wantError: true},
	}
	if engineID == "linux" {
		cases = append(cases, fileToolCase{name: "nonexecutable", mode: 0644, content: packaged, wantError: true})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var uploads, executions atomic.Int32
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				switch p := dockerTestPath(w, r); p {
				case "":
				case "/containers/c/archive":
					if r.Method == http.MethodGet {
						if r.URL.Query().Get("path") != inside {
							t.Errorf("utility inspection %s", r.URL.RawQuery)
						}
						if tc.missing {
							http.Error(w, "not found", http.StatusNotFound)
							return
						}
						stat, _ := json.Marshal(map[string]any{"name": filename, "size": len(tc.content), "mode": uint32(tc.mode), "linkTarget": tc.link})
						w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
						if tc.mode.IsRegular() {
							tw := tar.NewWriter(w)
							_ = tw.WriteHeader(&tar.Header{Name: filename, Mode: int64(tc.mode.Perm()), Size: int64(len(tc.content)), Typeflag: tar.TypeReg})
							_, _ = tw.Write(tc.content)
							_ = tw.Close()
						}
						return
					}
					uploads.Add(1)
					if r.Method != http.MethodPut || r.URL.Query().Get("path") != destination {
						t.Errorf("utility upload %s %s", r.Method, r.URL)
					}
					tr := tar.NewReader(r.Body)
					header, err := tr.Next()
					if err != nil {
						t.Error(err)
						return
					}
					data, _ := io.ReadAll(tr)
					if header.Name != filename || header.Mode != 0755 || !bytes.Equal(data, packaged) {
						t.Errorf("utility archive %#v: %q", header, data)
					}
					w.WriteHeader(http.StatusOK)
				case "/containers/c/exec":
					executions.Add(1)
					var body struct{ Cmd []string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if len(body.Cmd) != 3 || body.Cmd[0] != inside || body.Cmd[1] != "list" || body.Cmd[2] != sourcePath {
						t.Errorf("utility arguments %#v", body.Cmd)
					}
					fmt.Fprint(w, `{"Id":"file-exec"}`)
				case "/exec/file-exec/start":
					_, _ = io.Copy(io.Discard, r.Body)
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
					_ = rw.Flush()
					writeDockerFrame(conn, 1, []byte(`{"entries":[{"name":"x","path":"/x","type":"file","size":2,"modified":"2026-01-01T00:00:00Z"}]}`))
				case "/exec/file-exec/json":
					fmt.Fprint(w, `{"ID":"file-exec","Running":false,"ExitCode":0}`)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 500)
				}
			})
			b.id = engineID
			if engineID == "windows" {
				_, err := b.prepareFileTool(context.Background(), "c")
				if tc.wantError {
					if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") || uploads.Load() != 0 {
						t.Fatalf("expected collision without upload, got %v; uploads=%d", err, uploads.Load())
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := call(t, b, "container.files.list", api.ContainerFileParams{ID: "c", Path: sourcePath})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
					t.Fatalf("expected collision error, got %#v: %v", got, err)
				}
				if uploads.Load() != 0 || executions.Load() != 0 {
					t.Fatalf("collision caused upload=%d execute=%d", uploads.Load(), executions.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			list := got.(api.ContainerFileList)
			if len(list.Entries) != 1 || list.Entries[0].Name != "x" || uploads.Load() != tc.wantUploads || executions.Load() != 1 {
				t.Fatalf("file list %#v, uploads=%d, executions=%d", list, uploads.Load(), executions.Load())
			}
		})
	}
}
