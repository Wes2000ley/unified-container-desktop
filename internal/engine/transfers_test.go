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
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

const largeTransferSize = 10<<20 + 137

// repeatReader generates a large payload without storing it in memory.
type repeatReader struct {
	remaining int64
}

type dockerFrameWriter struct{ io.Writer }

func (w dockerFrameWriter) Write(p []byte) (int, error) {
	var header [8]byte
	header[0] = 1
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	if _, err := w.Writer.Write(header[:]); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.remaining))
	for i := range p[:n] {
		p[i] = 0x5a
	}
	r.remaining -= int64(n)
	return n, nil
}

func transferCall(t *testing.T, b *Backend, method string, params any) any {
	t.Helper()
	got, err := call(t, b, method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return got
}

func TestImageTransferStreamsBeyondEightMiB(t *testing.T) {
	expected := sha256.New()
	_, _ = io.Copy(expected, &repeatReader{remaining: largeTransferSize})
	var loaded [32]byte
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/images/get":
			_, _ = io.Copy(w, &repeatReader{remaining: largeTransferSize})
		case "/images/load":
			hash := sha256.New()
			n, err := io.Copy(hash, r.Body)
			if err != nil || n != largeTransferSize {
				t.Errorf("read image load: %d %v", n, err)
			}
			copy(loaded[:], hash.Sum(nil))
			_, _ = fmt.Fprintln(w, `{"stream":"loaded\n"}`)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	download := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "image.save", References: []string{"test:latest"}}).(api.TransferOpenResult)
	hash := sha256.New()
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: download.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		if len(part.Data) > transferChunkSize {
			t.Fatal("download exceeded the chunk limit")
		}
		_, _ = hash.Write(part.Data)
		if part.EOF {
			break
		}
	}
	if !bytes.Equal(hash.Sum(nil), expected.Sum(nil)) {
		t.Fatal("download hash differs")
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: download.SessionID})
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: download.SessionID})

	upload := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "image.load"}).(api.TransferOpenResult)
	source := &repeatReader{remaining: largeTransferSize}
	buf := make([]byte, transferChunkSize)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: upload.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
	}
	result := transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: upload.SessionID}).(api.ResourceOutput)
	if !strings.Contains(result.Output, "loaded") || !bytes.Equal(loaded[:], expected.Sum(nil)) {
		t.Fatalf("image load output/hash mismatch: %q", result.Output)
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: upload.SessionID})
}

func TestImageBuildTransferStreamsBeyondEightMiB(t *testing.T) {
	var received atomic.Int64
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/build":
			tr := tar.NewReader(r.Body)
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("build context: %v", err)
					return
				}
				if header.Name == "large.bin" {
					n, err := io.Copy(io.Discard, tr)
					if err != nil {
						t.Errorf("build file: %v", err)
					}
					received.Store(n)
				}
			}
			_, _ = fmt.Fprintln(w, `{"stream":"build completed\n"}`)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{
		Operation: "image.build", Dockerfile: "FROM scratch\nCOPY large.bin /large.bin\n", Tags: []string{"test:latest"},
	}).(api.TransferOpenResult)
	input, output := io.Pipe()
	producer := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(output)
		err := tw.WriteHeader(&tar.Header{Name: "large.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: largeTransferSize})
		if err == nil {
			_, err = io.Copy(tw, &repeatReader{remaining: largeTransferSize})
		}
		if err == nil {
			err = tw.Close()
		}
		_ = output.CloseWithError(err)
		producer <- err
	}()
	buf := make([]byte, transferChunkSize)
	for {
		n, err := input.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-producer; err != nil {
		t.Fatal(err)
	}
	result := transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID}).(api.ResourceOutput)
	if received.Load() != largeTransferSize || !strings.Contains(result.Output, "build completed") {
		t.Fatalf("build transferred %d bytes, output %q", received.Load(), result.Output)
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
	files, err := filepath.Glob(filepath.Join(b.dataRoot, ".ucd-build-context-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary build files remain: %v %v", files, err)
	}
}

