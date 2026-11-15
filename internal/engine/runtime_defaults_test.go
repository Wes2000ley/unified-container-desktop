package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

func TestApplyNetworkDefaults(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		driver     string
		ipv4       *bool
		ipv6       *bool
		wantIPv4   bool
		wantIPv6   bool
		wantChange bool
	}{
		{"defaults IPv4-only", api.NetworkModeIPv4Only, "", nil, nil, true, false, true},
		{"defaults dual-stack", api.NetworkModeDualStack, "bridge", nil, nil, true, true, true},
		{"defaults IPv6-only", api.NetworkModeIPv6Only, "bridge", nil, nil, false, true, true},
		{"preserves caller families", api.NetworkModeIPv6Only, "bridge", boolPointer(true), boolPointer(false), true, false, true},
		{"preserves explicit driver", api.NetworkModeDualStack, "macvlan", nil, nil, false, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := &network.CreateRequest{Driver: test.driver, EnableIPv4: test.ipv4, EnableIPv6: test.ipv6}
			if err := ApplyNetworkDefaults(request, networkWithMode(test.mode)); err != nil {
				t.Fatal(err)
			}
			if !test.wantChange {
				if request.EnableIPv4 != nil || request.EnableIPv6 != nil {
					t.Fatalf("custom driver request changed: %#v", request)
				}
				if len(request.Labels) != 0 {
					t.Fatalf("global DNS policy should not be stored as a network override: %#v", request.Labels)
				}
				return
			}
			if len(request.Labels) != 0 {
				t.Fatalf("global DNS policy should not be stored as a network override: %#v", request.Labels)
			}
			if request.EnableIPv4 == nil || request.EnableIPv6 == nil || *request.EnableIPv4 != test.wantIPv4 || *request.EnableIPv6 != test.wantIPv6 {
				t.Fatalf("network families = %v/%v, want %t/%t", pointerValue(request.EnableIPv4), pointerValue(request.EnableIPv6), test.wantIPv4, test.wantIPv6)
			}
		})
	}
}

func TestApplyNetworkDefaultsRetainsConfigOnlyAndExplicitFamily(t *testing.T) {
	settings := networkWithMode(api.NetworkModeDualStack)
	for _, request := range []*network.CreateRequest{
		{ConfigOnly: true},
		{ConfigFrom: &network.ConfigReference{Network: "base"}},
	} {
		if err := ApplyNetworkDefaults(request, settings); err != nil {
			t.Fatal(err)
		}
		if request.EnableIPv4 != nil || request.EnableIPv6 != nil || request.Labels != nil {
			t.Fatalf("configuration request changed: %#v", request)
		}
	}
	request := &network.CreateRequest{EnableIPv6: boolPointer(false)}
	if err := ApplyNetworkDefaults(request, settings); err != nil {
		t.Fatal(err)
	}
	if request.EnableIPv4 == nil || !*request.EnableIPv4 || *request.EnableIPv6 {
		t.Fatalf("explicit IPv6 override was lost: %#v", request)
	}
}

