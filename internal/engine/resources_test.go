package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/skyri/ucd/internal/api"
)

func TestImageBuildContextAndProgress(t *testing.T) {
	var built bool
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/build" {
			http.Error(w, "unexpected", 500)
			return
		}
		if r.URL.Query().Get("dockerfile") != "Dockerfile" || r.URL.Query().Get("t") != "demo:latest" {
			t.Errorf("build query %v", r.URL.Query())
		}
		files := map[string]string{}
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			files[h.Name] = string(data)
		}
		if files["Dockerfile"] != "FROM scratch\nCOPY app /app\n" || files["app"] != "binary" {
			t.Errorf("build context files %#v", files)
		}
		built = true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"stream":"Step 1/2\n"}`)
		fmt.Fprintln(w, `{"stream":"Successfully built abc\n"}`)
	})
	got, err := call(t, b, "image.build", api.ImageResourceParams{Dockerfile: "FROM scratch\nCOPY app /app\n", Files: []api.File{{Path: "app", Content: []byte("binary")}}, Tags: []string{"demo:latest"}, OperationID: "build-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !built || !strings.Contains(got.(api.ResourceOutput).Output, "Successfully built") {
		t.Fatalf("output %#v, built %v", got, built)
	}
	events := b.events.Since(0)
	if len(events) != 2 || events[0].Project != "build-1" || events[0].Type != "image.progress" {
		t.Fatalf("progress events %#v", events)
	}
	for _, name := range []string{"../escape", "/absolute", `x\y`, "Dockerfile"} {
		if _, err := buildContext(api.ImageResourceParams{Dockerfile: "FROM scratch", Files: []api.File{{Path: name, Content: []byte("x")}}}); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if _, err := b.resourceProgress(strings.NewReader(`{"errorDetail":{"message":"bad build"}}`), "build image", ""); err == nil || !strings.Contains(err.Error(), "bad build") {
		t.Fatalf("build error: %v", err)
	}
	secretOutput, err := b.resourceProgress(strings.NewReader(`{"stream":"token example-private-token\n"}`), "build image", "", "example-private-token")
	if err != nil || strings.Contains(secretOutput.Output, "example-private-token") {
		t.Fatalf("credential appeared in build output: %v", err)
	}
	for _, event := range b.events.Since(0) {
		if strings.Contains(event.Message, "example-private-token") {
			t.Fatal("credential appeared in a progress event")
		}
	}
	context, err := buildContext(api.ImageResourceParams{Dockerfile: "FROM scratch", Files: []api.File{
		{Path: ".dockerignore", Content: []byte("secret/\n!secret/keep.txt\n")},
		{Path: "secret/private.txt", Content: []byte("excluded")},
		{Path: "secret/keep.txt", Content: []byte("included")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	reader := tar.NewReader(bytes.NewReader(context))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		paths[header.Name] = true
	}
	if paths["secret/private.txt"] || !paths["secret/keep.txt"] || !paths[".dockerignore"] {
		t.Fatalf(".dockerignore was not applied: %v", paths)
	}
	context, err = buildContext(api.ImageResourceParams{Dockerfile: "FROM scratch", DockerfilePath: "build/Customfile", Files: []api.File{
		{Path: ".dockerignore", Content: []byte("root-only\n")},
		{Path: "build/Customfile.dockerignore", Content: []byte("secret/\n!secret/keep.txt\n")},
		{Path: "secret/private.txt", Content: []byte("excluded")},
		{Path: "secret/keep.txt", Content: []byte("included")},
		{Path: "root-only", Content: []byte("included")},
		{Path: "Dockerfile", Content: []byte("alternate")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	paths = map[string]bool{}
	reader = tar.NewReader(bytes.NewReader(context))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		paths[header.Name] = true
	}
	if paths["secret/private.txt"] || !paths["secret/keep.txt"] || !paths["build/Customfile"] || !paths["build/Customfile.dockerignore"] || !paths["root-only"] || !paths["Dockerfile"] {
		t.Fatalf("Dockerfile-specific ignore precedence failed: %v", paths)
	}
}

func TestMachineRegistryLoginLogout(t *testing.T) {
	t.Setenv("DOCKER_AUTH_CONFIG", "")
	const password = "dummy-private-registry-token"
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/auth" {
			http.Error(w, "unexpected", 500)
			return
		}
		var input struct {
			Username      string
			Password      string
			ServerAddress string
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Username != "tester" || input.Password != password || input.ServerAddress != "registry.example:5000" {
			t.Error("authentication request did not contain supplied credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Status":"Login Succeeded"}`)
	})
	p := api.RegistryParams{Server: "registry.example:5000", Username: "tester", Password: password}
	if _, err := call(t, b, "registry.login", p); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(b.dataRoot, "docker-config", "config.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(password)) || !bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString([]byte("tester:"+password)))) {
		t.Fatal("machine Docker config did not contain the expected encoded credentials")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("configuration permissions: %v %v", info, err)
		}
	}
	got, err := call(t, b, "registry.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(password)) || !bytes.Contains(encoded, []byte("registry.example:5000")) {
		t.Fatal("registry listing exposed credentials or omitted the registry")
	}
	if _, err := call(t, b, "registry.logout", api.RegistryParams{Server: p.Server}); err != nil {
		t.Fatal(err)
	}
	got, err = call(t, b, "registry.list", nil)
	if err != nil || len(got.([]map[string]string)) != 0 {
		t.Fatalf("logout did not remove registry: %v", err)
	}
	if strings.Contains(redactRegistryError("authentication rejected "+password, "tester", password), password) {
		t.Fatal("registry error exposed password")
	}
	if _, err := registryServer("https://user:password@example.test"); err == nil {
		t.Fatal("registry URL with credentials accepted")
	}
}