func TestStreamedImageBuildKeepsRegistrySnapshotAfterOpenRequestReturns(t *testing.T) {
	const secret = "synthetic-stream-build-secret"
	authReceived := make(chan error, 1)
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/build":
			data, err := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Config"))
			if err == nil {
				var configs map[string]struct {
					Username string `json:"username"`
					Password string `json:"password"`
				}
				err = json.Unmarshal(data, &configs)
				if err == nil {
					credential := configs["registry.example"]
					if credential.Username != "shared-user" || credential.Password != secret {
						err = fmt.Errorf("shared registry credentials missing from build request")
					}
				}
			}
			authReceived <- err
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintln(w, `{"stream":"build completed\n"}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	requestCtx = withRegistrySnapshot(requestCtx, api.RegistrySnapshot{AuthConfigs: map[string]api.RegistryAuth{
		"registry.example": {Username: "shared-user", Password: secret},
	}})
	request, err := api.NewRequest("transfer.open", "linux", api.TransferOpenParams{
		Operation: "image.build", Dockerfile: "FROM registry.example/base:latest\n", Tags: []string{"demo:latest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := b.Call(requestCtx, request)
	if err != nil {
		cancelRequest()
		t.Fatal(err)
	}
	up := opened.(api.TransferOpenResult)
	cancelRequest()
	b.transferMu.Lock()
	session := b.transfers[up.SessionID]
	b.transferMu.Unlock()
	if session == nil {
		t.Fatal("streamed build session was not registered")
	}
	if err := session.ctx.Err(); err != nil {
		t.Fatalf("request cancellation stopped the detached build session: %v", err)
	}
	if snapshot, ok := registrySnapshotFromContext(session.ctx); !ok || snapshot.AuthConfigs["registry.example"].Password != secret {
		t.Fatal("detached build session lost its operation-scoped registry snapshot")
	}

	var contextArchive bytes.Buffer
	tarWriter := tar.NewWriter(&contextArchive)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "app", Typeflag: tar.TypeReg, Mode: 0644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: contextArchive.Bytes()})
	if _, err := call(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID}); err != nil {
		t.Fatal(err)
	}
	if err := <-authReceived; err != nil {
		t.Fatal(err)
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
}

func TestImageTransferReportsDaemonFailure(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/images/load":
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = fmt.Fprintln(w, `{"error":"injected image failure"}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "image.load"}).(api.TransferOpenResult)
	transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: []byte("input")})
	if _, err := call(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID}); err == nil || !strings.Contains(err.Error(), "injected image failure") {
		t.Fatalf("daemon failure not reported: %v", err)
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
}

func TestImageImportAndContainerExport(t *testing.T) {
	var imported atomic.Int64
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/images/create":
			if r.URL.Query().Get("fromSrc") != "-" || r.URL.Query().Get("repo") != "demo:latest" {
				t.Errorf("image import query: %v", r.URL.Query())
			}
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				t.Errorf("read image import: %v", err)
			}
			imported.Store(n)
			_, _ = fmt.Fprintln(w, `{"status":"imported"}`)
		case "/containers/c/export":
			_, _ = io.Copy(w, &repeatReader{remaining: largeTransferSize})
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "image.import", Reference: "demo:latest"}).(api.TransferOpenResult)
	transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: []byte("archive")})
	result := transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID}).(api.ResourceOutput)
	if imported.Load() != 7 || !strings.Contains(result.Output, "imported") {
		t.Fatalf("image import bytes=%d output=%q", imported.Load(), result.Output)
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
	down := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.export", ID: "c"}).(api.TransferOpenResult)
	var total int64
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: down.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		total += int64(len(part.Data))
		if part.EOF {
			break
		}
	}
	if total != largeTransferSize || down.Name != "container.tar" {
		t.Fatalf("container export bytes=%d name=%q", total, down.Name)
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: down.SessionID})
}

