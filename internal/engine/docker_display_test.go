package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/skyri/ucd/internal/api"
)

const displayContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func windowsPortLabel(t *testing.T, ports ...api.Port) string {
	t.Helper()
	data, err := json.Marshal(ports)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func setDisplayForward(b *Backend, id string, index int, port api.Port) {
	b.portMu.Lock()
	defer b.portMu.Unlock()
	if b.forwards == nil {
		b.forwards = make(map[forwardKey]*portForward)
	}
	b.forwards[forwardKey{container: id, index: index}] = &portForward{
		port:        port,
		cancel:      func() {},
		connections: make(map[net.Conn]struct{}),
		sessions:    make(map[string]*net.UDPConn),
	}
}

func TestDockerDisplayKindSelectsOnlyFiniteContainerEndpoints(t *testing.T) {
	for _, test := range []struct {
		method string
		path   string
		want   dockerDisplayKind
	}{
		{http.MethodGet, "/v1.45/containers/json", dockerDisplayList},
		{http.MethodGet, "/containers/web/json", dockerDisplayInspect},
		{http.MethodGet, "/v1.45/containers/short-id/json", dockerDisplayInspect},
		{http.MethodGet, "/containers/web/archive", dockerDisplayNone},
		{http.MethodGet, "/containers/web/port/80", dockerDisplayNone},
		{http.MethodGet, "/containers/web/exec/json", dockerDisplayNone},
		{http.MethodPost, "/containers/json", dockerDisplayNone},
		{http.MethodGet, "/events", dockerDisplayNone},
	} {
		if got := dockerDisplayKindFor(test.method, test.path); got != test.want {
			t.Errorf("dockerDisplayKindFor(%s %s) = %v, want %v", test.method, test.path, got, test.want)
		}
	}
}

func TestDockerPolicyDecoratesOnlyActivePublishedPortsInList(t *testing.T) {
	requested := []api.Port{
		{HostIP: "127.0.0.1", Published: 0, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Published: 5353, Target: 53, Protocol: "udp"},
	}
	active := api.Port{HostIP: "127.0.0.1", Published: 49123, Target: 80, Protocol: "tcp"}
	b := &Backend{id: "windows"}
	setDisplayForward(b, displayContainerID, 0, active)
	t.Cleanup(b.closeWindowsPorts)
	label := windowsPortLabel(t, requested...)
	original := fmt.Sprintf(`[{"Id":%q,"Names":["/web"],"State":"running","Labels":{"%s":%q,"custom":"keep"},"Ports":[{"PrivatePort":80,"Type":"tcp","futurePortField":true}],"future":{"nested":"keep"}}]`, displayContainerID, windowsPortsLabel, label)
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Docker-Header", "keep")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, original)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.45/containers/json?all=1", nil))
	if response.Code != http.StatusOK || response.Header().Get("X-Docker-Header") != "keep" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("response metadata = %d %v", response.Code, response.Header())
	}
	var before, after []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || !bytes.Equal(before[0]["future"], after[0]["future"]) {
		t.Fatalf("unknown list fields changed: %s", response.Body.String())
	}
	var ports []json.RawMessage
	if err := json.Unmarshal(after[0]["Ports"], &ports); err != nil {
		t.Fatal(err)
	}
	var existingPort map[string]json.RawMessage
	if len(ports) > 0 {
		if err := json.Unmarshal(ports[0], &existingPort); err != nil {
			t.Fatal(err)
		}
	}
	if len(ports) != 2 || !bytes.Equal(existingPort["futurePortField"], []byte("true")) {
		t.Fatalf("existing Docker port fields changed or active port missing: %s", response.Body.String())
	}
	var actual struct {
		IP          string
		PrivatePort uint16
		PublicPort  uint16
		Type        string
	}
	if err := json.Unmarshal(ports[1], &actual); err != nil || actual.IP != active.HostIP || actual.PrivatePort != active.Target || actual.PublicPort != active.Published || actual.Type != active.Protocol {
		t.Fatalf("active port = %+v (%v), body %s", actual, err, response.Body.String())
	}
	if got, want := response.Header().Get("Content-Length"), strconv.Itoa(response.Body.Len()); got != want {
		t.Fatalf("content length = %q, want %q", got, want)
	}
}

