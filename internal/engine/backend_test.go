package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

func call(t *testing.T, b *Backend, method string, params any) (any, error) {
	t.Helper()
	r, err := api.NewRequest(method, b.id, params)
	if err != nil {
		t.Fatal(err)
	}
	return b.Call(context.Background(), r)
}

func testBackend(t *testing.T, handler http.HandlerFunc) *Backend {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	b, err := New("linux", "tcp://"+strings.TrimPrefix(server.URL, "http://"), t.TempDir(), &api.EventBus{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.hostIPv4 = testHostIPv4
	return b
}

func TestLoadStagedProjectUsesPortBindingDefault(t *testing.T) {
	for _, engineID := range []string{"linux", "windows"} {
		for _, binding := range []string{api.DefaultPortBindingLocal, api.DefaultPortBindingAll} {
			t.Run(engineID+"/"+binding, func(t *testing.T) {
				handler := func(w http.ResponseWriter, r *http.Request) {
					t.Errorf("unexpected Docker request %s", r.URL)
					http.Error(w, "unexpected Docker request", http.StatusInternalServerError)
				}
				var b *Backend
				if engineID == "windows" {
					b = windowsTestBackend(t, handler)
				} else {
					b = testBackend(t, handler)
				}
				b.configMu.Lock()
				b.runtimeConfig.Network.DefaultPortBinding = binding
				b.configMu.Unlock()

				value, err := call(t, b, "compose.import", api.ComposeParams{
					Project: api.ProjectPart{Engine: engineID, Name: "ports"},
					YAML:    "services:\n  web:\n    image: nginx\n    ports:\n      - '8080:80'\n      - '127.0.0.2:8443:443'\n",
				})
				if err != nil {
					t.Fatal(err)
				}
				project, err := b.loadProject(context.Background(), value.(api.ProjectPart))
				if err != nil {
					t.Fatal(err)
				}
				ports := project.Services["web"].Ports
				wantDefault := "127.0.0.1"
				if binding == api.DefaultPortBindingAll {
					wantDefault = "0.0.0.0"
				}
				if len(ports) != 2 || ports[0].HostIP != wantDefault || ports[1].HostIP != "127.0.0.2" {
					t.Fatalf("Compose ports = %#v, want default %s and explicit 127.0.0.2", ports, wantDefault)
				}
			})
		}
	}
}

func TestComposeValidationImportAndSafeWrite(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker request %s", r.URL)
		w.WriteHeader(500)
	})
	yaml := "services:\n  web:\n    image: nginx\n    ports:\n      - '8080:80'\n      - '127.0.0.2:8443:443'\n"
	p := api.ComposeParams{Project: api.ProjectPart{Engine: "linux", Name: "demo"}, YAML: yaml, Files: []api.File{{Path: "site/index.html", Content: []byte("hello")}, {Path: "obsolete.txt", Content: []byte("original")}}}
	got, err := call(t, b, "compose.validate", p)
	if err != nil {
		t.Fatal(err)
	}
	ports := got.([]api.Port)
	if len(ports) != 2 || ports[0].HostIP != "127.0.0.1" || ports[0].Published != 8080 || ports[1].HostIP != "127.0.0.2" {
		t.Fatalf("ports = %#v", ports)
	}
	got, err = call(t, b, "compose.import", p)
	if err != nil {
		t.Fatal(err)
	}
	part := got.(api.ProjectPart)
	if data, err := os.ReadFile(filepath.Join(part.Directory, "site", "index.html")); err != nil || string(data) != "hello" {
		t.Fatalf("imported file: %q, %v", data, err)
	}
	project, err := b.loadProject(context.Background(), part)
	if err != nil {
		t.Fatal(err)
	}
	if project.Services["web"].Ports[0].HostIP != "127.0.0.1" {
		t.Fatal("loopback default missing from actual Compose model")
	}
	if _, err := call(t, b, "compose.import", p); err == nil {
		t.Fatal("duplicate import succeeded")
	}
	if err := os.WriteFile(filepath.Join(part.Directory, "keep.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	replace := p
	replace.Replace = true
	replace.YAML = "services: ["
	replace.Files = []api.File{{Path: "site/index.html", Content: []byte("changed")}}
	if _, err := call(t, b, "compose.import", replace); err == nil {
		t.Fatal("invalid replacement succeeded")
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "site", "index.html")); err != nil || string(data) != "hello" {
		t.Fatalf("replacement rollback: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "obsolete.txt")); err != nil || string(data) != "original" {
		t.Fatalf("stale file rollback: %q, %v", data, err)
	}
	replace.YAML = yaml
	if err := os.WriteFile(filepath.Join(part.Directory, "obsolete.txt"), []byte("runtime change"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "compose.import", replace); err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Fatalf("modified tracked file removal: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "obsolete.txt")); err != nil || string(data) != "runtime change" {
		t.Fatalf("modified tracked file: %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(part.Directory, "obsolete.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(part.Directory, "site", "index.html"), []byte("runtime edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "compose.import", replace); err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Fatalf("modified tracked file overwrite: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "site", "index.html")); err != nil || string(data) != "runtime edit" {
		t.Fatalf("modified tracked file overwritten: %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(part.Directory, "site", "index.html"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	conflict := replace
	conflict.Files = append(append([]api.File{}, replace.Files...), api.File{Path: "keep.txt", Content: []byte("new source")})
	if _, err := call(t, b, "compose.import", conflict); err == nil || !strings.Contains(err.Error(), "untracked") {
		t.Fatalf("untracked collision: %v", err)
	}
	if _, err := call(t, b, "compose.import", replace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(part.Directory, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file still exists: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "site", "index.html")); err != nil || string(data) != "changed" {
		t.Fatalf("replacement file: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "keep.txt")); err != nil || string(data) != "data" {
		t.Fatalf("preserved file: %q, %v", data, err)
	}
	if _, err := call(t, b, "compose.write", api.ComposeParams{Project: part, YAML: "services: ["}); err == nil {
		t.Fatal("invalid YAML accepted")
	}
	data, err := os.ReadFile(part.ComposeFile)
	if err != nil || string(data) != yaml {
		t.Fatalf("invalid write changed file: %q, %v", data, err)
	}
	updated := strings.Replace(yaml, "8080:80", "8081:80", 1)
	if _, err := call(t, b, "compose.write", api.ComposeParams{Project: part, YAML: updated}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(part.ComposeFile)
	if err != nil || string(data) != updated {
		t.Fatalf("valid write: %q, %v", data, err)
	}
	part.ComposeFile = filepath.Join(t.TempDir(), "outside.yaml")
	if _, err := call(t, b, "compose.write", api.ComposeParams{Project: part, YAML: updated}); err == nil {
		t.Fatal("outside write accepted")
	}
}

func TestDaemonSettingsPreserveOtherValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	if err := os.WriteFile(path, []byte(`{"log-level":"warn","proxies":{"no-proxy":"localhost"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonSettings(path, daemonSettings{
		Proxies:         map[string]string{"http-proxy": "http://proxy.example:8080", "https-proxy": "http://proxy.example:8080", "no-proxy": "localhost"},
		RegistryMirrors: []string{"https://mirror.example"},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	proxies := got["proxies"].(map[string]any)
	if got["log-level"] != "warn" || proxies["no-proxy"] != "localhost" || proxies["http-proxy"] != "http://proxy.example:8080" || proxies["https-proxy"] != "http://proxy.example:8080" {
		t.Fatalf("configuration %#v", got)
	}
	for _, value := range []string{"http://user:pass@proxy.example", "file:///tmp/x", "https://mirror.example?token=x"} {
		if validEndpoint(value) {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestRejectsUnsafeProjectFilesAndLinuxWindowsMounts(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker request %s", r.URL)
		w.WriteHeader(500)
	})
	part := api.ProjectPart{Engine: "linux", Name: "demo"}
	for _, path := range []string{"../escape", "/absolute", `C:\\escape`, "compose.yaml", ".ucd-import.json"} {
		_, err := call(t, b, "compose.validate", api.ComposeParams{Project: part, YAML: "services:\n  web:\n    image: nginx\n", Files: []api.File{{Path: path, Content: []byte("x")}}})
		if err == nil {
			t.Errorf("accepted path %q", path)
		}
	}
	yaml := "services:\n  web:\n    image: nginx\n    volumes:\n      - type: bind\n        source: 'C:\\data'\n        target: /data\n"
	_, err := call(t, b, "compose.validate", api.ComposeParams{Project: part, YAML: yaml})
	if err == nil || !strings.Contains(err.Error(), "Windows bind path") {
		t.Fatalf("Windows bind path: %v", err)
	}
}

func TestDockerReadAndRefuseUsedImage(t *testing.T) {
	var deleted bool
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			w.Header().Set("OSType", "linux")
			w.WriteHeader(200)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/v1.45")
		switch {
		case path == "/info":
			fmt.Fprint(w, `{"OSType":"linux","ServerVersion":"29.0","Containers":1,"ContainersRunning":1,"NCPU":4,"MemTotal":1024}`)
		case path == "/containers/json":
			fmt.Fprint(w, `[{"Id":"container1","Names":["/web"],"Image":"example:latest","ImageID":"sha256:abc","State":"running","Status":"Up","Labels":{"com.docker.compose.project":"demo"},"Ports":[{"IP":"127.0.0.1","PrivatePort":80,"PublicPort":8080,"Type":"tcp"}]}]`)
		case path == "/containers/container1/stats":
			fmt.Fprint(w, `{"preread":"2026-01-01T00:00:00Z","read":"2026-01-01T00:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":200},"system_cpu_usage":1200,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":100},"system_cpu_usage":1000},"memory_stats":{"usage":1000,"stats":{"inactive_file":100}}}`)
		case path == "/images/json":
			fmt.Fprint(w, `[{"Id":"sha256:abc","RepoTags":["example:latest"],"Size":123,"Containers":-1}]`)
		case path == "/images/example:latest/json":
			fmt.Fprint(w, `{"Id":"sha256:abc"}`)
		case strings.HasPrefix(path, "/images/") && r.Method == http.MethodDelete:
			deleted = true
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	status, err := call(t, b, "engine.status", nil)
	if err != nil || !status.(api.EngineStatus).Online || status.(api.EngineStatus).CPUs != 4 {
		t.Fatalf("status %#v: %v", status, err)
	}
	got, err := call(t, b, "container.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	containers := got.([]api.Container)
	if len(containers) != 1 || containers[0].Name != "web" || containers[0].Ports[0].Published != 8080 {
		t.Fatalf("containers %#v", containers)
	}
	if containers[0].MetricsAvailable {
		t.Fatal("unrequested metrics reported as available")
	}
	discovered, err := call(t, b, "project.discover", nil)
	if err != nil || len(discovered.([]api.ProjectPart)) != 1 {
		t.Fatalf("discovered %#v: %v", discovered, err)
	}
	_, err = call(t, b, "compose.read", api.ComposeParams{Project: discovered.([]api.ProjectPart)[0]})
	if err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatalf("discovered read: %v", err)
	}
	got, err = call(t, b, "container.list", map[string]bool{"metrics": true})
	if err != nil {
		t.Fatal(err)
	}
	measured := got.([]api.Container)[0]
	if !measured.MetricsAvailable || measured.CPUPercent != 100 || measured.MemoryBytes != 900 {
		t.Fatalf("metrics %#v", measured)
	}
	got, err = call(t, b, "image.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if images := got.([]api.Image); len(images) != 1 || images[0].Containers != 1 {
		t.Fatalf("image usage %#v", images)
	}
	_, err = call(t, b, "image.remove", api.ImageParams{Reference: "example:latest"})
	if err == nil || !strings.Contains(err.Error(), "used by container") || deleted {
		t.Fatalf("used image removal: deleted=%v err=%v", deleted, err)
	}
}

func TestEngineOfflineReturnsStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	address := "tcp://" + strings.TrimPrefix(server.URL, "http://")
	server.Close()
	b, err := New("linux", address, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := call(t, b, "engine.status", nil)
	if err != nil || got.(api.EngineStatus).Online || got.(api.EngineStatus).Error == "" {
		t.Fatalf("offline status %#v: %v", got, err)
	}
}

func TestDockerEventsReconnectAndStop(t *testing.T) {
	var calls atomic.Int32
	reconnected := make(chan struct{}, 1)
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/events" {
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			fmt.Fprintln(w, `{"Type":"container","Action":"start","Actor":{"ID":"abc","Attributes":{"com.docker.compose.project":"demo"}}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case reconnected <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	select {
	case <-reconnected:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("event stream did not reconnect")
	}
	var sawEvent, sawDisconnect bool
	for _, event := range b.events.Since(0) {
		if event.Type == "engine.event" && event.Project == "demo" && strings.Contains(event.Message, "start abc") {
			sawEvent = true
		}
		if event.Type == "engine.disconnected" {
			sawDisconnect = true
		}
	}
	if !sawEvent || !sawDisconnect {
		t.Errorf("event=%v disconnect=%v", sawEvent, sawDisconnect)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event stream did not stop")
	}
}