func TestContainerFileTransferLargeAndExactSize(t *testing.T) {
	size := int64(largeTransferSize)
	var uploaded [32]byte
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/containers/c/archive":
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("path") == "/tmp/new" {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"name":"large","size":%d,"mode":420}`, size))))
				tw := tar.NewWriter(w)
				_ = tw.WriteHeader(&tar.Header{Name: "large", Typeflag: tar.TypeReg, Mode: 0644, Size: size})
				_, _ = io.Copy(tw, &repeatReader{remaining: size})
				_ = tw.Close()
				return
			}
			if r.URL.Query().Get("copyUIDGID") != "true" {
				t.Error("container upload did not preserve archive ownership")
			}
			tr := tar.NewReader(r.Body)
			header, err := tr.Next()
			if err != nil || header.Name != "new" || header.Size != size {
				t.Errorf("file header %#v: %v", header, err)
				return
			}
			hash := sha256.New()
			n, err := io.Copy(hash, tr)
			if err != nil || n != size {
				t.Errorf("file bytes %d: %v", n, err)
			}
			copy(uploaded[:], hash.Sum(nil))
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	expected := sha256.New()
	_, _ = io.Copy(expected, &repeatReader{remaining: size})
	down := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.download", ID: "c", Path: "/tmp/large"}).(api.TransferOpenResult)
	if down.Size == nil || *down.Size != size {
		t.Fatalf("download size %v", down.Size)
	}
	hash := sha256.New()
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: down.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		_, _ = hash.Write(part.Data)
		if part.EOF {
			break
		}
	}
	if !bytes.Equal(hash.Sum(nil), expected.Sum(nil)) {
		t.Fatal("download content differs")
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: down.SessionID})

	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.upload", ID: "c", Path: "/tmp/new", Size: &size}).(api.TransferOpenResult)
	if _, err := call(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID}); err == nil {
		t.Fatal("accepted short file")
	}
	source := &repeatReader{remaining: size}
	buf := make([]byte, transferChunkSize)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
	}
	if _, err := call(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: []byte{1}}); err == nil {
		t.Fatal("accepted file larger than declared size")
	}
	transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID})
	if !bytes.Equal(uploaded[:], expected.Sum(nil)) {
		t.Fatal("uploaded file content differs")
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
}

func TestWindowsFileTransfersUseNativeExec(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows utility runs on the Windows engine host")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(filepath.Dir(executable), "ucd-filetool.exe")
	if _, err := os.Stat(toolPath); err == nil {
		t.Skip("packaged file utility already exists next to test binary")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolPath, []byte("test utility"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(toolPath) })
	type command struct {
		args []string
		fail bool
	}
	var commands sync.Map
	var sequence atomic.Int64
	var failRead atomic.Bool
	var blockRead atomic.Bool
	blocked := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	var uploaded atomic.Int64
	var uploadedHash atomic.Value
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		requestPath := dockerTestPath(w, r)
		switch {
		case requestPath == "":
		case requestPath == "/containers/c/archive":
			t.Error("used the Docker archive API for a running Windows container")
			http.Error(w, "archive forbidden", http.StatusInternalServerError)
		case requestPath == "/containers/c/exec":
			var body struct {
				Cmd  []string
				User string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Cmd) < 2 {
				t.Errorf("invalid utility command: %#v %v", body, err)
				return
			}
			if body.User != "ContainerAdministrator" {
				t.Errorf("Windows utility user = %q", body.User)
			}
			id := strconv.FormatInt(sequence.Add(1), 10)
			commands.Store(id, command{args: body.Cmd, fail: body.Cmd[1] == "read" && failRead.Load()})
			_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
		case strings.HasPrefix(requestPath, "/exec/"):
			pieces := strings.Split(requestPath, "/")
			if len(pieces) != 4 {
				http.Error(w, "invalid exec", http.StatusBadRequest)
				return
			}
			value, ok := commands.Load(pieces[2])
			if !ok {
				http.Error(w, "missing exec", http.StatusNotFound)
				return
			}
			cmd := value.(command)
			if pieces[3] == "json" {
				code := 0
				if cmd.fail {
					code = 23
				}
				_, _ = fmt.Fprintf(w, `{"Running":false,"ExitCode":%d}`, code)
				return
			}
			if pieces[3] != "start" {
				http.Error(w, "unexpected exec", http.StatusNotFound)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
			_ = rw.Flush()
			switch cmd.args[1] {
			case "stat":
				writeDockerFrame(conn, 1, []byte(fmt.Sprintf(`{"type":"file","size":%d}`, largeTransferSize)))
			case "read":
				if blockRead.Load() {
					blocked <- struct{}{}
					_, _ = io.Copy(io.Discard, rw)
					disconnected <- struct{}{}
					return
				}
				if cmd.fail {
					writeDockerFrame(conn, 1, []byte("partial"))
					writeDockerFrame(conn, 2, []byte("injected utility failure"))
					return
				}
				_, _ = io.Copy(dockerFrameWriter{Writer: conn}, &repeatReader{remaining: largeTransferSize})
			case "archive":
				tw := tar.NewWriter(dockerFrameWriter{Writer: conn})
				_ = tw.WriteHeader(&tar.Header{Name: "file.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: largeTransferSize})
				_, _ = io.Copy(tw, &repeatReader{remaining: largeTransferSize})
				_ = tw.Close()
			case "write":
				if len(cmd.args) != 5 || cmd.args[4] != "0" {
					t.Errorf("unexpected write arguments: %v", cmd.args)
					return
				}
				size, err := strconv.ParseInt(cmd.args[3], 10, 64)
				if err != nil {
					t.Error(err)
					return
				}
				hash := sha256.New()
				n, err := io.CopyN(hash, rw, size)
				if err != nil {
					t.Errorf("read native write stream: %v", err)
				}
				var digest [32]byte
				copy(digest[:], hash.Sum(nil))
				uploadedHash.Store(digest)
				uploaded.Store(n)
			default:
				t.Errorf("unexpected file utility operation %q", cmd.args[1])
			}
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	b.id = "windows"
	expected := sha256.New()
	_, _ = io.Copy(expected, &repeatReader{remaining: largeTransferSize})
	down := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.download", ID: "c", Path: "C:/tmp/file.bin"}).(api.TransferOpenResult)
	if down.Size == nil || *down.Size != largeTransferSize {
		t.Fatalf("native download size %v", down.Size)
	}
	hash := sha256.New()
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: down.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		_, _ = hash.Write(part.Data)
		if part.EOF {
			break
		}
	}
	if !bytes.Equal(hash.Sum(nil), expected.Sum(nil)) {
		t.Fatal("native download hash differs")
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: down.SessionID})
	size := int64(largeTransferSize)
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.upload", ID: "c", Path: "C:/tmp/file.bin", Size: &size}).(api.TransferOpenResult)
	input := &repeatReader{remaining: size}
	buf := make([]byte, transferChunkSize)
	for {
		n, err := input.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
	}
	transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID})
	var expectedHash [32]byte
	copy(expectedHash[:], expected.Sum(nil))
	if uploaded.Load() != size || uploadedHash.Load() != expectedHash {
		t.Fatal("native upload hash differs")
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
	archiveSession := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.archive", ID: "c", Path: "C:/tmp/file.bin"}).(api.TransferOpenResult)
	archiveFile, err := os.CreateTemp(t.TempDir(), "container-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer archiveFile.Close()
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: archiveSession.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		if _, err := archiveFile.Write(part.Data); err != nil {
			t.Fatal(err)
		}
		if part.EOF {
			break
		}
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: archiveSession.SessionID})
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(archiveFile)
	header, err := archive.Next()
	if err != nil || header.Name != "file.bin" {
		t.Fatalf("native archive header %#v: %v", header, err)
	}
	archiveHash := sha256.New()
	if _, err := io.Copy(archiveHash, archive); err != nil || !bytes.Equal(archiveHash.Sum(nil), expected.Sum(nil)) {
		t.Fatalf("native archive content differs: %v", err)
	}
	failRead.Store(true)
	broken := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.download", ID: "c", Path: "C:/tmp/file.bin"}).(api.TransferOpenResult)
	for {
		_, err := call(t, b, "transfer.read", api.TransferParams{SessionID: broken.SessionID, MaxBytes: transferChunkSize})
		if err != nil {
			if !strings.Contains(err.Error(), "injected utility failure") {
				t.Fatalf("utility failure lost: %v", err)
			}
			break
		}
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: broken.SessionID})
	failRead.Store(false)
	blockRead.Store(true)
	blockedTransfer := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "container.files.download", ID: "c", Path: "C:/tmp/file.bin"}).(api.TransferOpenResult)
	readResult := make(chan error, 1)
	go func() {
		req, _ := api.NewRequest("transfer.read", b.id, api.TransferParams{SessionID: blockedTransfer.SessionID, MaxBytes: transferChunkSize})
		_, err := b.Call(context.Background(), req)
		readResult <- err
	}()
	<-blocked
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: blockedTransfer.SessionID})
	if err := <-readResult; err == nil {
		t.Fatal("canceled Windows file read returned success")
	}
	<-disconnected
}

func TestVolumeFileTransfersKeepUtilityUntilFinished(t *testing.T) {
	const size = 1024*1024 + 17
	var removed atomic.Int32
	var uploaded atomic.Int64
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/volumes/data":
			_, _ = fmt.Fprint(w, `{"Name":"data","Driver":"local"}`)
		case "/images/busybox:1.37.0/json":
			_, _ = fmt.Fprint(w, `{"Id":"sha256:abc"}`)
		case "/containers/create":
			_, _ = fmt.Fprint(w, `{"Id":"helper1"}`)
		case "/containers/helper1/start":
			w.WriteHeader(http.StatusNoContent)
		case "/containers/helper1/archive":
			if r.Method == http.MethodPut {
				if r.URL.Query().Get("copyUIDGID") != "true" {
					t.Error("volume upload did not preserve archive ownership")
				}
				tw := tar.NewReader(r.Body)
				header, err := tw.Next()
				if err != nil || header.Name != "new.bin" || header.Size != size {
					t.Errorf("uploaded file header %#v: %v", header, err)
					return
				}
				n, err := io.Copy(io.Discard, tw)
				if err != nil {
					t.Errorf("read uploaded volume file: %v", err)
				}
				uploaded.Store(n)
				return
			}
			if r.URL.Query().Get("path") == "/ucd/new.bin" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"name":"old.bin","size":%d,"mode":420}`, size))))
			writer := tar.NewWriter(w)
			_ = writer.WriteHeader(&tar.Header{Name: "old.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: size})
			_, _ = io.Copy(writer, &repeatReader{remaining: size})
			_ = writer.Close()
		case "/containers/helper1":
			removed.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	down := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.files.download", Name: "data", Path: "old.bin"}).(api.TransferOpenResult)
	if down.Size == nil || *down.Size != size {
		t.Fatalf("volume file size = %v", down.Size)
	}
	var downloaded int64
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: down.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		downloaded += int64(len(part.Data))
		if part.EOF {
			break
		}
	}
	if downloaded != size || removed.Load() != 1 {
		t.Fatalf("downloaded %d bytes, removed %d helpers", downloaded, removed.Load())
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: down.SessionID})
	uploadSize := int64(size)
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.files.upload", Name: "data", Path: "new.bin", Size: &uploadSize}).(api.TransferOpenResult)
	buf := make([]byte, transferChunkSize)
	src := &repeatReader{remaining: size}
	for {
		n, err := src.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
	}
	transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID})
	if uploaded.Load() != size || removed.Load() != 2 {
		t.Fatalf("uploaded %d bytes, removed %d helpers", uploaded.Load(), removed.Load())
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
}

