package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
)

func TestDaemonSettingsForLinuxNetworkingModes(t *testing.T) {
	dnsIPv6 := netip.MustParseAddr(dnsfilter.IPv6Address)
	bridgePrefix := netip.MustParsePrefix(linuxBridgeIPv6Subnet)
	customPrefix := netip.MustParsePrefix(linuxCustomIPv6Pool)
	if bridgePrefix.Contains(dnsIPv6) || customPrefix.Contains(dnsIPv6) {
		t.Fatalf("IPv6 DNS listener %s overlaps a Docker address pool", dnsIPv6)
	}
	for _, tc := range []struct {
		mode      string
		wantIPv6  bool
		wantFixed string
	}{
		{api.NetworkModeIPv4Only, false, ""},
		{api.NetworkModeDualStack, true, linuxBridgeIPv6Subnet},
		{api.NetworkModeIPv6Only, true, linuxBridgeIPv6Subnet},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			settings := api.EngineConfiguration{Network: api.DefaultNetworkSettings()}
			settings.Network.DefaultNetworkingMode = tc.mode
			settings.Network.DockerSubnet = "10.44.0.0/16"
			settings.Network.DefaultPortBinding = api.DefaultPortBindingAll
			settings.DockerProxy = api.ProxySettings{
				HTTP: "http://http-proxy.example:8080", SOCKS5: "socks5://socks-proxy.example:1080",
				NoProxy: "localhost,.example.com",
			}
			got, err := daemonSettingsForEngine("linux", settings)
			if err != nil {
				t.Fatal(err)
			}
			bridgeOptions := got.DefaultNetworkOpts["bridge"]
			for _, key := range []string{"com.docker.network.enable_ipv4", "com.docker.network.enable_ipv6"} {
				if _, exists := bridgeOptions[key]; exists {
					t.Fatalf("bridge family default %q should be applied by network creation: %#v", key, bridgeOptions)
				}
			}
			if _, exists := bridgeOptions["com.docker.network.bridge.host_binding_ipv4"]; exists {
				t.Fatal("all-address port binding was restricted to loopback")
			}
			if got.FixedCIDRV6 != tc.wantFixed || got.IPv6 == nil || *got.IPv6 != tc.wantIPv6 {
				t.Fatalf("IPv6 daemon settings = %#v", got)
			}
			if got.BIP != "10.44.0.1/16" {
				t.Fatalf("bridge IPv4 subnet = %q", got.BIP)
			}
			if len(got.DNS) != 2 || got.DNS[0] != dnsfilter.IPv4Address || got.DNS[1] != dnsfilter.IPv6Address {
				t.Fatalf("DNS helper addresses = %#v", got.DNS)
			}
			if got.Proxies["http-proxy"] != settings.DockerProxy.HTTP || got.Proxies["https-proxy"] != api.DockerProxyAddress("linux") || got.Proxies["no-proxy"] != settings.DockerProxy.NoProxy {
				t.Fatalf("daemon proxy precedence = %#v", got.Proxies)
			}
			if len(got.DefaultAddressPools) != 8 || got.DefaultAddressPools[7] != (daemonAddressPool{Base: linuxCustomIPv6Pool, Size: linuxIPv6SubnetSize}) {
				t.Fatalf("default address pools = %#v", got.DefaultAddressPools)
			}
		})
	}
}

func TestDaemonSettingsMergePreservesUnmanagedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	old := `{"log-level":"warn","proxies":{"ftp-proxy":"ftp://proxy.example:2121","http-proxy":"http://old.example:8080","no-proxy":"old.example"},"default-network-opts":{"bridge":{"mtu":"1450","com.docker.network.enable_ipv4":"true","com.docker.network.enable_ipv6":"false","com.docker.network.bridge.host_binding_ipv4":"0.0.0.0"},"overlay":{"custom":"kept"}}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	settings := api.EngineConfiguration{Network: api.DefaultNetworkSettings()}
	settings.DockerProxy.HTTP = "http://new.example:8080"
	daemon, err := daemonSettingsForEngine("linux", settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonSettings(path, daemon); err != nil {
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
	if got["log-level"] != "warn" || proxies["ftp-proxy"] != "ftp://proxy.example:2121" || proxies["http-proxy"] != "http://new.example:8080" {
		t.Fatalf("unmanaged daemon values were not preserved: %#v", got)
	}
	if _, exists := proxies["no-proxy"]; exists {
		t.Fatal("stale UCD no-proxy setting was retained")
	}
	defaults := got["default-network-opts"].(map[string]any)
	bridge := defaults["bridge"].(map[string]any)
	overlay := defaults["overlay"].(map[string]any)
	if bridge["mtu"] != "1450" || bridge["com.docker.network.bridge.host_binding_ipv4"] != "127.0.0.1" || overlay["custom"] != "kept" {
		t.Fatalf("default network options = %#v", defaults)
	}
	for _, key := range []string{"com.docker.network.enable_ipv4", "com.docker.network.enable_ipv6"} {
		if _, exists := bridge[key]; exists {
			t.Fatalf("stale managed bridge family option %q was retained: %#v", key, bridge)
		}
	}
}

func TestConfigureRestartRequiredTracksDaemonChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			w.Header().Set("OSType", "windows")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	root := t.TempDir()
	b, err := New("windows", "tcp://"+strings.TrimPrefix(server.URL, "http://"), root, &api.EventBus{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	settings := api.EngineConfiguration{Network: api.DefaultNetworkSettings()}
	configure := func(value api.EngineConfiguration) bool {
		t.Helper()
		req, err := api.NewRequest("engine.configure", "windows", value)
		if err != nil {
			t.Fatal(err)
		}
		result, err := b.configure(req)
		if err != nil {
			t.Fatal(err)
		}
		return result.(map[string]bool)["restartRequired"]
	}
	if !configure(settings) {
		t.Fatal("first daemon configuration did not require restart")
	}
	if configure(settings) {
		t.Fatal("unchanged daemon configuration required restart")
	}
	settings.ContainersProxy.HTTP = "http://container-proxy.example:8080"
	if configure(settings) {
		t.Fatal("container environment proxy change required daemon restart")
	}
	settings.DockerProxy.HTTP = "http://daemon-proxy.example:8080"
	if !configure(settings) {
		t.Fatal("daemon proxy change did not require restart")
	}
	if _, err := os.Stat(runtimeConfigurationPath(root)); err != nil {
		t.Fatalf("runtime settings were not persisted: %v", err)
	}
}

func TestRuntimeConfigurationLoadsDefaultsAndRejectsTrailingJSON(t *testing.T) {
	root := t.TempDir()
	got, err := loadRuntimeConfiguration(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != api.DefaultNetworkSettings() {
		t.Fatalf("runtime network defaults = %#v", got.Network)
	}
	path := runtimeConfigurationPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"network":{"defaultNetworkingMode":"ipv4only","dnsInhibition":"auto","defaultPortBinding":"local","hostNetworkingEnabled":false}} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimeConfiguration(root); err == nil {
		t.Fatal("accepted trailing runtime JSON")
	}
}
