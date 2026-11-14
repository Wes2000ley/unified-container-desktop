//go:build linux

package integration

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/helper"
	"github.com/skyri/ucd/internal/pki"
)

// TestHelperDockerLifecycle is opt-in and must run only in a disposable Linux
// environment with its own Docker daemon at /var/run/docker.sock.
func TestHelperDockerLifecycle(t *testing.T) {
	if os.Getenv("UCD_DOCKER_INTEGRATION") != "1" {
		t.Skip("set UCD_DOCKER_INTEGRATION=1 inside an isolated Linux Docker test environment")
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	projectName := "ucd-it-" + hex.EncodeToString(random[:])
	imageName := projectName + ":test"
	root := t.TempDir()
	certs := filepath.Join(root, "certificates")
	if err := pki.Ensure(certs); err != nil {
		t.Fatalf("create test certificates: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(certs, "ca.pem")
	config := helper.Config{Listen: address, DataRoot: filepath.Join(root, "data"), CACert: ca,
		ServerCert: filepath.Join(certs, "server.pem"), ServerKey: filepath.Join(certs, "server-key.pem")}
	settings := api.Settings{HelperURL: "https://localhost:" + port, CACert: ca,
		ClientCert: filepath.Join(certs, "client.pem"), ClientKey: filepath.Join(certs, "client-key.pem")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- helper.Serve(ctx, config) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("helper shutdown: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("helper did not stop after cancellation")
		}
	})
	client, err := helper.NewClient(settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	deadline := time.Now().Add(30 * time.Second)
	for {
		request, _ := api.NewRequest("engine.status", "linux", nil)
		value, callErr := client.Call(ctx, request)
		if callErr == nil {
			var status api.EngineStatus
			if err := json.Unmarshal(value.(json.RawMessage), &status); err != nil {
				t.Fatal(err)
			}
			if status.Online {
				break
			}
			callErr = errors.New(status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper or isolated Docker daemon did not become ready: %v", callErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Log("authenticated helper and isolated Docker daemon are ready")

	caPEM, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("test CA could not be loaded")
	}
	unauthenticated := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}, Timeout: 5 * time.Second}
	response, err := unauthenticated.Post(settings.HelperURL+"/v1/call", "application/json", strings.NewReader(`{"method":"engine.status"}`))
	if err == nil {
		response.Body.Close()
		t.Fatal("helper accepted a client without a certificate")
	}
	t.Log("unauthenticated TLS client was rejected")

	call := func(method string, params any, result any) {
		t.Helper()
		request, err := api.NewRequest(method, "linux", params)
		if err != nil {
			t.Fatal(err)
		}
		value, err := client.Call(ctx, request)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if result != nil {
			if err := json.Unmarshal(value.(json.RawMessage), result); err != nil {
				t.Fatalf("decode %s result: %v", method, err)
			}
		}
	}
	call("image.pull", api.ImageParams{Reference: "busybox:1.37.0"}, nil)
	t.Log("pulled busybox:1.37.0 into the isolated daemon")
	yaml := `services:
  app:
    image: __TEST_IMAGE__
    build:
      context: .
      additional_contexts:
        extra: ./extra
    network_mode: none
    command: ["sh", "-c", "echo ucd-build:$(cat /marker.txt); echo ucd-script:$(su -s /bin/sh nobody -c /scripts/run.sh); if [ -f /data/value ]; then echo ucd-persisted:$(cat /data/value); else echo ucd-volume-marker >/data/value; echo ucd-initial; fi; exec tail -f /dev/null"]
    volumes:
      - store:/data
      - ./scripts:/scripts:ro
volumes:
  store: {}
`
	yaml = strings.ReplaceAll(yaml, "__TEST_IMAGE__", imageName)
	importParams := api.ComposeParams{Project: api.ProjectPart{Engine: "linux", Name: projectName}, YAML: yaml,
		Files: []api.File{{Path: "Dockerfile", Content: []byte("FROM busybox:1.37.0\nCOPY --from=extra /marker.txt /marker.txt\n")},
			{Path: "extra/marker.txt", Content: []byte("test build context\n")},
			{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\ncat /scripts/data.txt\n"), Mode: 0755},
			{Path: "scripts/data.txt", Content: []byte("nonroot-readable\n")}}}
	var ports []api.Port
	call("compose.validate", importParams, &ports)
	var part api.ProjectPart
	call("compose.import", importParams, &part)
	if got, err := os.ReadFile(filepath.Join(part.Directory, "extra", "marker.txt")); err != nil || string(got) != "test build context\n" {
		t.Fatalf("imported build file = %q, %v", got, err)
	}
	for path, want := range map[string]os.FileMode{"scripts": 0755, "scripts/run.sh": 0755, "scripts/data.txt": 0644} {
		info, err := os.Stat(filepath.Join(part.Directory, path))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("imported %s mode = %v, error %v; want %04o", path, info, err, want)
		}
	}
	cleanupParams := api.ComposeParams{Project: part, Action: "down", RemoveVolumes: true}
	imageBuilt := false
	t.Cleanup(func() {
		request, err := api.NewRequest("compose.action", "linux", cleanupParams)
		if err != nil {
			t.Errorf("cleanup request: %v", err)
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := client.Call(cleanupCtx, request); err != nil {
			t.Errorf("cleanup project: %v", err)
		}
		if imageBuilt {
			request, err = api.NewRequest("image.remove", "linux", api.ImageParams{Reference: imageName})
			if err != nil {
				t.Errorf("cleanup image request: %v", err)
				return
			}
			if _, err := client.Call(cleanupCtx, request); err != nil {
				t.Errorf("cleanup image: %v", err)
			}
		}
	})
	action := func(name string, removeVolumes bool) {
		t.Helper()
		call("compose.action", api.ComposeParams{Project: part, Action: name, RemoveVolumes: removeVolumes}, nil)
		t.Logf("compose %s completed", name)
	}
	action("build", false)
	imageBuilt = true
	action("up", false)
	var containers []api.Container
	call("container.list", nil, &containers)
	if len(containers) != 1 || containers[0].Project != part.Name || containers[0].State != "running" {
		t.Fatalf("unexpected containers after up: %+v", containers)
	}
	waitLogs := func(id, marker string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var logs api.LogResult
			call("container.logs", api.ContainerParams{ID: id, Tail: 50}, &logs)
			if strings.Contains(logs.Text, marker) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("container logs missing %q: %q", marker, logs.Text)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	waitLogs(containers[0].ID, "ucd-build:test build context")
	waitLogs(containers[0].ID, "ucd-script:nonroot-readable")
	waitLogs(containers[0].ID, "ucd-initial")
	action("stop", false)
	call("container.list", nil, &containers)
	if len(containers) != 1 || containers[0].State == "running" {
		t.Fatalf("container did not stop: %+v", containers)
	}
	action("start", false)
	action("restart", false)
	action("down", false)
	var volumes []api.Volume
	call("volume.list", nil, &volumes)
	if !hasVolume(volumes, projectName+"_store") {
		t.Fatalf("down without volume removal deleted the named volume: %+v", volumes)
	}
	action("up", false)
	call("container.list", nil, &containers)
	if len(containers) != 1 {
		t.Fatalf("unexpected containers after second up: %+v", containers)
	}
	waitLogs(containers[0].ID, "ucd-persisted:ucd-volume-marker")
	action("down", true)
	call("volume.list", nil, &volumes)
	if hasVolume(volumes, projectName+"_store") {
		t.Fatalf("explicit removal left the named volume: %+v", volumes)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		var batch api.EventBatch
		call("system.events", api.EventsParams{}, &batch)
		for _, event := range batch.Events {
			if event.Type == "engine.event" && event.Engine == "linux" && event.Project == part.Name && strings.Contains(event.Message, "container ") {
				t.Logf("received Docker event through helper: %s", event.Message)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not expose Docker container events; received %d events", len(batch.Events))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func hasVolume(volumes []api.Volume, name string) bool {
	for _, volume := range volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}
