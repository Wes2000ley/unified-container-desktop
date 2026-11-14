package engine

import (
	"bytes"
	"context"
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
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

func installTestWindowsFileTool(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("Windows utility tests require a Windows host")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(filepath.Dir(exe), "ucd-filetool.exe")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if errors.Is(err, os.ErrExist) {
		t.Skip("packaged file utility exists beside test binary")
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("test utility")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(name) })
}

func TestPrepareWindowsFilesRejectsPausedContainer(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed=%t", confirmed), func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				p := dockerTestPath(w, r)
				if p == "" {
					return
				}
				mu.Lock()
				requests = append(requests, r.Method+" "+p)
				mu.Unlock()
				if p == "/containers/c/json" && r.Method == http.MethodGet {
					fmt.Fprint(w, `{"State":{"Running":true,"Paused":true}}`)
					return
				}
				http.Error(w, "unexpected Docker operation", http.StatusInternalServerError)
			})
			b.id = "windows"
			_, err := call(t, b, "container.files.prepare", api.ContainerFileParams{ID: "c", Confirmed: confirmed})
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_state" || !strings.Contains(apiErr.Message, "unpause") {
				t.Fatalf("expected actionable paused-state error, got %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0] != "GET /containers/c/json" {
				t.Fatalf("paused container received unexpected operations: %v", requests)
			}
		})
	}
}

