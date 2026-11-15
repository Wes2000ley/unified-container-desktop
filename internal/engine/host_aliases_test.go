package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

func testHostIPv4() (netip.Addr, error) { return netip.MustParseAddr("192.0.2.10"), nil }

func TestInternalHostAliasesPersistWithoutStartingDocker(t *testing.T) {
	b := &Backend{id: "linux", dataRoot: t.TempDir()}
	request, err := api.NewRequest("engine.internal.hostaliases", "linux", map[string]string{"hostIPv4": "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.configureHostAliases(request); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.dataRoot, "config", "hostaliases.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != "{\"hostIPv4\":\"192.0.2.10\"}\n" {
		t.Fatalf("unexpected configuration: %s", original)
	}
	for _, address := range []string{"127.0.0.1", "0.0.0.0", "::1", "invalid"} {
		request, _ := api.NewRequest("engine.internal.hostaliases", "linux", map[string]string{"hostIPv4": address})
		if err := b.configureHostAliases(request); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("invalid request changed configuration: %s %v", current, err)
	}
	b.id = "windows"
	if err := b.configureHostAliases(request); err == nil {
		t.Fatal("accepted Linux DNS configuration on Windows")
	}
}

func TestHostsWithAliasesPreservesOverridesAndUnrelatedNames(t *testing.T) {
	gateway := netip.MustParseAddr("172.28.240.1")
	original := []byte("# custom hosts\n127.0.0.1 localhost\n192.0.2.1 HOST.DOCKER.INTERNAL. keep.example # preserved comment\n")
	entries := []string{"host.docker.internal:" + gateway.String(), "gateway.docker.internal:" + gateway.String(), "HOST.DOCKER.INTERNAL:192.0.2.25", "database:2001:db8::5", "native-gateway:host-gateway"}
	got, err := hostsWithAliases(original, entries, gateway)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.1 localhost", "192.0.2.1\tkeep.example # preserved comment", "192.0.2.25\thost.docker.internal", "2001:db8::5\tdatabase", "172.28.240.1\tnative-gateway"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	again, err := hostsWithAliases(got, entries, gateway)
	if err != nil || !bytes.Equal(got, again) {
		t.Fatalf("alias update is not idempotent: %q, %v", again, err)
	}
	if strings.Count(string(got), "\thost.docker.internal") != 1 {
		t.Fatalf("duplicate host alias: %s", got)
	}
}

func TestHostsWithAliasesRejectsCorruptFilesAndInvalidAddresses(t *testing.T) {
	for _, test := range []struct{ name, content, entry string }{
		{"unterminated block", "# BEGIN UCD DOCKER HOST ALIASES\n", "host.docker.internal:192.0.2.1"},
		{"unmatched block", "# END UCD DOCKER HOST ALIASES\n", "host.docker.internal:192.0.2.1"},
		{"NUL", "bad\x00file", "host.docker.internal:192.0.2.1"},
		{"invalid IP", "", "host.docker.internal:not-an-ip"},
		{"newline injection", "", "bad\nname:192.0.2.1"},
		{"oversized", strings.Repeat("a", maxHostsFile+1), "host.docker.internal:192.0.2.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := hostsWithAliases([]byte(test.content), []string{test.entry}, netip.MustParseAddr("172.28.240.1")); err == nil {
				t.Fatal("accepted invalid hosts data")
			}
		})
	}
}

