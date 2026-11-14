//go:build windows

package local

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestDockerRelayStreamsRequestsAndResponsesThenCloses(t *testing.T) {
	ctx := context.Background()
	server := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		if r.URL.RequestURI() != "/v1.45/build?version=1" {
			t.Errorf("request URI = %q", r.URL.RequestURI())
		}
		if len(body) != 4<<20 || !bytes.Equal(body[:8], []byte("context-")) {
			t.Errorf("request body length/prefix = %d/%q", len(body), body[:min(8, len(body))])
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.Copy(w, bytes.NewReader(bytes.Repeat([]byte("progress"), 128<<10)))
	})
	relay, err := startDockerRelay(ctx, server)
	if err != nil {
		t.Fatal(err)
	}
	transport, client := dockerRelayHTTPClient(t, relay.Endpoint())
	request, err := http.NewRequest(http.MethodPost, "http://docker/v1.45/build?version=1", io.MultiReader(strings.NewReader("context-"), bytes.NewReader(bytes.Repeat([]byte("x"), (4<<20)-8))))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusAccepted || response.Header.Get("Content-Type") != "application/octet-stream" || len(body) != len("progress")*128<<10 {
		t.Fatalf("streamed response = status %d, bytes %d, read error %v", response.StatusCode, len(body), readErr)
	}
	transport.CloseIdleConnections()
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	pipe := strings.Replace(relay.Endpoint(), `npipe:////./pipe/`, `\\.\pipe\`, 1)
	dialCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := winio.DialPipeContext(dialCtx, pipe); err == nil {
		t.Fatal("private relay still accepted connections after close")
	}
}

func dockerRelayHTTPClient(t *testing.T, endpoint string) (*http.Transport, *http.Client) {
	t.Helper()
	pipe := strings.Replace(endpoint, `npipe:////./pipe/`, `\\.\pipe\`, 1)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, pipe)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return transport, &http.Client{Transport: transport}
}
