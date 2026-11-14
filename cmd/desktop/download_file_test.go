package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

type testDownloadCaller struct {
	data      []byte
	offset    int
	reads     int
	closed    int
	failAfter int
	fail      error
	cancel    bool
	started   chan struct{}
}

func (c *testDownloadCaller) Call(ctx context.Context, request api.Request) (any, error) {
	if request.Engine != "linux" {
		return nil, errors.New("incorrect engine")
	}
	params, err := api.Decode[api.TransferParams](request)
	if err != nil || params.SessionID != "test-session" {
		return nil, errors.New("incorrect session")
	}
	switch request.Method {
	case "transfer.close":
		c.closed++
		return nil, nil
	case "transfer.read":
		if params.MaxBytes != 256<<10 {
			return nil, errors.New("incorrect chunk size")
		}
		c.reads++
		if c.cancel && c.reads == 2 {
			close(c.started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if c.fail != nil && c.reads > c.failAfter {
			return nil, c.fail
		}
		end := min(c.offset+params.MaxBytes, len(c.data))
		chunk := c.data[c.offset:end]
		c.offset = end
		return api.TransferReadResult{Data: chunk, EOF: end == len(c.data)}, nil
	default:
		return nil, errors.New("unexpected method")
	}
}

func TestDownloadStreamsLargeFileAndReplacesAtomically(t *testing.T) {
	data := bytes.Repeat([]byte("large-download-data"), (9<<20)/len("large-download-data")+1)
	caller := &testDownloadCaller{data: data}
	bridge := &Bridge{client: caller}
	dir := t.TempDir()
	destination := filepath.Join(dir, "archive.tar")
	if err := os.WriteFile(destination, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := bridge.download(context.Background(), "linux", "test-session", `../archive.tar`, func(_ context.Context, name string) (string, error) {
		if name != "archive.tar" {
			t.Errorf("suggested name = %q", name)
		}
		return destination, nil
	})
	if err != nil || selected != destination {
		t.Fatalf("download = %q, %v", selected, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, data) || caller.reads < 2 || caller.closed != 1 {
		t.Fatalf("downloaded %d bytes, reads %d, closes %d, error %v", len(got), caller.reads, caller.closed, err)
	}
	assertNoDownloadTempFiles(t, dir)
}

func TestDownloadFailurePreservesDestinationAndClosesSession(t *testing.T) {
	sourceErr := errors.New("engine stream failed")
	caller := &testDownloadCaller{data: bytes.Repeat([]byte("x"), 1<<20), failAfter: 1, fail: sourceErr}
	bridge := &Bridge{client: caller}
	dir := t.TempDir()
	destination := filepath.Join(dir, "existing.tar")
	if err := os.WriteFile(destination, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := bridge.download(context.Background(), "linux", "test-session", "existing.tar", func(context.Context, string) (string, error) { return destination, nil })
	if !errors.Is(err, sourceErr) || caller.closed != 1 {
		t.Fatalf("download error = %v, closes = %d", err, caller.closed)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "keep" {
		t.Fatalf("destination changed: %q, %v", got, err)
	}
	assertNoDownloadTempFiles(t, dir)
}

func TestDownloadCancelClosesSessionAndRemovesPartialFile(t *testing.T) {
	t.Run("dialog", func(t *testing.T) {
		caller := &testDownloadCaller{}
		bridge := &Bridge{client: caller}
		selected, err := bridge.download(context.Background(), "linux", "test-session", "x", func(context.Context, string) (string, error) { return "", nil })
		if err != nil || selected != "" || caller.closed != 1 || caller.reads != 0 {
			t.Fatalf("canceled dialog = %q, %v, closes %d, reads %d", selected, err, caller.closed, caller.reads)
		}
	})
	t.Run("in flight", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		caller := &testDownloadCaller{data: bytes.Repeat([]byte("x"), 1<<20), cancel: true, started: make(chan struct{})}
		bridge := &Bridge{client: caller}
		dir := t.TempDir()
		destination := filepath.Join(dir, "archive.tar")
		result := make(chan error, 1)
		go func() {
			_, err := bridge.download(ctx, "linux", "test-session", "x", func(context.Context, string) (string, error) { return destination, nil })
			result <- err
		}()
		<-caller.started
		cancel()
		err := <-result
		if !errors.Is(err, context.Canceled) || caller.closed != 1 {
			t.Fatalf("canceled download = %v, closes %d", err, caller.closed)
		}
		assertNoDownloadTempFiles(t, dir)
	})
}

func assertNoDownloadTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ucd-download-") {
			t.Fatalf("partial download remains: %s", entry.Name())
		}
	}
}