func TestImageRegistryAuthenticationUsesMachineConfig(t *testing.T) {
	foreign := base64.StdEncoding.EncodeToString([]byte("foreign-user:foreign-dummy-password"))
	t.Setenv("DOCKER_AUTH_CONFIG", `{"auths":{"registry.example":{"auth":"`+foreign+`"}}}`)
	var authenticated int
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/auth":
			fmt.Fprint(w, `{"Status":"Login Succeeded"}`)
		case "/images/create", "/images/registry.example/asset/push":
			auth, err := authconfig.Decode(r.Header.Get("X-Registry-Auth"))
			if err != nil || auth.Username != "machine-user" || auth.Password != "machine-dummy-password" {
				t.Error("image operation did not use machine-owned registry credentials")
			}
			authenticated++
			fmt.Fprintln(w, `{"status":"done"}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	if _, err := call(t, b, "registry.login", api.RegistryParams{Server: "registry.example", Username: "machine-user", Password: "machine-dummy-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "image.pull", api.ImageParams{Reference: "registry.example/asset:latest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "image.push", api.ImageResourceParams{Reference: "registry.example/asset:latest"}); err != nil {
		t.Fatal(err)
	}
	if authenticated != 2 {
		t.Fatalf("expected two authenticated image operations, got %d", authenticated)
	}
	if _, err := call(t, b, "registry.login", api.RegistryParams{Username: "hub-user", Password: "hub-dummy-password"}); err != nil {
		t.Fatal(err)
	}
	token, _, err := b.machineRegistryAuth(context.Background(), "alpine:latest")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := authconfig.Decode(token)
	if err != nil || hub.Username != "hub-user" || hub.Password != "hub-dummy-password" {
		t.Fatal("Docker Hub image did not use its canonical machine registry entry")
	}
	b.configMu.Lock()
	b.registryConfig.CredentialHelpers = map[string]string{"registry.example": "unsupported"}
	b.configMu.Unlock()
	if _, _, err := b.machineRegistryAuth(context.Background(), "registry.example/asset"); err == nil || !strings.Contains(err.Error(), "external credential helper") {
		t.Fatalf("external credential helper must be rejected: %v", err)
	}
}

func TestRegistryCredentialHelperStoreAndInternalSnapshot(t *testing.T) {
	const secret = "synthetic-registry-helper-token"
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/auth" {
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Status":"Login Succeeded"}`)
	})
	if _, err := call(t, b, "registry.credential.store", api.RegistryCredentialParams{
		ServerURL: "https://registry.example", Username: "test-user", Secret: secret,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := call(t, b, "registry.credential.get", api.RegistryCredentialParams{ServerURL: "registry.example"})
	if err != nil {
		t.Fatal(err)
	}
	credential := got.(api.RegistryCredentialParams)
	if credential.Username != "test-user" || credential.Secret != secret || credential.ServerURL != "registry.example" {
		t.Fatalf("credential = %#v", credential)
	}
	const identityToken = "synthetic-identity-token"
	if _, err := call(t, b, "registry.credential.store", api.RegistryCredentialParams{
		ServerURL: "identity.example", Username: "<token>", Secret: identityToken,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = call(t, b, "registry.credential.get", api.RegistryCredentialParams{ServerURL: "identity.example"})
	if err != nil {
		t.Fatal(err)
	}
	identity := got.(api.RegistryCredentialParams)
	if identity.Username != "<token>" || identity.Secret != identityToken {
		t.Fatalf("identity token credential = %#v", identity)
	}
	listed, err := call(t, b, "registry.credential.list", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(listed)
	if err != nil || bytes.Contains(encoded, []byte(secret)) || !bytes.Contains(encoded, []byte("test-user")) {
		t.Fatalf("credential list exposed or omitted values: %s, %v", encoded, err)
	}
	var credentialNames map[string]string
	if err := json.Unmarshal(encoded, &credentialNames); err != nil || bytes.Contains(encoded, []byte(identityToken)) || credentialNames["identity.example"] != "<token>" {
		t.Fatalf("credential list exposed the identity token or omitted its token label: %s, %v", encoded, err)
	}
	snapshot, err := call(t, b, "registry.internal.snapshot", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	registrySnapshot := snapshot.(api.RegistrySnapshot)
	auth := registrySnapshot.AuthConfigs["registry.example"]
	if auth.Username != "test-user" || auth.Password != secret {
		t.Fatalf("central snapshot = %#v", registrySnapshot)
	}
	if _, err := call(t, b, "registry.credential.erase", api.RegistryCredentialParams{ServerURL: "registry.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "registry.credential.get", api.RegistryCredentialParams{ServerURL: "registry.example"}); err == nil {
		t.Fatal("erased credentials are still available")
	}
}

func TestRegistryLoginErrorRedactsSuppliedSecret(t *testing.T) {
	const secret = "synthetic-rejected-registry-secret"
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"message":"rejected credential %s"}`, secret)
	})
	_, err := call(t, b, "registry.login", api.RegistryParams{Server: "registry.example", Username: "test-user", Password: secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("login error leaked credential: %v", err)
	}
}

func TestRegistryOperationSnapshotIsScopedToTheCall(t *testing.T) {
	var authenticated, anonymous int
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/images/create" {
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		value, err := authconfig.Decode(r.Header.Get("X-Registry-Auth"))
		if err != nil {
			t.Error(err)
		}
		if value.Username == "shared-user" && value.Password == "synthetic-shared-secret" {
			authenticated++
		} else if value.Username == "" && value.Password == "" {
			anonymous++
		} else {
			t.Errorf("unexpected auth config: %#v", value)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"done"}`)
	})
	operation, err := api.NewRequest("image.pull", "linux", api.ImageParams{Reference: "registry.example/app:latest"})
	if err != nil {
		t.Fatal(err)
	}
	internal, err := api.NewRequest("registry.internal.call", "linux", api.RegistryOperation{
		Request: operation,
		Snapshot: api.RegistrySnapshot{AuthConfigs: map[string]api.RegistryAuth{
			"registry.example": {Username: "shared-user", Password: "synthetic-shared-secret"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call(context.Background(), internal); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "image.pull", api.ImageParams{Reference: "registry.example/app:latest"}); err != nil {
		t.Fatal(err)
	}
	if authenticated != 1 || anonymous != 1 {
		t.Fatalf("authenticated=%d anonymous=%d", authenticated, anonymous)
	}
	_, err = os.Stat(filepath.Join(b.dataRoot, "docker-config", "config.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operation credentials became persistent: %v", err)
	}
}
func TestStoragePruneConfirmationAndVolumeScope(t *testing.T) {
	var calls int
	var filters []string
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/volumes/prune" {
			http.Error(w, "unexpected", 500)
			return
		}
		calls++
		filters = append(filters, r.URL.Query().Get("filters"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"VolumesDeleted":[],"SpaceReclaimed":0}`)
	})
	if _, err := call(t, b, "storage.prune", api.StoragePruneParams{Scope: "volumes"}); err == nil || calls != 0 {
		t.Fatalf("unconfirmed prune: calls=%d, err=%v", calls, err)
	}
	if _, err := call(t, b, "storage.prune", api.StoragePruneParams{Scope: "volumes", Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, b, "storage.prune", api.StoragePruneParams{Scope: "volumes", All: true, Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || strings.Contains(filters[0], `"all"`) || !strings.Contains(filters[1], `"all"`) {
		t.Fatalf("prune scopes %#v", filters)
	}
}

func TestStoragePruneScopeAndAllRouting(t *testing.T) {
	tests := []struct {
		scope, endpoint string
		all             bool
		filters, allArg string
	}{
		{scope: "containers", endpoint: "/containers/prune"},
		{scope: "images", endpoint: "/images/prune"},
		{scope: "images", endpoint: "/images/prune", all: true, filters: `{"dangling":{"false":true}}`},
		{scope: "networks", endpoint: "/networks/prune"},
		{scope: "volumes", endpoint: "/volumes/prune"},
		{scope: "volumes", endpoint: "/volumes/prune", all: true, filters: `{"all":{"true":true}}`},
		{scope: "build-cache", endpoint: "/build/prune"},
		{scope: "build-cache", endpoint: "/build/prune", all: true, allArg: "1"},
	}
	type pruneRequest struct{ method, path, filters, all string }
	for _, test := range tests {
		t.Run(fmt.Sprintf("%s-all-%t", test.scope, test.all), func(t *testing.T) {
			requests := make(chan pruneRequest, 2)
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.45")
					return
				}
				select {
				case requests <- pruneRequest{r.Method, strings.TrimPrefix(r.URL.Path, "/v1.45"), r.URL.Query().Get("filters"), r.URL.Query().Get("all")}:
				default:
					t.Error("unexpected additional Docker prune request")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ContainersDeleted":["owned"],"ImagesDeleted":[{"Deleted":"owned"}],"NetworksDeleted":["owned"],"VolumesDeleted":["owned"],"CachesDeleted":["owned"],"SpaceReclaimed":123}`)
			})
			params := api.StoragePruneParams{Scope: test.scope, All: test.all}
			_, err := call(t, b, "storage.prune", params)
			var denial *api.Error
			if !errors.As(err, &denial) || denial.Code != "confirmation_required" || len(requests) != 0 {
				t.Fatalf("unconfirmed prune was not denied before the Docker call: %v", err)
			}
			params.Confirmed = true
			result, err := call(t, b, "storage.prune", params)
			if err != nil {
				t.Fatal(err)
			}
			var request pruneRequest
			select {
			case request = <-requests:
			default:
				t.Fatal("confirmed prune did not reach Docker")
			}
			if request.method != http.MethodPost || request.path != test.endpoint || request.filters != test.filters || request.all != test.allArg || len(requests) != 0 {
				t.Fatalf("unexpected Docker prune request: %#v", request)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("owned")) || (test.scope != "networks" && !bytes.Contains(data, []byte(`"SpaceReclaimed":123`))) {
				t.Fatalf("prune result did not preserve the Docker report: %s", data)
			}
		})
	}
}