func TestDockerPolicyInspectResolvesShortIDAndNameFromResponse(t *testing.T) {
	requested := []api.Port{
		{HostIP: "127.0.0.1", Published: 8080, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Target: 53, Protocol: "udp"},
	}
	active := api.Port{HostIP: "127.0.0.1", Published: 8080, Target: 80, Protocol: "tcp"}
	b := &Backend{id: "windows"}
	setDisplayForward(b, displayContainerID, 0, active)
	t.Cleanup(b.closeWindowsPorts)
	label := windowsPortLabel(t, requested...)
	original := fmt.Sprintf(`{"Id":%q,"State":{"Running":true},"Config":{"Labels":{"%s":%q},"futureConfig":"keep"},"HostConfig":{"PortBindings":null,"futureHost":"keep"},"NetworkSettings":{"Ports":{"80/tcp":null,"53/udp":null,"9999/tcp":[{"HostIp":"127.0.0.1","HostPort":"9999"}]},"futureNetwork":"keep"},"futureTop":{"field":"keep"}}`, displayContainerID, windowsPortsLabel, label)
	for _, path := range []string{"/v1.45/containers/012345/json", "/containers/web/json"} {
		t.Run(path, func(t *testing.T) {
			handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Docker-Header", "keep")
				_, _ = io.WriteString(w, original)
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusOK || response.Header().Get("X-Docker-Header") != "keep" {
				t.Fatalf("response metadata = %d %v", response.Code, response.Header())
			}
			var got struct {
				HostConfig struct {
					PortBindings map[string][]struct{ HostIP, HostPort string }
					FutureHost   string
				}
				NetworkSettings struct {
					Ports         map[string][]struct{ HostIP, HostPort string }
					FutureNetwork string
				}
				FutureTop map[string]string
				Config    map[string]json.RawMessage
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.HostConfig.FutureHost != "keep" || got.NetworkSettings.FutureNetwork != "keep" || got.FutureTop["field"] != "keep" || string(got.Config["futureConfig"]) != `"keep"` {
				t.Fatalf("unknown inspect fields changed: %s", response.Body.String())
			}
			if bindings := got.HostConfig.PortBindings["80/tcp"]; len(bindings) != 1 || bindings[0].HostPort != "8080" || bindings[0].HostIP != "127.0.0.1" {
				t.Fatalf("requested host binding = %#v", bindings)
			}
			if bindings := got.HostConfig.PortBindings["53/udp"]; len(bindings) != 1 || bindings[0].HostPort != "" {
				t.Fatalf("auto host binding = %#v", bindings)
			}
			if bindings := got.NetworkSettings.Ports["80/tcp"]; len(bindings) != 1 || bindings[0].HostPort != strconv.Itoa(int(active.Published)) {
				t.Fatalf("active network binding = %#v", bindings)
			}
			if _, ok := got.NetworkSettings.Ports["53/udp"]; ok {
				t.Fatalf("inactive port binding was exposed: %#v", got.NetworkSettings.Ports)
			}
			if bindings := got.NetworkSettings.Ports["9999/tcp"]; len(bindings) != 1 || bindings[0].HostPort != "9999" {
				t.Fatalf("unmanaged network binding changed: %#v", bindings)
			}
		})
	}
}

func TestDockerPolicyPreservesErrorResponsesAndGzip(t *testing.T) {
	b := &Backend{id: "windows"}
	label := windowsPortLabel(t, api.Port{HostIP: "127.0.0.1", Published: 0, Target: 80, Protocol: "tcp"})
	body := fmt.Sprintf(`[{"Id":%q,"State":"running","Labels":{"%s":%q},"Ports":[]}]`, displayContainerID, windowsPortsLabel, label)
	compressed, err := gzipDockerDisplayBody([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(compressed)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/containers/json", nil))
	if response.Code != http.StatusNotFound || response.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(response.Body.Bytes(), compressed) {
		t.Fatalf("error response changed: status %d headers %v", response.Code, response.Header())
	}

	noMetadata, err := gzipDockerDisplayBody([]byte(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	handler = b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(noMetadata)))
		_, _ = w.Write(noMetadata)
	}))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/containers/json", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Content-Length") != strconv.Itoa(len(noMetadata)) || !bytes.Equal(response.Body.Bytes(), noMetadata) {
		t.Fatalf("unmodified gzip response changed: status %d headers %v body %x", response.Code, response.Header(), response.Body.Bytes())
	}

	b.forwards = map[forwardKey]*portForward{}
	setDisplayForward(b, displayContainerID, 0, api.Port{HostIP: "127.0.0.1", Published: 49000, Target: 80, Protocol: "tcp"})
	t.Cleanup(b.closeWindowsPorts)
	handler = b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed)
	}))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/containers/json", nil))
	if response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("gzip response metadata = %v", response.Header())
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		Ports []struct {
			PublicPort uint16 `json:"PublicPort"`
		} `json:"Ports"`
	}
	if err := json.Unmarshal(decoded, &list); err != nil || len(list) != 1 || len(list[0].Ports) != 1 || list[0].Ports[0].PublicPort != 49000 {
		t.Fatalf("gzip decorated list = %s (%v)", decoded, err)
	}
}

