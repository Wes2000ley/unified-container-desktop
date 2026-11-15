package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
)

func TestNativeDockerCreateDNSDefaultsWithoutSettingsSave(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       []string
	}{
		{"new default network", `{"Image":"test"}`, []string{dnsfilter.IPv4Address, dnsfilter.IPv6Address}},
		{"explicit resolver", `{"Image":"test","HostConfig":{"DNS":["192.0.2.53"]}}`, []string{"192.0.2.53"}},
		{"host resolver", `{"Image":"test","HostConfig":{"NetworkMode":"host"}}`, nil},
		{"no networking", `{"Image":"test","HostConfig":{"NetworkMode":"none"}}`, nil},
		{"shared namespace", `{"Image":"test","HostConfig":{"NetworkMode":"container:other"}}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := api.DefaultNetworkSettings()
			settings.HostNetworkingEnabled = true
			b := &Backend{id: "linux", runtimeConfig: api.EngineConfiguration{Network: settings}}
			forwarded := false
			handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded = true
				var config struct{ HostConfig struct{ DNS []string } }
				if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(config.HostConfig.DNS, test.want) {
					t.Fatalf("DNS = %v, want %v", config.HostConfig.DNS, test.want)
				}
				w.WriteHeader(http.StatusCreated)
			}))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader(test.body)))
			if !forwarded || w.Code != http.StatusCreated {
				t.Fatalf("create failed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeDockerCreateUsesMachineDefaults(t *testing.T) {
	b := &Backend{id: "linux", runtimeConfig: api.EngineConfiguration{
		Network:         api.DefaultNetworkSettings(),
		ContainersProxy: api.ProxySettings{HTTP: "http://proxy.example:8080", HTTPS: "http://secure.example:8080"},
	}}
	forwarded := false
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != r.ContentLength {
			t.Fatal("rewritten body has stale length")
		}
		var got struct {
			Env        []string
			Labels     map[string]string
			Future     bool
			HostConfig struct {
				FutureHost   bool
				PortBindings map[string][]struct{ HostIP, HostPort string }
			}
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(got.Env, "\n")
		if !strings.Contains(joined, "HTTPS_PROXY=http://explicit.example") || !strings.Contains(joined, "HTTP_PROXY=http://proxy.example:8080") {
			t.Fatalf("proxy defaults lost: %v", got.Env)
		}
		if !got.Future || !got.HostConfig.FutureHost {
			t.Fatal("unknown Docker fields were dropped")
		}
		bindings := got.HostConfig.PortBindings["80/tcp"]
		if len(bindings) != 2 || bindings[0].HostIP != "127.0.0.1" || bindings[0].HostPort != "0" ||
			bindings[1].HostIP != "::1" || bindings[1].HostPort != "0" {
			t.Fatalf("Linux relay binding = %#v", bindings)
		}
		ports, err := parseLinuxPortMetadata(got.Labels[linuxPortsLabel])
		if err != nil || len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || ports[0].Published != 18432 {
			t.Fatalf("original Docker request metadata = %#v, %v", ports, err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1.53/containers/create", strings.NewReader(`{"Image":"busybox:1.37.0","Env":["HTTPS_PROXY=http://explicit.example"],"Future":true,"hostconfig":{"FutureHost":true,"PortBindings":{"80/tcp":[{"HostPort":"18432"}]}}}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if !forwarded || w.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", w.Code, w.Body.String())
	}
}

func TestNativeDockerHostNetworkingRequiresOptIn(t *testing.T) {
	for _, engineID := range []string{"linux", "windows"} {
		t.Run(engineID, func(t *testing.T) {
			b := &Backend{id: engineID, runtimeConfig: api.EngineConfiguration{Network: api.DefaultNetworkSettings()}}
			handler := b.DockerPolicyHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("disabled host networking reached daemon") }))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader(`{"Image":"test","HostConfig":{"NetworkMode":"host"}}`)))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNativeDockerBuildKeepsStreamingBodyAndExplicitArguments(t *testing.T) {
	b := &Backend{id: "linux", runtimeConfig: api.EngineConfiguration{ContainersProxy: api.ProxySettings{HTTP: "http://default.example", SOCKS5: "socks5://socks.example:1080"}}}
	r := httptest.NewRequest(http.MethodPost, "/v1.53/build", strings.NewReader("streaming build archive"))
	query := r.URL.Query()
	query.Set("buildargs", `{"HTTP_PROXY":"http://explicit.example","CUSTOM":"value"}`)
	r.URL.RawQuery = query.Encode()
	body := r.Body
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, forwarded *http.Request) {
		if forwarded.Body != body {
			t.Fatal("build archive was buffered or replaced")
		}
		var args map[string]*string
		if err := json.Unmarshal([]byte(forwarded.URL.Query().Get("buildargs")), &args); err != nil {
			t.Fatal(err)
		}
		if args["HTTP_PROXY"] == nil || *args["HTTP_PROXY"] != "http://explicit.example" || args["ALL_PROXY"] == nil || *args["ALL_PROXY"] != "socks5://socks.example:1080" || *args["CUSTOM"] != "value" {
			t.Fatalf("build proxy arguments differ: %#v", args)
		}
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("build failed: %d %s", w.Code, w.Body.String())
	}
}

func TestNativeDockerPolicyRejectsAmbiguousControlledFields(t *testing.T) {
	b := &Backend{id: "linux", runtimeConfig: api.EngineConfiguration{Network: api.DefaultNetworkSettings()}}
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("ambiguous request reached daemon") }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/containers/create", strings.NewReader(`{"Image":"test","HostConfig":{},"hostconfig":{"NetworkMode":"host"}}`)))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "ambiguous") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestNativeDockerNetworkDefaultsPreserveExplicitFamilies(t *testing.T) {
	settings := api.DefaultNetworkSettings()
	settings.DefaultNetworkingMode = api.NetworkModeIPv6Only
	b := &Backend{id: "linux", runtimeConfig: api.EngineConfiguration{Network: settings}}
	handler := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			EnableIPv4, EnableIPv6 *bool
			Future                 string
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.EnableIPv4 == nil || !*got.EnableIPv4 || got.EnableIPv6 == nil || !*got.EnableIPv6 || got.Future != "kept" {
			t.Fatalf("network defaults/override differ: %#v", got)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/networks/create", strings.NewReader(`{"Name":"test","EnableIPv4":true,"Future":"kept"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("network failed: %d %s", w.Code, w.Body.String())
	}
}