func TestNetworkInventoryAndConnect(t *testing.T) {
	var connected bool
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/networks":
			fmt.Fprint(w, `[{"Id":"net1","Name":"demo","Driver":"bridge"}]`)
		case "/networks/net1/connect":
			var input struct {
				Container      string
				EndpointConfig struct{ Aliases []string }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			connected = input.Container == "ctr1" && len(input.EndpointConfig.Aliases) == 1 && input.EndpointConfig.Aliases[0] == "web"
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected", 500)
		}
	})
	got, err := call(t, b, "network.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	items := got.([]api.Network)
	if len(items) != 1 || items[0].Engine != "linux" || items[0].Name != "demo" {
		t.Fatalf("networks %#v", items)
	}
	if _, err := call(t, b, "network.connect", api.NetworkParams{ID: "net1", Container: "ctr1", Aliases: []string{"web"}}); err != nil || !connected {
		t.Fatalf("connect: %v connected=%v", err, connected)
	}
}

func TestVolumeBackupCleansUtilityAndNormalizesArchive(t *testing.T) {
	var tarData bytes.Buffer
	tw := tar.NewWriter(&tarData)
	if err := tw.WriteHeader(&tar.Header{Name: "ucd/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "ucd/hello.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var removed, safeMount bool
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/volumes/data":
			fmt.Fprint(w, `{"Name":"data","Driver":"local"}`)
		case "/images/busybox:1.37.0/json":
			fmt.Fprint(w, `{"Id":"sha256:abc"}`)
		case "/containers/create":
			var input struct {
				HostConfig struct {
					NetworkMode string
					Mounts      []struct {
						Source        string
						Target        string
						VolumeOptions struct{ NoCopy bool }
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			m := input.HostConfig.Mounts
			safeMount = input.HostConfig.NetworkMode == "none" && len(m) == 1 && m[0].Source == "data" && m[0].Target == "/ucd" && m[0].VolumeOptions.NoCopy
			fmt.Fprint(w, `{"Id":"helper1"}`)
		case "/containers/helper1/start":
			w.WriteHeader(http.StatusNoContent)
		case "/containers/helper1/archive":
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"ucd","mode":2147483648}`)))
			_, _ = w.Write(tarData.Bytes())
		case "/containers/helper1":
			removed = r.Method == http.MethodDelete && r.URL.Query().Get("force") == "1"
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", 500)
		}
	})
	got, err := call(t, b, "volume.backup", api.VolumeResourceParams{Name: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if !removed || !safeMount {
		t.Fatalf("safe mount=%v, cleanup=%v", safeMount, removed)
	}
	backup := got.(api.ResourceData)
	tr := tar.NewReader(bytes.NewReader(backup.Data))
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(tr)
	if err != nil || h.Name != "hello.txt" || string(content) != "hello" {
		t.Fatalf("backup contents: %#v %q %v", h, content, err)
	}
	if n, err := validateVolumeArchive(backup.Data); err != nil || n != 1 {
		t.Fatalf("backup validation %d: %v", n, err)
	}
}

func TestVolumeValidationAndWindowsVersion(t *testing.T) {
	for _, path := range []string{"../outside", "/absolute", `folder\..\outside`, "nul\x00name"} {
		if _, err := volumePath("/ucd", path); err == nil {
			t.Errorf("accepted path %q", path)
		}
	}
	for _, version := range []string{"10.0 20348 (20348.1.amd64fre)", "10.0.26100.123"} {
		if len(kernelVersion.FindStringSubmatch(version)) != 2 {
			t.Errorf("Windows kernel version %q did not match", version)
		}
	}
	for _, link := range []string{"/outside", "../../outside"} {
		var data bytes.Buffer
		writer := tar.NewWriter(&data)
		_ = writer.WriteHeader(&tar.Header{Name: "dir/link", Typeflag: tar.TypeSymlink, Linkname: link, Mode: 0777})
		_ = writer.Close()
		if _, err := validateVolumeArchive(data.Bytes()); err != nil {
			t.Errorf("rejected symlink %q: %v", link, err)
		}
	}
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	_ = writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside", Mode: 0777})
	_ = writer.WriteHeader(&tar.Header{Name: "link/file", Typeflag: tar.TypeReg, Mode: 0644})
	_ = writer.Close()
	if _, err := validateVolumeArchive(data.Bytes()); err == nil {
		t.Fatal("accepted an archive entry under a symlink")
	}
	data.Reset()
	writer = tar.NewWriter(&data)
	_ = writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "missing", Mode: 0644})
	_ = writer.Close()
	if _, err := validateVolumeArchive(data.Bytes()); err == nil {
		t.Fatal("accepted a hard link to a file outside the archive")
	}
	data.Reset()
	writer = tar.NewWriter(&data)
	_ = writer.WriteHeader(&tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644})
	_ = writer.Close()
	data.WriteString("unexpected trailer")
	if _, err := validateVolumeArchive(data.Bytes()); err == nil {
		t.Fatal("accepted nonzero data after the archive")
	}
}

func TestWindowsVolumeUtilityPreparationAndCleanup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows file utility requires a Windows engine service")
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
	tool := []byte("packaged test utility")
	if err := os.WriteFile(toolPath, tool, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(toolPath) })
	for _, test := range []struct {
		name                   string
		failInstall, failStart bool
	}{
		{name: "successful operation"},
		{name: "start failure", failStart: true},
		{name: "installation failure", failInstall: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			steps := make(chan string, 8)
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.45")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch path := strings.TrimPrefix(r.URL.Path, "/v1.45"); path {
				case "/info":
					fmt.Fprint(w, `{"OSType":"windows","KernelVersion":"10.0 20348 (20348.1.amd64fre.fe_release)"}`)
				case "/images/mcr.microsoft.com/windows/servercore:ltsc2022/json":
					steps <- "image"
					fmt.Fprint(w, `{"Id":"sha256:abc"}`)
				case "/containers/create":
					steps <- "create"
					fmt.Fprint(w, `{"Id":"helper-win"}`)
				case "/containers/helper-win/archive":
					if r.Method == http.MethodGet {
						steps <- "inspect utility"
						http.Error(w, "not found", http.StatusNotFound)
						return
					}
					steps <- "install utility"
					if r.Method != http.MethodPut || r.URL.Query().Get("path") != "C:/Windows/Temp" {
						t.Errorf("unexpected utility upload: %s %s", r.Method, r.URL)
					}
					reader := tar.NewReader(r.Body)
					if _, err := reader.Next(); err != nil {
						t.Error(err)
					}
					data, err := io.ReadAll(reader)
					if err != nil || !bytes.Equal(data, tool) {
						t.Errorf("unexpected packaged utility: %v", err)
					}
					if test.failInstall {
						http.Error(w, "utility upload failed", http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusOK)
				case "/containers/helper-win/start":
					steps <- "start"
					if test.failStart {
						http.Error(w, `{"message":"start failed"}`, http.StatusInternalServerError)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case "/containers/helper-win":
					if r.Method != http.MethodDelete {
						t.Errorf("unexpected container request: %s", r.Method)
					}
					steps <- "remove"
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			})
			b.id = "windows"
			result, err := b.withVolumeContainer(context.Background(), "data", func(id, root string) (any, error) {
				if test.failInstall || test.failStart || id != "helper-win" || root != `C:\ucd` {
					t.Errorf("unexpected callback: id=%s root=%s", id, root)
				}
				return "done", nil
			})
			wantSteps := []string{"image", "create", "inspect utility", "install utility", "start", "remove"}
			switch {
			case test.failInstall:
				wantSteps = []string{"image", "create", "inspect utility", "install utility", "remove"}
				if err == nil || !strings.Contains(err.Error(), "prepare Windows volume utility before start") {
					t.Fatalf("unexpected installation error: %v", err)
				}
			case test.failStart:
				if err == nil || !strings.Contains(err.Error(), "start volume utility") {
					t.Fatalf("unexpected start error: %v", err)
				}
			default:
				if err != nil || result != "done" {
					t.Fatalf("utility operation result=%v error=%v", result, err)
				}
			}
			close(steps)
			var got []string
			for step := range steps {
				got = append(got, step)
			}
			if strings.Join(got, ",") != strings.Join(wantSteps, ",") {
				t.Fatalf("utility container order = %v, want %v", got, wantSteps)
			}
		})
	}
}

func TestWindowsVolumeRestoreValidatesBeforeStartingHelper(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows archive filename rules require Windows")
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "CON", Typeflag: tar.TypeReg, Mode: 0644}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := validateVolumeArchive(archive.Bytes()); err != nil {
		t.Fatalf("archive should pass platform-independent validation: %v", err)
	}
	requests := make(chan string, 4)
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v1.45")
		requests <- p
		if p == "/volumes/data" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Name":"data","Driver":"local"}`)
			return
		}
		http.Error(w, "unexpected Docker operation", http.StatusInternalServerError)
	})
	b.id = "windows"
	_, err := call(t, b, "volume.restore", api.VolumeResourceParams{Name: "data", Data: archive.Bytes(), Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "validate Windows volume backup") {
		t.Fatalf("invalid Windows filename was not rejected: %v", err)
	}
	if len(requests) != 1 || <-requests != "/volumes/data" {
		t.Fatal("validation touched Docker beyond inspecting the volume")
	}
}

func TestLimitedVolumeBackup(t *testing.T) {
	var backup limitedVolumeBackup
	if _, err := backup.Write(make([]byte, maxResourceTransfer)); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Write([]byte{1}); err == nil || backup.Len() != maxResourceTransfer {
		t.Fatalf("backup limit not enforced: length=%d error=%v", backup.Len(), err)
	}
}

func TestStorageUsageDoesNotExposeResourceMetadata(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/system/df" {
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"LayersSize":64,"Images":[{"Id":"sha256:abc","Size":64,"SharedSize":0,"Containers":0,"Labels":{"sensitive":"private-metadata"}}],"Containers":[],"Volumes":[],"BuildCache":[]}`)
	})
	got, err := call(t, b, "storage.df", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-metadata")) || !bytes.Contains(data, []byte(`"TotalCount":1`)) {
		t.Fatalf("unexpected aggregate disk usage %s", data)
	}
}

func TestConcurrentRegistryAndImagePush(t *testing.T) {
	t.Setenv("DOCKER_AUTH_CONFIG", "")
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/auth":
			fmt.Fprint(w, `{"Status":"Login Succeeded"}`)
		case "/images/registry.example/asset/push":
			fmt.Fprintln(w, `{"status":"done"}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				var err error
				switch worker {
				case 0:
					_, err = call(t, b, "registry.login", api.RegistryParams{Server: "registry.example", Username: "tester", Password: "dummy-token"})
				case 1:
					_, err = call(t, b, "registry.list", nil)
				case 2:
					_, err = call(t, b, "image.push", api.ImageResourceParams{Reference: "registry.example/asset:latest"})
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestVolumeCloneRejectsExistingTargetBeforeReadingSource(t *testing.T) {
	requests := 0
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/volumes/source":
			fmt.Fprint(w, `{"Name":"source","Driver":"local"}`)
		case "/volumes/target":
			fmt.Fprint(w, `{"Name":"target","Driver":"local"}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	_, err := call(t, b, "volume.clone", api.VolumeResourceParams{Name: "source", Target: "target"})
	if err == nil || !strings.Contains(err.Error(), "already exists") || requests != 2 {
		t.Fatalf("clone collision: requests=%d error=%v", requests, err)
	}
}

func TestImageInspectHistoryTagAndArchiveOperations(t *testing.T) {
	var tagged, imported bool
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/images/demo:old/json":
			fmt.Fprint(w, `{"Id":"sha256:abc","RepoTags":["demo:old"]}`)
		case "/images/demo:old/history":
			fmt.Fprint(w, `[{"Id":"sha256:abc","CreatedBy":"RUN example","Size":42}]`)
		case "/images/demo:old/tag":
			tagged = r.URL.Query().Get("tag") == "new" && strings.HasSuffix(r.URL.Query().Get("repo"), "/demo")
			w.WriteHeader(http.StatusCreated)
		case "/images/load":
			fmt.Fprintln(w, `{"stream":"Loaded image: demo:old\n"}`)
		case "/images/get":
			_, _ = w.Write([]byte("image-archive"))
		case "/images/create":
			imported = r.URL.Query().Get("fromSrc") == "-" && r.URL.Query().Get("repo") == ""
			fmt.Fprintln(w, `{"status":"sha256:imported"}`)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	for _, method := range []string{"image.inspect", "image.history"} {
		if _, err := call(t, b, method, api.ImageResourceParams{Reference: "demo:old"}); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	if _, err := call(t, b, "image.tag", api.ImageResourceParams{Reference: "demo:old", Target: "demo:new"}); err != nil || !tagged {
		t.Fatalf("tag: %v tagged=%v", err, tagged)
	}
	loaded, err := call(t, b, "image.load", api.ImageResourceParams{Data: []byte("archive")})
	if err != nil || !strings.Contains(loaded.(api.ResourceOutput).Output, "Loaded image") {
		t.Fatalf("load: %v", err)
	}
	saved, err := call(t, b, "image.save", api.ImageResourceParams{References: []string{"demo:old"}})
	if err != nil || string(saved.(api.ResourceData).Data) != "image-archive" {
		t.Fatalf("save: %v", err)
	}
	if _, err := call(t, b, "image.import", api.ImageResourceParams{Data: []byte("archive")}); err != nil || !imported {
		t.Fatalf("import without tag: %v imported=%v", err, imported)
	}
}

func TestVolumeCloneStreamsLargeArchive(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	const size = 9 << 20
	if err := writer.WriteHeader(&tar.Header{Name: "ucd/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "ucd/large.bin", Typeflag: tar.TypeReg, Mode: 0644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, size)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var restored int64
	var removed int
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.45") {
		case "/volumes/source":
			fmt.Fprint(w, `{"Name":"source","Driver":"local"}`)
		case "/volumes/target":
			http.Error(w, `{"message":"volume not found"}`, http.StatusNotFound)
		case "/images/busybox:1.37.0/json":
			fmt.Fprint(w, `{"Id":"sha256:abc"}`)
		case "/volumes/create":
			var input struct {
				Name   string
				Labels map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": input.Name, "Driver": "local", "Labels": input.Labels})
		case "/containers/create":
			var input struct {
				HostConfig struct{ Mounts []struct{ Source string } }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if len(input.HostConfig.Mounts) != 1 {
				t.Error("missing mount")
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "helper-" + input.HostConfig.Mounts[0].Source})
		case "/containers/helper-source/start", "/containers/helper-target/start":
			w.WriteHeader(http.StatusNoContent)
		case "/containers/helper-source/archive":
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"ucd","mode":2147483648}`)))
			_, _ = w.Write(archive.Bytes())
		case "/containers/helper-target/archive":
			reader := tar.NewReader(r.Body)
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				if header.Name == "large.bin" {
					restored, err = io.Copy(io.Discard, reader)
					if err != nil {
						t.Error(err)
					}
				}
			}
			w.WriteHeader(http.StatusOK)
		case "/containers/helper-source", "/containers/helper-target":
			removed++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	if _, err := call(t, b, "volume.clone", api.VolumeResourceParams{Name: "source", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	if restored != size || removed != 2 {
		t.Fatalf("restored %d bytes, cleaned %d utility containers", restored, removed)
	}
	files, err := filepath.Glob(filepath.Join(b.dataRoot, ".ucd-volume-clone-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("clone temporary files remain: %v %v", files, err)
	}
}

func TestWindowsVolumeCloneUsesNativeStreams(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows volume utility requires a Windows engine service")
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

	const content = "cloned volume contents"
	var mu sync.Mutex
	started, installed := map[string]bool{}, map[string]bool{}
	var commands = map[string][]string{}
	var restored string
	var removed int
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		p := dockerTestPath(w, r)
		w.Header().Set("Content-Type", "application/json")
		switch p {
		case "":
		case "/volumes/source":
			fmt.Fprint(w, `{"Name":"source","Driver":"local"}`)
		case "/volumes/target":
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case "/info":
			fmt.Fprint(w, `{"KernelVersion":"10.0.20348.1"}`)
		case "/images/mcr.microsoft.com/windows/servercore:ltsc2022/json":
			fmt.Fprint(w, `{"Id":"image"}`)
		case "/volumes/create":
			var input struct {
				Name   string
				Labels map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": input.Name, "Driver": "local", "Labels": input.Labels})
		case "/containers/create":
			var input struct {
				HostConfig struct{ Mounts []struct{ Source string } }
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.HostConfig.Mounts) != 1 {
				t.Errorf("invalid helper configuration: %v", err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": "helper-" + input.HostConfig.Mounts[0].Source})
		case "/containers/helper-source/archive", "/containers/helper-target/archive":
			id := strings.Split(p, "/")[2]
			mu.Lock()
			isStarted := started[id]
			if r.Method == http.MethodPut {
				installed[id] = true
			}
			mu.Unlock()
			if isStarted {
				t.Error("Docker archive API used after helper start")
				http.Error(w, "archive unavailable", http.StatusInternalServerError)
				return
			}
			if r.Method == http.MethodGet {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		case "/containers/helper-source/start", "/containers/helper-target/start":
			id := strings.Split(p, "/")[2]
			mu.Lock()
			if !installed[id] {
				t.Error("helper started before utility installation")
			}
			started[id] = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "/containers/helper-source/exec", "/containers/helper-target/exec":
			var input struct{ Cmd []string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			id := strings.Split(p, "/")[2]
			mu.Lock()
			commands[id] = input.Cmd
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case "/exec/helper-source/start", "/exec/helper-target/start":
			_, _ = io.Copy(io.Discard, r.Body)
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprint(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\n\r\n")
			_ = rw.Flush()
			id := strings.Split(p, "/")[2]
			mu.Lock()
			command := append([]string(nil), commands[id]...)
			mu.Unlock()
			if id == "helper-source" {
				if len(command) != 3 || command[1] != "archive" || command[2] != `C:\ucd` {
					t.Errorf("unexpected backup command: %v", command)
					return
				}
				tw := tar.NewWriter(dockerFrameWriter{Writer: conn})
				if err := tw.WriteHeader(&tar.Header{Name: "ucd/file.txt", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(content))}); err != nil {
					t.Error(err)
					return
				}
				if _, err := tw.Write([]byte(content)); err != nil {
					t.Error(err)
				}
				if err := tw.Close(); err != nil {
					t.Error(err)
				}
				return
			}
			if len(command) != 4 || command[1] != "extract" || command[2] != `C:\ucd` {
				t.Errorf("unexpected restore command: %v", command)
				return
			}
			size, err := strconv.ParseInt(command[3], 10, 64)
			if err != nil {
				t.Error(err)
				return
			}
			input := &io.LimitedReader{R: rw, N: size}
			reader := tar.NewReader(input)
			header, err := reader.Next()
			if err != nil || header.Name != "file.txt" {
				t.Errorf("restored header = %#v, error = %v", header, err)
				return
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Error(err)
			}
			_, err = io.Copy(io.Discard, input)
			if err != nil || input.N != 0 {
				t.Errorf("incorrect restore size: remaining=%d error=%v", input.N, err)
			}
			mu.Lock()
			restored = string(data)
			mu.Unlock()
		case "/exec/helper-source/json", "/exec/helper-target/json":
			fmt.Fprint(w, `{"Running":false,"ExitCode":0}`)
		case "/containers/helper-source", "/containers/helper-target":
			mu.Lock()
			removed++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
	b.id = "windows"
	if _, err := call(t, b, "volume.clone", api.VolumeResourceParams{Name: "source", Target: "target"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotContent, gotRemoved := restored, removed
	mu.Unlock()
	if gotContent != content || gotRemoved != 2 {
		t.Fatalf("restored %q and removed %d utility containers", gotContent, gotRemoved)
	}
	files, err := filepath.Glob(filepath.Join(b.dataRoot, ".ucd-volume-clone-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("clone temporary files remain: %v %v", files, err)
	}
}

func TestStreamedImageBuildFiltersLargeContextAndUsesEditedDockerfile(t *testing.T) {
	var source bytes.Buffer
	writer := tar.NewWriter(&source)
	add := func(name string, data []byte) {
		t.Helper()
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add("build/Customfile", []byte("original recipe"))
	add(".dockerignore", []byte("root-only\n"))
	add("build/Customfile.dockerignore", []byte("secret/\n!secret/keep.txt\n"))
	add("secret/private", []byte("do-not-send"))
	add("secret/keep.txt", []byte("keep"))
	add("root-only", []byte("specific-ignore-takes-precedence"))
	add("big.bin", bytes.Repeat([]byte{'x'}, 9<<20))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	var bigSize int64
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/v1.45") != "/build" {
			http.Error(w, "unexpected", 500)
			return
		}
		if r.URL.Query().Get("dockerfile") != "build/Customfile" {
			t.Error("incorrect selected Dockerfile path")
		}
		reader := tar.NewReader(r.Body)
		for {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			if header.Name == "big.bin" {
				bigSize, err = io.Copy(io.Discard, reader)
				if err != nil {
					t.Error(err)
				}
				continue
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Error(err)
				return
			}
			seen[header.Name] = string(data)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"stream":"build completed\n"}`)
	})
	result, err := b.streamImageBuild(context.Background(), bytes.NewReader(source.Bytes()), api.TransferOpenParams{DockerfilePath: "build/Customfile", Dockerfile: "FROM scratch\nCOPY big.bin /big.bin\n", Tags: []string{"demo:latest"}, OperationID: "build-large"})
	if err != nil {
		t.Fatal(err)
	}
	if bigSize != 9<<20 || seen["build/Customfile"] != "FROM scratch\nCOPY big.bin /big.bin\n" || seen["secret/private"] != "" || seen["secret/keep.txt"] != "keep" || seen["root-only"] != "specific-ignore-takes-precedence" || !strings.Contains(result.Output, "build completed") {
		t.Fatalf("streamed build: big=%d entries=%v output=%q", bigSize, seen, result.Output)
	}
	files, err := filepath.Glob(filepath.Join(b.dataRoot, ".ucd-build-context-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("build temporary files remain: %v %v", files, err)
	}
}

func TestStreamedImageBuildRejectsUnsafeArchives(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}}},
		{"symlink", []*tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside", Mode: 0777}}},
		{"duplicate", []*tar.Header{{Name: "same", Typeflag: tar.TypeReg, Mode: 0644}, {Name: "same", Typeflag: tar.TypeReg, Mode: 0644}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			writer := tar.NewWriter(&archive)
			for _, header := range tc.headers {
				if err := writer.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectBuildArchive(context.Background(), bytes.NewReader(archive.Bytes()), "Dockerfile"); err == nil {
				t.Fatal("unsafe build context accepted")
			}
		})
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	_ = writer.Close()
	archive.WriteString("unexpected")
	if _, err := inspectBuildArchive(context.Background(), bytes.NewReader(archive.Bytes()), "Dockerfile"); err == nil {
		t.Fatal("trailing data accepted")
	}
}