func windowsAliasFixture(t *testing.T, next http.HandlerFunc, copied func([]byte)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		path := dockerTestPath(w, r)
		if path == "" {
			return
		}
		if path == "/networks/nat" {
			fmt.Fprint(w, `{"Name":"nat","Driver":"nat","IPAM":{"Config":[{"Subnet":"172.28.240.0/20","Gateway":"172.28.240.1"}]}}`)
			return
		}
		archivePath := strings.ReplaceAll(r.URL.Query().Get("path"), `\`, "/")
		if strings.HasSuffix(path, "/archive") && archivePath == "C:/Windows/System32/drivers/etc/hosts" && r.Method == http.MethodGet {
			data := []byte("127.0.0.1 localhost\r\n192.0.2.5 custom.example\r\n")
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"hosts","size":54,"mode":420}`)))
			archive := tar.NewWriter(w)
			if err := archive.WriteHeader(&tar.Header{Name: "hosts", Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
				t.Error(err)
			}
			_, _ = archive.Write(data)
			_ = archive.Close()
			return
		}
		if strings.HasSuffix(path, "/archive") && archivePath == "C:/Windows/System32/drivers/etc" && r.Method == http.MethodPut {
			reader := tar.NewReader(r.Body)
			header, err := reader.Next()
			if err != nil {
				t.Error(err)
				return
			}
			if header.Name != "hosts" || header.Mode != 0644 {
				t.Errorf("hosts metadata: %#v", header)
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Error(err)
				return
			}
			if copied != nil {
				copied(data)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func TestNativeWindowsCreatePreparesAliasesBeforeReturning(t *testing.T) {
	var hosts []byte
	b := windowsTestBackend(t, windowsAliasFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker call %s", r.URL)
		http.Error(w, "unexpected", 500)
	}, func(data []byte) { hosts = data }))
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var config struct {
			HostConfig struct {
				ExtraHosts []string
				Future     bool
			}
			Future bool
		}
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			t.Fatal(err)
		}
		if !config.Future || !config.HostConfig.Future || len(config.HostConfig.ExtraHosts) != 1 {
			t.Fatal("explicit Docker fields lost")
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"Id":"native"}`)
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader(`{"Image":"probe","Future":true,"HostConfig":{"Future":true,"ExtraHosts":["host.docker.internal:192.0.2.50"]}}`)))
	if w.Code != http.StatusCreated || !strings.Contains(string(hosts), "192.0.2.50\thost.docker.internal") || !strings.Contains(string(hosts), "172.28.240.1\tgateway.docker.internal") {
		t.Fatalf("create %d %s; hosts %s", w.Code, w.Body.String(), hosts)
	}
}

func TestWindowsAliasesUseSelectedComposeNetwork(t *testing.T) {
	var hosts []byte
	b := windowsTestBackend(t, windowsAliasFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/networks/project-default") {
			fmt.Fprint(w, `{"Driver":"nat","IPAM":{"Config":[{"Gateway":"172.30.0.1"}]}}`)
			return
		}
		t.Errorf("unexpected Docker request %s", r.URL)
		http.Error(w, "unexpected", 500)
	}, func(data []byte) { hosts = data }))
	if err := b.prepareWindowsHostAliases(context.Background(), "compose", &container.HostConfig{NetworkMode: "project-default"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), "192.0.2.10\thost.docker.internal") || !strings.Contains(string(hosts), "172.30.0.1\tgateway.docker.internal") {
		t.Fatalf("wrong Compose aliases: %s", hosts)
	}
}

func TestWindowsAliasesPreserveNativeNonNATNetworks(t *testing.T) {
	b := windowsTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if dockerTestPath(w, r) == "" {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/networks/lan") {
			fmt.Fprint(w, `{"Driver":"transparent"}`)
			return
		}
		t.Errorf("unexpected Docker request %s", r.URL)
		http.Error(w, "unexpected", 500)
	})
	if err := b.prepareWindowsHostAliases(context.Background(), "transparent", &container.HostConfig{NetworkMode: "lan"}); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCreateReportsAliasFailureWithCreatedContainerID(t *testing.T) {
	b := windowsTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if dockerTestPath(w, r) != "" {
			http.Error(w, "HNS unavailable", 500)
		}
	})
	_, err := b.finishContainerCreate(context.Background(), client.ContainerCreateResult{ID: "owned-container"}, nil, &container.HostConfig{})
	if err == nil || !strings.Contains(err.Error(), "owned-container was created") || !strings.Contains(err.Error(), "HNS unavailable") {
		t.Fatalf("failure lost container ID or cause: %v", err)
	}
}