func archiveWithHeaders(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var data bytes.Buffer
	w := tar.NewWriter(&data)
	for _, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.CopyN(w, &repeatReader{remaining: h.Size}, h.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestVolumeRestoreValidatesBeforeMutationAndCleansSpool(t *testing.T) {
	var mutations atomic.Int32
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/volumes/data":
			_, _ = fmt.Fprint(w, `{"Name":"data","Driver":"local"}`)
		default:
			t.Logf("unexpected restore request %s %s", r.Method, r.URL)
			mutations.Add(1)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	if _, err := call(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.restore", Name: "data"}); err == nil {
		t.Fatal("restore did not require confirmation")
	}
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.restore", Name: "data", Confirmed: true}).(api.TransferOpenResult)
	data := archiveWithHeaders(t, &tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
	transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: data})
	if _, err := call(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID}); err == nil {
		t.Fatal("restore accepted traversal")
	}
	if mutations.Load() != 0 {
		t.Fatal("invalid archive reached the volume utility")
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
	files, err := filepath.Glob(filepath.Join(b.dataRoot, ".ucd-volume-restore-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary restore files remain: %v %v", files, err)
	}
	if runtime.GOOS == "windows" {
		b.id = "windows"
		up = transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.restore", Name: "data", Confirmed: true}).(api.TransferOpenResult)
		data = archiveWithHeaders(t, &tar.Header{Name: "CON.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
		transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: data})
		_, err := call(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID})
		if err == nil {
			t.Fatal("restore accepted a Windows-reserved path")
		}
		if mutations.Load() != 0 {
			t.Fatalf("invalid Windows archive reached the volume utility: %v", err)
		}
		transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
	}
}