func TestDockerDisplayWriterBoundsResponseBody(t *testing.T) {
	writer := newDockerDisplayWriter(4)
	if _, err := writer.Write([]byte("12345")); !errors.Is(err, errDockerDisplayResponseTooLarge) || !writer.tooLarge {
		t.Fatalf("oversized write = %v, tooLarge %v", err, writer.tooLarge)
	}
}

func TestManagementContainerInspectDecoratesRequestedAndActivePorts(t *testing.T) {
	requested := []api.Port{
		{HostIP: "127.0.0.1", Published: 8080, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.2", Published: 8081, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Target: 53, Protocol: "udp"},
	}
	label := windowsPortLabel(t, requested...)
	for _, running := range []bool{false, true} {
		name := "stopped"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			body := fmt.Sprintf(`{"Id":%q,"State":{"Running":%t},"Config":{"Labels":{"%s":%q}},"HostConfig":{"PortBindings":{"80/tcp":null}},"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"9999"}],"53/udp":null,"9999/tcp":[{"HostIp":"127.0.0.1","HostPort":"9999"}]}}}`, displayContainerID, running, windowsPortsLabel, label)
			b := windowsTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.45")
					w.WriteHeader(http.StatusOK)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/json") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, body)
					return
				}
				http.NotFound(w, r)
			})
			if running {
				setDisplayForward(b, displayContainerID, 0, api.Port{HostIP: "127.0.0.1", Published: 8080, Target: 80, Protocol: "tcp"})
				setDisplayForward(b, displayContainerID, 1, api.Port{HostIP: "127.0.0.2", Published: 8081, Target: 80, Protocol: "tcp"})
				t.Cleanup(b.closeWindowsPorts)
			}
			result, err := call(t, b, "container.inspect", api.ContainerParams{ID: "web"})
			if err != nil {
				t.Fatal(err)
			}
			inspect := result.(container.InspectResponse)
			managedTCP := network.MustParsePort("80/tcp")
			if bindings := inspect.HostConfig.PortBindings[managedTCP]; len(bindings) != 2 || bindings[0].HostPort != "8080" || bindings[1].HostPort != "8081" {
				t.Fatalf("requested host binding = %#v", bindings)
			}
			managedUDP := network.MustParsePort("53/udp")
			if bindings := inspect.HostConfig.PortBindings[managedUDP]; len(bindings) != 1 || bindings[0].HostPort != "" {
				t.Fatalf("automatic host binding = %#v", bindings)
			}
			if running {
				if bindings := inspect.NetworkSettings.Ports[managedTCP]; len(bindings) != 2 || bindings[0].HostPort != "8080" || bindings[1].HostPort != "8081" {
					t.Fatalf("active network binding = %#v", bindings)
				}
			} else if _, ok := inspect.NetworkSettings.Ports[managedTCP]; ok {
				t.Fatalf("stopped container retained active network binding: %#v", inspect.NetworkSettings.Ports)
			}
			if _, ok := inspect.NetworkSettings.Ports[managedUDP]; ok {
				t.Fatalf("inactive network binding reported: %#v", inspect.NetworkSettings.Ports)
			}
			if bindings := inspect.NetworkSettings.Ports[network.MustParsePort("9999/tcp")]; len(bindings) != 1 || bindings[0].HostPort != "9999" {
				t.Fatalf("unmanaged network binding changed: %#v", bindings)
			}
		})
	}
}

func TestManagementInspectLeavesLinuxAndUnmanagedContainersUntouched(t *testing.T) {
	for _, engineID := range []string{"linux", "windows"} {
		t.Run(engineID, func(t *testing.T) {
			inspect := container.InspectResponse{ID: "container", Config: &container.Config{Labels: map[string]string{"custom": "keep"}}}
			b := &Backend{id: engineID}
			if err := b.decorateManagedInspectPorts(&inspect); err != nil {
				t.Fatal(err)
			}
			if inspect.HostConfig != nil || inspect.NetworkSettings != nil {
				t.Fatalf("unmanaged inspection was changed: %#v", inspect)
			}
		})
	}
}