func TestApplyNetworkDefaultsConsumesDNSOverride(t *testing.T) {
	request := &network.CreateRequest{Driver: "bridge", Options: map[string]string{dnsInhibitionLabel: api.DNSInhibitionIPv6, "mtu": "1400"}}
	if err := ApplyNetworkDefaults(request, api.DefaultNetworkSettings()); err != nil {
		t.Fatal(err)
	}
	if _, exists := request.Options[dnsInhibitionLabel]; exists {
		t.Fatalf("DNS policy leaked into driver options: %#v", request.Options)
	}
	if request.Options["mtu"] != "1400" || request.Labels[dnsInhibitionLabel] != api.DNSInhibitionIPv6 {
		t.Fatalf("network options/labels = %#v/%#v", request.Options, request.Labels)
	}
	for _, value := range []string{"bogus", "AUTO", ""} {
		request := &network.CreateRequest{Options: map[string]string{dnsInhibitionLabel: value}}
		if err := ApplyNetworkDefaults(request, api.DefaultNetworkSettings()); err == nil {
			t.Errorf("accepted invalid DNS policy %q", value)
		}
	}
	request = &network.CreateRequest{Labels: map[string]string{dnsInhibitionLabel: api.DNSInhibitionNone}}
	if err := ApplyNetworkDefaults(request, api.DefaultNetworkSettings()); err == nil {
		t.Fatal("accepted caller-provided reserved DNS label")
	}
	request = &network.CreateRequest{ConfigOnly: true, Options: map[string]string{dnsInhibitionLabel: api.DNSInhibitionNone}}
	if err := ApplyNetworkDefaults(request, api.DefaultNetworkSettings()); err == nil {
		t.Fatal("accepted per-network DNS policy on a config-only network")
	}
	request = &network.CreateRequest{Driver: "macvlan", Options: map[string]string{dnsInhibitionLabel: api.DNSInhibitionIPv4}}
	if err := ApplyNetworkDefaults(request, api.DefaultNetworkSettings()); err == nil {
		t.Fatal("accepted per-network DNS policy on a non-bridge network")
	}
}