func TestPrepareWindowsFilesRestoresContainerState(t *testing.T) {
	installTestWindowsFileTool(t)
	cases := []struct {
		name                          string
		running, confirmed            bool
		cancelBefore, cancelAfterStop bool
		failStop, failCopy, failStart bool
		failSnapshot                  bool
		wantStop, wantCopy, wantStart int
		wantError                     string
	}{
		{name: "confirmation required", running: true, wantError: "requires stopping"},
		{name: "running", running: true, confirmed: true, wantStop: 1, wantCopy: 1, wantStart: 1},
		{name: "stopped", confirmed: true, wantCopy: 1},
		{name: "already canceled", running: true, confirmed: true, cancelBefore: true, wantError: "canceled"},
		{name: "canceled after stop", running: true, confirmed: true, cancelAfterStop: true, wantStop: 1, wantCopy: 1, wantStart: 1},
		{name: "stop failure attempts start", running: true, confirmed: true, failStop: true, wantStop: 1, wantStart: 1, wantError: "stop container"},
		{name: "copy failure restarts", running: true, confirmed: true, failCopy: true, wantStop: 1, wantCopy: 1, wantStart: 1, wantError: "copy file utility"},
		{name: "restart failure is reported", running: true, confirmed: true, failStart: true, wantStop: 1, wantCopy: 1, wantStart: 1, wantError: "restart container"},
		{name: "copy and restart failures reported", running: true, confirmed: true, failCopy: true, failStart: true, wantStop: 1, wantCopy: 1, wantStart: 1, wantError: "restart container"},
		{name: "port reconciliation failure restarts", running: true, confirmed: true, failSnapshot: true, wantStop: 1, wantStart: 1, wantError: "reconcile published ports"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			var mu sync.Mutex
			var stop, copyCount, start, snapshots int
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch p := dockerTestPath(w, r); p {
				case "":
				case "/containers/c/json":
					fmt.Fprintf(w, `{"State":{"Running":%t}}`, tc.running)
				case "/containers/json":
					snapshots++
					if tc.failSnapshot {
						http.Error(w, "snapshot failed", http.StatusInternalServerError)
						return
					}
					fmt.Fprint(w, `[]`)
				case "/containers/c/stop":
					stop++
					if tc.cancelAfterStop {
						cancel()
					}
					if tc.failStop {
						http.Error(w, "stop failed", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case "/containers/c/archive":
					if r.Method == http.MethodGet {
						http.Error(w, "missing", http.StatusNotFound)
						return
					}
					copyCount++
					if tc.failCopy {
						http.Error(w, "copy failed", http.StatusInternalServerError)
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					w.WriteHeader(http.StatusOK)
				case "/containers/c/start":
					start++
					if tc.failStart {
						http.Error(w, "start failed", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			b.id = "windows"
			req, err := api.NewRequest("container.files.prepare", "windows", api.ContainerFileParams{ID: "c", Confirmed: tc.confirmed})
			if err != nil {
				t.Fatal(err)
			}
			got, err := b.Call(ctx, req)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.wantError) {
					t.Fatalf("expected %q error, got %v", tc.wantError, err)
				}
				if tc.failCopy && tc.failStart && !strings.Contains(err.Error(), "copy file utility") {
					t.Fatalf("lost copy error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if result := got.(api.ContainerFilePrepareResult); !result.Prepared || result.Restarted != tc.running {
				t.Fatalf("unexpected preparation result %#v", result)
			}
			mu.Lock()
			defer mu.Unlock()
			if stop != tc.wantStop || copyCount != tc.wantCopy || start != tc.wantStart {
				t.Fatalf("stop=%d copy=%d start=%d", stop, copyCount, start)
			}
			if snapshots != tc.wantStop+tc.wantStart {
				t.Fatalf("expected lifecycle port reconciliation for each action, got %d", snapshots)
			}
		})
	}
}

func TestWindowsFileToolStreamsBinaryAndReportsExit(t *testing.T) {
	installTestWindowsFileTool(t)
	payload := []byte{0, 1, 2, '\r', '\n', 255}
	for _, tc := range []struct {
		name, command string
		exitCode      int
		empty         bool
	}{
		{name: "download", command: "read"},
		{name: "upload", command: "write"},
		{name: "empty upload", command: "write", empty: true},
		{name: "utility error", command: "read", exitCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := payload
			if tc.empty {
				payload = nil
			}
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				switch p := dockerTestPath(w, r); p {
				case "":
				case "/containers/c/exec":
					var body struct {
						Cmd         []string
						User        string
						AttachStdin bool
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.User != "ContainerAdministrator" || len(body.Cmd) < 2 || body.Cmd[1] != tc.command || body.AttachStdin != (tc.command == "write") {
						t.Errorf("unexpected exec request: %#v", body)
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
					if tc.command == "write" {
						data := make([]byte, len(payload))
						if _, err := io.ReadFull(rw, data); err != nil || !bytes.Equal(data, payload) {
							t.Errorf("upload data %v, error %v", data, err)
						}
					} else {
						writeDockerFrame(conn, 1, payload)
						if tc.exitCode != 0 {
							writeDockerFrame(conn, 2, []byte("ucd-filetool: access denied"))
						}
					}
				case "/exec/file-exec/json":
					fmt.Fprintf(w, `{"ID":"file-exec","Running":false,"ExitCode":%d}`, tc.exitCode)
				default:
					t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			b.id = "windows"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.command == "write" {
				err := b.writeFileTool(ctx, "c", []string{"write", "C:/file", strconv.Itoa(len(payload)), "0"}, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			reader, done, err := b.openFileToolRead(ctx, "c", []string{"read", "C:/file"})
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(reader)
			_ = reader.Close()
			finalErr := <-done
			if !bytes.Equal(data, payload) {
				t.Fatalf("got %v, want %v", data, payload)
			}
			if tc.exitCode == 0 && (readErr != nil || finalErr != nil) {
				t.Fatalf("read %v; final %v", readErr, finalErr)
			}
			if tc.exitCode != 0 && (readErr == nil || finalErr == nil || !strings.Contains(finalErr.Error(), "access denied")) {
				t.Fatalf("utility error lost: read %v; final %v", readErr, finalErr)
			}
		})
	}
}

func TestWindowsFileToolInterruptedStreamsCloseConnection(t *testing.T) {
	installTestWindowsFileTool(t)
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			closed := make(chan struct{})
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				switch p := dockerTestPath(w, r); p {
				case "":
				case "/containers/c/exec":
					fmt.Fprint(w, `{"Id":"file-exec"}`)
				case "/exec/file-exec/start":
					_, _ = io.Copy(io.Discard, r.Body)
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
					_ = rw.Flush()
					_, _ = io.Copy(io.Discard, rw)
					close(closed)
				default:
					t.Errorf("unexpected Docker request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			b.id = "windows"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var done <-chan error
			if operation == "read" {
				reader, result, err := b.openFileToolRead(ctx, "c", []string{"read", "C:/file"})
				if err != nil {
					t.Fatal(err)
				}
				done = result
				_ = reader.Close()
			} else {
				writer, result, err := b.openFileToolWrite(ctx, "c", []string{"write", "C:/file", "6", "0"})
				if err != nil {
					t.Fatal(err)
				}
				done = result
				if _, err := writer.Write([]byte("abc")); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err == nil || !strings.Contains(err.Error(), "3 of 6") {
					t.Fatalf("expected short-input error, got %v", err)
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected canceled execution")
				}
			case <-ctx.Done():
				t.Fatal("timed out waiting for interrupted execution")
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("hijacked connection remained open")
			}
		})
	}
}

func TestFileToolWriterCloseUnblocksConcurrentWrite(t *testing.T) {
	reader, pipe := io.Pipe()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &fileToolWriter{
		PipeWriter: pipe, cancel: cancel, executionDone: make(chan struct{}), expected: 2,
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte{1, 2})
		writeDone <- err
	}()
	// Consume only the first byte. The same Write is now blocked until the
	// second byte is read or the upload is canceled.
	first := make([]byte, 1)
	if _, err := io.ReadFull(reader, first); err != nil || first[0] != 1 {
		t.Fatalf("read first byte %v: %v", first, err)
	}
	if _, err := writer.Write([]byte{3}); err == nil || !strings.Contains(err.Error(), "exceeds declared size") {
		t.Fatalf("concurrent extra write was accepted: %v", err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- writer.Close() }()
	select {
	case err := <-closeDone:
		if err == nil {
			t.Fatal("expected an incomplete upload error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close blocked behind the upload")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked upload succeeded after close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not unblock the upload")
	}
	if ctx.Err() == nil {
		t.Fatal("incomplete upload did not cancel the exec")
	}
}