func TestVolumeRestoreStreamsValidatedLargeArchive(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	toolName, engineID, utilityImage, root := "ucd-filetool", "linux", "busybox:1.37.0", "/ucd"
	if runtime.GOOS == "windows" {
		toolName, engineID, utilityImage, root = "ucd-filetool.exe", "windows", "mcr.microsoft.com/windows/servercore:ltsc2025", `C:\ucd`
	}
	toolPath := filepath.Join(filepath.Dir(executable), toolName)
	if _, err := os.Stat(toolPath); err == nil {
		t.Skip("packaged file utility already exists next to test binary")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(toolPath, []byte("test utility"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(toolPath) })
	var restored, removed, volumeUploaded atomic.Int64
	var restoredHash atomic.Value
	var started atomic.Bool
	var commandMu sync.Mutex
	var command []string
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/volumes/data":
			_, _ = fmt.Fprint(w, `{"Name":"data","Driver":"local"}`)
		case "/info":
			_, _ = fmt.Fprint(w, `{"KernelVersion":"10.0.26100.123"}`)
		case "/images/" + utilityImage + "/json":
			_, _ = fmt.Fprint(w, `{"Id":"sha256:abc"}`)
		case "/containers/create":
			started.Store(false)
			_, _ = fmt.Fprint(w, `{"Id":"helper1"}`)
		case "/containers/helper1/start":
			started.Store(true)
			w.WriteHeader(http.StatusNoContent)
		case "/containers/helper1/archive":
			if engineID == "windows" && started.Load() {
				t.Error("used Docker archive API after the Windows utility container started")
				http.Error(w, "archive API is unavailable after start", http.StatusInternalServerError)
				return
			}
			if r.Method == http.MethodGet {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if r.URL.Query().Get("path") != strings.ReplaceAll(root, `\`, "/") {
				_, _ = io.Copy(io.Discard, r.Body) // file utility upload
				return
			}
			tr := tar.NewReader(r.Body)
			header, err := tr.Next()
			if err != nil || header.Name != "large.bin" || header.Size != largeTransferSize {
				t.Errorf("restore header %#v: %v", header, err)
				return
			}
			hash := sha256.New()
			n, err := io.Copy(hash, tr)
			if err != nil {
				t.Errorf("restore content: %v", err)
			}
			var digest [32]byte
			copy(digest[:], hash.Sum(nil))
			restoredHash.Store(digest)
			restored.Store(n)
		case "/containers/helper1/exec":
			var input struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			commandMu.Lock()
			command = input.Cmd
			commandMu.Unlock()
			_, _ = fmt.Fprint(w, `{"Id":"file-exec"}`)
		case "/exec/file-exec/start":
			_, _ = io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
			_ = rw.Flush()
			commandMu.Lock()
			current := append([]string(nil), command...)
			commandMu.Unlock()
			if len(current) >= 2 && current[1] == "stat" {
				writeDockerFrame(conn, 1, []byte(fmt.Sprintf(`{"type":"file","size":%d}`, largeTransferSize)))
				return
			}
			if len(current) >= 2 && current[1] == "read" {
				_, _ = io.Copy(dockerFrameWriter{Writer: conn}, &repeatReader{remaining: largeTransferSize})
				return
			}
			if len(current) >= 5 && current[1] == "write" {
				size, err := strconv.ParseInt(current[3], 10, 64)
				if err != nil || current[4] != "0" {
					t.Errorf("unexpected volume write arguments %v: %v", current, err)
					return
				}
				n, err := io.CopyN(io.Discard, rw, size)
				if err != nil {
					t.Errorf("read volume file upload: %v", err)
				}
				volumeUploaded.Store(n)
				return
			}
			if len(current) >= 2 && current[1] == "archive" {
				writer := tar.NewWriter(dockerFrameWriter{Writer: conn})
				if err := writer.WriteHeader(&tar.Header{Name: "ucd/large.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: largeTransferSize}); err != nil {
					t.Error(err)
					return
				}
				if _, err := io.Copy(writer, &repeatReader{remaining: largeTransferSize}); err != nil {
					t.Error(err)
					return
				}
				if err := writer.Close(); err != nil {
					t.Error(err)
				}
				return
			}
			if len(current) >= 4 && current[1] == "extract" {
				size, err := strconv.ParseInt(current[3], 10, 64)
				if err != nil {
					t.Error(err)
					return
				}
				input := &io.LimitedReader{R: rw, N: size}
				tr := tar.NewReader(input)
				header, err := tr.Next()
				if err != nil || header.Name != "large.bin" || header.Size != largeTransferSize {
					t.Errorf("restore header %#v: %v", header, err)
					return
				}
				hash := sha256.New()
				n, err := io.Copy(hash, tr)
				if err != nil {
					t.Errorf("restore content: %v", err)
				}
				if _, err := io.Copy(io.Discard, input); err != nil {
					t.Errorf("restore archive trailer: %v", err)
				}
				var digest [32]byte
				copy(digest[:], hash.Sum(nil))
				restoredHash.Store(digest)
				restored.Store(n)
				return
			}
			writeDockerFrame(conn, 1, []byte(`{"entries":[]}`))
		case "/exec/file-exec/json":
			_, _ = fmt.Fprint(w, `{"ID":"file-exec","Running":false,"ExitCode":0}`)
		case "/containers/helper1":
			removed.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	b.id = engineID
	up := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.restore", Name: "data", Confirmed: true}).(api.TransferOpenResult)
	input, output := io.Pipe()
	producer := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(output)
		err := tw.WriteHeader(&tar.Header{Name: "large.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: largeTransferSize})
		if err == nil {
			_, err = io.Copy(tw, &repeatReader{remaining: largeTransferSize})
		}
		if err == nil {
			err = tw.Close()
		}
		_ = output.CloseWithError(err)
		producer <- err
	}()
	buf := make([]byte, transferChunkSize)
	for {
		n, err := input.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: up.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-producer; err != nil {
		t.Fatal(err)
	}
	transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: up.SessionID})
	expected := sha256.New()
	_, _ = io.Copy(expected, &repeatReader{remaining: largeTransferSize})
	var expectedHash [32]byte
	copy(expectedHash[:], expected.Sum(nil))
	if restored.Load() != largeTransferSize || restoredHash.Load() != expectedHash || removed.Load() != 1 {
		t.Fatalf("restored %d bytes, removed %d helpers", restored.Load(), removed.Load())
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: up.SessionID})
	files, err := filepath.Glob(filepath.Join(b.dataRoot, ".ucd-volume-restore-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary restore files remain: %v %v", files, err)
	}
	if engineID != "windows" {
		return
	}
	down := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.backup", Name: "data"}).(api.TransferOpenResult)
	backup, err := os.CreateTemp(t.TempDir(), "volume-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: down.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		if _, err := backup.Write(part.Data); err != nil {
			t.Fatal(err)
		}
		if part.EOF {
			break
		}
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: down.SessionID})
	if _, err := backup.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(backup)
	header, err := archive.Next()
	if err != nil || header.Name != "large.bin" {
		t.Fatalf("backup header %#v: %v", header, err)
	}
	backupHash := sha256.New()
	if _, err := io.Copy(backupHash, archive); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupHash.Sum(nil), expected.Sum(nil)) || removed.Load() != 2 {
		t.Fatalf("backup content differs or helpers not removed: %d", removed.Load())
	}
	fileSize := int64(largeTransferSize)
	fileUpload := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.files.upload", Name: "data", Path: "new.bin", Size: &fileSize}).(api.TransferOpenResult)
	fileInput := &repeatReader{remaining: fileSize}
	for {
		n, err := fileInput.Read(buf)
		if n > 0 {
			transferCall(t, b, "transfer.write", api.TransferParams{SessionID: fileUpload.SessionID, Data: buf[:n]})
		}
		if err == io.EOF {
			break
		}
	}
	transferCall(t, b, "transfer.finish", api.TransferParams{SessionID: fileUpload.SessionID})
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: fileUpload.SessionID})
	if volumeUploaded.Load() != fileSize || removed.Load() != 3 {
		t.Fatalf("native volume file upload bytes=%d, removed=%d", volumeUploaded.Load(), removed.Load())
	}
	fileDownload := transferCall(t, b, "transfer.open", api.TransferOpenParams{Operation: "volume.files.download", Name: "data", Path: "old.bin"}).(api.TransferOpenResult)
	fileHash := sha256.New()
	for {
		part := transferCall(t, b, "transfer.read", api.TransferParams{SessionID: fileDownload.SessionID, MaxBytes: transferChunkSize}).(api.TransferReadResult)
		_, _ = fileHash.Write(part.Data)
		if part.EOF {
			break
		}
	}
	transferCall(t, b, "transfer.close", api.TransferParams{SessionID: fileDownload.SessionID})
	if !bytes.Equal(fileHash.Sum(nil), expected.Sum(nil)) || removed.Load() != 4 {
		t.Fatalf("native volume file download differs or helper not removed: %d", removed.Load())
	}
}

func TestVolumeArchiveSymlinkAndHardlinkSafety(t *testing.T) {
	safe := archiveWithHeaders(t,
		&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside/data"},
		&tar.Header{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		&tar.Header{Name: "copy", Typeflag: tar.TypeLink, Linkname: "dir/file"},
	)
	if err := validateVolumeArchiveStream(bytes.NewReader(safe)); err != nil {
		t.Fatalf("standalone symlink rejected: %v", err)
	}
	if err := validateVolumeArchiveStream(bytes.NewReader(safe[:len(safe)-512])); err == nil {
		t.Fatal("accepted an incomplete tar end marker")
	}
	for _, headers := range [][]*tar.Header{
		{{Name: "link/child", Typeflag: tar.TypeReg, Size: 1}, {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside"}},
		{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside"}, {Name: "link/child", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "hard", Typeflag: tar.TypeLink, Linkname: "../outside"}},
		{{Name: "hard", Typeflag: tar.TypeLink, Linkname: "later"}, {Name: "later", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "file", Typeflag: tar.TypeReg, Size: 1}, {Name: "file/child", Typeflag: tar.TypeReg, Size: 1}},
		{{Name: "pipe", Typeflag: tar.TypeFifo}},
	} {
		if err := validateVolumeArchiveStream(bytes.NewReader(archiveWithHeaders(t, headers...))); err == nil {
			t.Fatalf("accepted unsafe archive: %#v", headers)
		}
	}
}

func TestVolumeArchiveStreamingLarge(t *testing.T) {
	source, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(writer)
		err := tw.WriteHeader(&tar.Header{Name: "ucd/large", Typeflag: tar.TypeReg, Size: largeTransferSize, Mode: 0644})
		if err == nil {
			_, err = io.Copy(tw, &repeatReader{remaining: largeTransferSize})
		}
		if err == nil {
			err = tw.Close()
		}
		_ = writer.CloseWithError(err)
		done <- err
	}()
	file, err := os.CreateTemp(t.TempDir(), "backup-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := normalizeVolumeArchiveStream(source, file, "ucd"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := validateVolumeArchiveStream(file); err != nil {
		t.Fatalf("large backup rejected: %v", err)
	}
}

func TestVolumeBackupRejectsTruncatedSource(t *testing.T) {
	source := archiveWithHeaders(t, &tar.Header{Name: "ucd/file", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
	var output bytes.Buffer
	if err := normalizeVolumeArchiveStream(bytes.NewReader(source[:len(source)-512]), &output, "ucd"); err == nil {
		t.Fatal("accepted a truncated Docker volume archive")
	}
}

func TestTransferCancelClosesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	s := &transferSession{ctx: ctx, cancel: cancel, reader: reader}
	b := &Backend{}
	id, err := b.registerTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		req, _ := api.NewRequest("transfer.read", "linux", api.TransferParams{SessionID: id})
		_, err := b.transferCall(ctx, req)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; err == nil {
		t.Fatal("canceled read returned success")
	}
	b.removeTransfer(id)
	if _, err := writer.Write([]byte("x")); err == nil {
		t.Fatal("canceled transfer left its source open")
	}
	_ = writer.Close()
}

type failedTransferReader struct{ closed bool }

func (r *failedTransferReader) Read(p []byte) (int, error) {
	copy(p, "abc")
	return 3, errors.New("injected source failure")
}

func (r *failedTransferReader) Close() error {
	r.closed = true
	return nil
}

func TestTransferReturnsPartialDataBeforeSourceError(t *testing.T) {
	streamCtx, cancel := context.WithCancel(context.Background())
	reader := &failedTransferReader{}
	s := &transferSession{ctx: streamCtx, cancel: cancel, reader: reader}
	b := &Backend{}
	id, err := b.registerTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := api.NewRequest("transfer.read", "linux", api.TransferParams{SessionID: id, MaxBytes: 8})
	got, err := b.transferCall(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	part := got.(api.TransferReadResult)
	if string(part.Data) != "abc" || part.EOF || !reader.closed {
		t.Fatalf("partial transfer result %#v, closed=%v", part, reader.closed)
	}
	if _, err := b.transferCall(context.Background(), req); err == nil || !strings.Contains(err.Error(), "injected source failure") {
		t.Fatalf("stream error was lost: %v", err)
	}
	if err := b.removeTransfer(id); err != nil {
		t.Fatal(err)
	}
}

func TestTransferRejectsOversizeChunk(t *testing.T) {
	streamCtx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	s := &transferSession{ctx: streamCtx, cancel: cancel, writer: writer}
	b := &Backend{}
	id, err := b.registerTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := api.NewRequest("transfer.write", "linux", api.TransferParams{SessionID: id, Data: make([]byte, transferChunkSize+1)})
	if _, err := b.transferCall(context.Background(), req); err == nil {
		t.Fatal("accepted an oversized chunk")
	}
	if err := b.removeTransfer(id); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
}

func TestIdleTransferExpiresAndRemovesSpool(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "restore-*")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	streamCtx, cancel := context.WithCancel(context.Background())
	s := &transferSession{ctx: streamCtx, cancel: cancel, spool: file}
	b := &Backend{}
	id, err := b.registerTransfer(s)
	if err != nil {
		t.Fatal(err)
	}
	b.transferMu.Lock()
	s.timer.Stop()
	s.lastActive = time.Now().Add(-2 * transferIdleTime)
	b.transferMu.Unlock()
	b.expireTransfer(id, s)
	if _, err := b.useTransfer(id); err == nil {
		t.Fatal("idle transfer was not removed")
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("restore spool remains: %v", err)
	}
}