func TestValidateContainerNetworkMode(t *testing.T) {
	settings := networkWithMode(api.NetworkModeIPv4Only)
	for _, test := range []struct {
		name    string
		engine  string
		mode    string
		enable  bool
		wantErr bool
	}{
		{"Linux bridge", "linux", "bridge", false, false},
		{"Linux host opt-in disabled", "linux", "host", false, true},
		{"Linux host opt-in enabled", "linux", "host", true, false},
		{"Windows host unsupported", "windows", "host", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings.HostNetworkingEnabled = test.enable
			err := ValidateContainerNetworkMode(test.engine, test.mode, settings)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestComposeContainerCreateTrustsManagedWindowsPorts(t *testing.T) {
	service := types.ServiceConfig{}
	service.Ports = []types.ServicePortConfig{{Target: 80, Published: "8080"}}
	runtimeProject, err := windowsRuntimeProject(&types.Project{Services: types.Services{"web": service}})
	if err != nil {
		t.Fatal(err)
	}
	wantLabel := runtimeProject.Services["web"].Labels[windowsPortsLabel]
	if wantLabel == "" {
		t.Fatal("Windows Compose port metadata was not generated")
	}

	server := httptest.NewServer(windowsAliasFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			w.Header().Set("OSType", "windows")
			w.WriteHeader(http.StatusOK)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/containers/create") {
			t.Errorf("unexpected Docker request %s", r.URL)
			http.NotFound(w, r)
			return
		}
		var body struct {
			Labels map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode container create request: %v", err)
		} else if body.Labels[windowsPortsLabel] != wantLabel {
			t.Errorf("managed published port label = %q, want %q", body.Labels[windowsPortsLabel], wantLabel)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"compose-container"}`))
	}, nil))
	defer server.Close()
	b, err := New("windows", "tcp://"+strings.TrimPrefix(server.URL, "http://"), t.TempDir(), &api.EventBus{})
	if err != nil {
		t.Fatal(err)
	}
	b.hostIPv4 = testHostIPv4
	defer b.Close()

	composeClient := &composeDefaultsClient{APIClient: b.docker, backend: b}
	result, err := composeClient.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Config:     &container.Config{Image: "example", Labels: runtimeProject.Services["web"].Labels},
		HostConfig: &container.HostConfig{},
	})
	if err != nil || result.ID != "compose-container" {
		t.Fatalf("Compose container create = %#v, %v", result, err)
	}

	_, err = call(t, b, "container.create", api.ContainerCreateParams{
		Config: &container.Config{Image: "example", Labels: runtimeProject.Services["web"].Labels},
	})
	if err == nil || !strings.Contains(err.Error(), "reserved published port label") {
		t.Fatalf("raw API reserved label error = %v", err)
	}
}

func TestApplyContainerProxyEnvironmentPreservesExplicitValues(t *testing.T) {
	proxy := api.ProxySettings{
		HTTP: "http://proxy.example:8080", HTTPS: "http://proxy.example:8080",
		SOCKS5: "socks5://proxy.example:1080", NoProxy: "localhost,.example.com",
	}
	existing := []string{"PATH=/usr/bin", "http_proxy=http://override.example", "HTTP_PROXY=http://proxy.example:8080", "No_Proxy=custom"}
	got := ApplyContainerProxyEnvironment(existing, proxy)
	want := []string{
		"PATH=/usr/bin", "http_proxy=http://override.example", "HTTP_PROXY=http://proxy.example:8080", "No_Proxy=custom",
		"HTTPS_PROXY=http://proxy.example:8080", "https_proxy=http://proxy.example:8080",
		"ALL_PROXY=socks5://proxy.example:1080", "all_proxy=socks5://proxy.example:1080",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(existing, []string{"PATH=/usr/bin", "http_proxy=http://override.example", "HTTP_PROXY=http://proxy.example:8080", "No_Proxy=custom"}) {
		t.Fatalf("input environment mutated: %#v", existing)
	}
}

func TestApplyContainerDNSDefaults(t *testing.T) {
	for _, mode := range []string{"", "bridge", "ucd-net"} {
		host := ApplyContainerDNSDefaults("linux", &container.HostConfig{NetworkMode: container.NetworkMode(mode)})
		if len(host.DNS) != 2 || host.DNS[0].String() != "169.254.253.53" || host.DNS[1].String() != "fd42:7763:b25b:ffff::53" {
			t.Errorf("mode %q DNS = %v", mode, host.DNS)
		}
	}
	for _, mode := range []string{"host", "none", "container:other"} {
		host := ApplyContainerDNSDefaults("linux", &container.HostConfig{NetworkMode: container.NetworkMode(mode)})
		if len(host.DNS) != 0 {
			t.Errorf("mode %q unexpectedly received DNS defaults: %v", mode, host.DNS)
		}
	}
	explicit := []netip.Addr{netip.MustParseAddr("192.0.2.53")}
	host := ApplyContainerDNSDefaults("linux", &container.HostConfig{DNS: explicit})
	if !reflect.DeepEqual(host.DNS, explicit) {
		t.Fatalf("explicit DNS changed: %v", host.DNS)
	}
	if ApplyContainerDNSDefaults("windows", nil) != nil {
		t.Fatal("Windows container received Linux DNS defaults")
	}
}

func TestApplyProxyBuildArgsPreservesExplicitValuesAndInput(t *testing.T) {
	proxy := api.ProxySettings{
		HTTP: "http://proxy.example:8080", HTTPS: "https://proxy.example:8443",
		SOCKS5: "socks5://proxy.example:1080", NoProxy: "localhost,.example.com",
	}
	existingValue := "http://override.example"
	configuredDefault := "http://proxy.example:8080"
	existing := map[string]*string{"HTTP_PROXY": &configuredDefault, "http_proxy": &existingValue, "APP_SETTING": nil}
	got := ApplyProxyBuildArgs(existing, proxy)
	if got["http_proxy"] != &existingValue || got["HTTP_PROXY"] != &configuredDefault || got["APP_SETTING"] != nil {
		t.Fatal("explicit build args were not preserved")
	}
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		if got[key] == nil || *got[key] == "" {
			t.Errorf("missing default build arg %s", key)
		}
	}
	if _, exists := existing["HTTPS_PROXY"]; exists {
		t.Fatal("input build args were mutated")
	}
}

func networkWithMode(mode string) api.NetworkSettings {
	settings := api.DefaultNetworkSettings()
	settings.DefaultNetworkingMode = mode
	return settings
}

func boolPointer(value bool) *bool { return &value }

func pointerValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}
