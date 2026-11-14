package api

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

const (
	NetworkModeDualStack = "dual-stack"
	NetworkModeIPv4Only  = "ipv4only"
	NetworkModeIPv6Only  = "ipv6only"

	DNSInhibitionAuto = "auto"
	DNSInhibitionNone = "none"
	DNSInhibitionIPv4 = "ipv4"
	DNSInhibitionIPv6 = "ipv6"

	DefaultPortBindingAll   = "all"
	DefaultPortBindingLocal = "local"
)

// ProxySettings configures one machine-wide HTTP/HTTPS or SOCKS5 proxy.
// Proxy URLs must not contain credentials; noProxy uses the standard comma list.
type ProxySettings struct {
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	SOCKS5  string `json:"socks5,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
}

// NetworkSettings controls defaults for new Linux Docker networks and containers.
type NetworkSettings struct {
	DockerSubnet          string `json:"dockerSubnet,omitempty"`
	DefaultNetworkingMode string `json:"defaultNetworkingMode"`
	DNSInhibition         string `json:"dnsInhibition"`
	DefaultPortBinding    string `json:"defaultPortBinding"`
	HostNetworkingEnabled bool   `json:"hostNetworkingEnabled"`
}

// ResourceSaverSettings controls when the service-owned Linux Docker engine sleeps.
type ResourceSaverSettings struct {
	Enabled     bool `json:"enabled"`
	IdleSeconds int  `json:"idleSeconds"`
}

// EngineConfiguration carries machine defaults to one Docker engine.
type EngineConfiguration struct {
	DockerProxy     ProxySettings   `json:"dockerProxy"`
	ContainersProxy ProxySettings   `json:"containersProxy"`
	Network         NetworkSettings `json:"network"`
	RegistryMirrors []string        `json:"registryMirrors"`
}

// DefaultNetworkSettings keeps IPv4 networking enabled and limits new published ports to loopback.
func DefaultNetworkSettings() NetworkSettings {
	return NetworkSettings{
		DefaultNetworkingMode: NetworkModeIPv4Only,
		DNSInhibition:         DNSInhibitionAuto,
		DefaultPortBinding:    DefaultPortBindingLocal,
	}
}

// DefaultResourceSaverSettings keeps the engine awake until a user enables Resource Saver.
func DefaultResourceSaverSettings() ResourceSaverSettings {
	return ResourceSaverSettings{Enabled: false, IdleSeconds: 300}
}

// DockerProxyAddress returns the local HTTP bridge address used for SOCKS5 daemon traffic.
func DockerProxyAddress(engineID string) string {
	switch engineID {
	case "windows":
		return "http://127.0.0.1:7444"
	case "linux":
		return "http://127.0.0.1:7445"
	default:
		return ""
	}
}

// ValidateProxySettings checks URLs before they reach a daemon, child process, or environment.
func ValidateProxySettings(name string, proxy ProxySettings) error {
	for _, item := range []struct{ label, value string }{{"HTTP proxy", proxy.HTTP}, {"HTTPS proxy", proxy.HTTPS}} {
		if item.value != "" {
			if err := validateProxyURL(item.value, "http", "https"); err != nil {
				return fmt.Errorf("%s %s: %w", name, item.label, err)
			}
		}
	}
	if proxy.SOCKS5 != "" {
		if err := validateProxyURL(proxy.SOCKS5, "socks5"); err != nil {
			return fmt.Errorf("%s SOCKS5 proxy: %w", name, err)
		}
	}
	if len(proxy.NoProxy) > 4096 || strings.ContainsAny(proxy.NoProxy, "\r\n\x00") {
		return fmt.Errorf("%s no-proxy list is invalid or exceeds 4096 characters", name)
	}
	return nil
}

func validateProxyURL(value string, schemes ...string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("URL must not contain leading or trailing whitespace")
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("URL must include a host and must not contain credentials, a path, a query, or a fragment")
	}
	validScheme := false
	for _, scheme := range schemes {
		if strings.EqualFold(u.Scheme, scheme) {
			validScheme = true
			break
		}
	}
	if !validScheme {
		return fmt.Errorf("URL scheme must be %s", strings.Join(schemes, " or "))
	}
	return nil
}

// ValidateNetworkSettings ensures the requested Linux network defaults are supported.
func ValidateNetworkSettings(settings NetworkSettings) error {
	if settings.DockerSubnet != "" {
		prefix, err := netip.ParsePrefix(settings.DockerSubnet)
		if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.Bits() < 12 || prefix.Bits() > 24 {
			return fmt.Errorf("Docker subnet must be a network-aligned IPv4 CIDR between /12 and /24")
		}
	}
	if settings.DefaultNetworkingMode != NetworkModeDualStack && settings.DefaultNetworkingMode != NetworkModeIPv4Only && settings.DefaultNetworkingMode != NetworkModeIPv6Only {
		return fmt.Errorf("default networking mode must be dual-stack, ipv4only, or ipv6only")
	}
	if settings.DNSInhibition != DNSInhibitionAuto && settings.DNSInhibition != DNSInhibitionNone && settings.DNSInhibition != DNSInhibitionIPv4 && settings.DNSInhibition != DNSInhibitionIPv6 {
		return fmt.Errorf("DNS inhibition must be auto, none, ipv4, or ipv6")
	}
	if settings.DefaultPortBinding != DefaultPortBindingAll && settings.DefaultPortBinding != DefaultPortBindingLocal {
		return fmt.Errorf("default port binding must be all or local")
	}
	return nil
}

// ValidateResourceSaverSettings enforces Docker Desktop's minimum idle threshold.
func ValidateResourceSaverSettings(settings ResourceSaverSettings) error {
	if settings.IdleSeconds < 31 || settings.IdleSeconds > 3600 {
		return fmt.Errorf("Resource Saver threshold must be between 31 and 3600 seconds")
	}
	return nil
}
