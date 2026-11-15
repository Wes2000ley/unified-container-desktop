package engine

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
)

const dnsInhibitionLabel = "com.ucd.dns-inhibition"

// ApplyNetworkDefaults applies the selected address-family defaults only when
// a user creates a bridge network without an explicit family choice.
func ApplyNetworkDefaults(request *network.CreateRequest, settings api.NetworkSettings) error {
	if request == nil {
		return fmt.Errorf("network create request is required")
	}
	if err := api.ValidateNetworkSettings(settings); err != nil {
		return err
	}
	if request.ConfigOnly || request.ConfigFrom != nil {
		if _, exists := request.Options[dnsInhibitionLabel]; exists {
			return fmt.Errorf("DNS inhibition cannot be set on a config-only network")
		}
		if _, exists := request.Labels[dnsInhibitionLabel]; exists {
			return fmt.Errorf("network label %q is reserved by UCD", dnsInhibitionLabel)
		}
		return nil
	}
	if _, exists := request.Labels[dnsInhibitionLabel]; exists {
		return fmt.Errorf("network label %q is reserved by UCD", dnsInhibitionLabel)
	}
	if override, exists := request.Options[dnsInhibitionLabel]; exists {
		if !validDNSInhibition(override) {
			return fmt.Errorf("network DNS inhibition must be auto, none, ipv4, or ipv6")
		}
		if request.Driver != "" && request.Driver != "bridge" {
			return fmt.Errorf("per-network DNS inhibition is supported only on bridge networks")
		}
		delete(request.Options, dnsInhibitionLabel)
		if request.Labels == nil {
			request.Labels = make(map[string]string)
		}
		request.Labels[dnsInhibitionLabel] = override
	}
	if request.Driver != "" && request.Driver != "bridge" {
		return nil
	}
	ipv4, ipv6 := true, false
	switch settings.DefaultNetworkingMode {
	case api.NetworkModeDualStack:
		ipv6 = true
	case api.NetworkModeIPv6Only:
		ipv4, ipv6 = false, true
	case api.NetworkModeIPv4Only:
	default:
		return fmt.Errorf("unsupported default networking mode %q", settings.DefaultNetworkingMode)
	}
	if request.EnableIPv4 == nil {
		request.EnableIPv4 = &ipv4
	}
	if request.EnableIPv6 == nil {
		request.EnableIPv6 = &ipv6
	}
	return nil
}

func validDNSInhibition(value string) bool {
	return value == api.DNSInhibitionAuto || value == api.DNSInhibitionNone || value == api.DNSInhibitionIPv4 || value == api.DNSInhibitionIPv6
}

// ValidateContainerNetworkMode enforces the Linux-only host-network opt-in.
func ValidateContainerNetworkMode(engineID, mode string, settings api.NetworkSettings) error {
	if !strings.EqualFold(mode, "host") {
		return nil
	}
	if engineID != "linux" {
		return fmt.Errorf("host networking is available only for Linux containers")
	}
	if !settings.HostNetworkingEnabled {
		return fmt.Errorf("Linux host networking is disabled; enable it in Settings first")
	}
	return nil
}

// ApplyContainerProxyEnvironment adds proxy defaults while preserving explicit
// container variables, including either standard spelling of each variable.
func ApplyContainerProxyEnvironment(existing []string, proxy api.ProxySettings) []string {
	result := append([]string(nil), existing...)
	set := func(primary, secondary, value string) {
		if value == "" || hasEnvironmentValue(result, primary, secondary) {
			return
		}
		result = append(result, primary+"="+value, secondary+"="+value)
	}
	set("HTTP_PROXY", "http_proxy", proxy.HTTP)
	set("HTTPS_PROXY", "https_proxy", proxy.HTTPS)
	set("ALL_PROXY", "all_proxy", proxy.SOCKS5)
	set("NO_PROXY", "no_proxy", proxy.NoProxy)
	return result
}

// ApplyContainerDNSDefaults configures the filtering resolver for Linux
// containers that use an isolated network and have no explicit DNS servers.
func ApplyContainerDNSDefaults(engineID string, hostConfig *container.HostConfig) *container.HostConfig {
	if engineID != "linux" {
		return hostConfig
	}
	if hostConfig == nil {
		hostConfig = &container.HostConfig{}
	}
	if len(hostConfig.DNS) != 0 {
		return hostConfig
	}
	mode := strings.ToLower(string(hostConfig.NetworkMode))
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") {
		return hostConfig
	}
	hostConfig.DNS = []netip.Addr{
		netip.MustParseAddr(dnsfilter.IPv4Address),
		netip.MustParseAddr(dnsfilter.IPv6Address),
	}
	return hostConfig
}

// ApplyProxyBuildArgs adds proxy build arguments while preserving explicit
// Dockerfile build arguments, regardless of their name's letter casing.
func ApplyProxyBuildArgs(existing map[string]*string, proxy api.ProxySettings) map[string]*string {
	result := make(map[string]*string, len(existing)+8)
	for key, value := range existing {
		result[key] = value
	}
	set := func(primary, secondary, value string) {
		if value == "" || hasBuildArg(result, primary, secondary) {
			return
		}
		primaryValue, secondaryValue := value, value
		result[primary] = &primaryValue
		result[secondary] = &secondaryValue
	}
	set("HTTP_PROXY", "http_proxy", proxy.HTTP)
	set("HTTPS_PROXY", "https_proxy", proxy.HTTPS)
	set("ALL_PROXY", "all_proxy", proxy.SOCKS5)
	set("NO_PROXY", "no_proxy", proxy.NoProxy)
	return result
}

func hasBuildArg(values map[string]*string, names ...string) bool {
	for key := range values {
		for _, name := range names {
			if strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

func hasEnvironmentValue(env []string, names ...string) bool {
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		for _, name := range names {
			if strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

func composeBuildProxyConfig(proxy api.ProxySettings) map[string]string {
	result := make(map[string]string)
	set := func(key, value string) {
		if value != "" {
			result[key] = value
		}
	}
	set("HTTP_PROXY", proxy.HTTP)
	set("http_proxy", proxy.HTTP)
	set("HTTPS_PROXY", proxy.HTTPS)
	set("https_proxy", proxy.HTTPS)
	set("ALL_PROXY", proxy.SOCKS5)
	set("all_proxy", proxy.SOCKS5)
	set("NO_PROXY", proxy.NoProxy)
	set("no_proxy", proxy.NoProxy)
	return result
}

type composeDefaultsClient struct {
	client.APIClient
	backend *Backend
}

func (c *composeDefaultsClient) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if options.Config == nil {
		return c.APIClient.ContainerCreate(ctx, options)
	}
	settings := c.backend.RuntimeConfiguration()
	options.Config.Env = ApplyContainerProxyEnvironment(options.Config.Env, settings.ContainersProxy)
	options.HostConfig = ApplyContainerDNSDefaults(c.backend.id, options.HostConfig)
	if options.HostConfig != nil {
		if err := ValidateContainerNetworkMode(c.backend.id, string(options.HostConfig.NetworkMode), settings.Network); err != nil {
			return client.ContainerCreateResult{}, err
		}
	}
	if c.backend.id == "windows" {
		if _, managedWindowsPorts := options.Config.Labels[windowsPortsLabel]; managedWindowsPorts {
			// windowsRuntimeProject has already validated and transformed this metadata.
			result, err := c.APIClient.ContainerCreate(ctx, options)
			return c.backend.finishContainerCreate(ctx, result, err, options.HostConfig)
		}
	}
	params := api.ContainerCreateParams{
		Name: options.Name, Config: options.Config,
		HostConfig: options.HostConfig, NetworkingConfig: options.NetworkingConfig,
	}
	if err := c.backend.prepareContainerPorts(ctx, &params); err != nil {
		return client.ContainerCreateResult{}, err
	}
	options.Config, options.HostConfig = params.Config, params.HostConfig
	result, err := c.APIClient.ContainerCreate(ctx, options)
	return c.backend.finishContainerCreate(ctx, result, err, options.HostConfig)
}

func (c *composeDefaultsClient) NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	if c.backend.id == "linux" {
		var configFrom *network.ConfigReference
		if options.ConfigFrom != "" {
			configFrom = &network.ConfigReference{Network: options.ConfigFrom}
		}
		request := network.CreateRequest{
			Name: name, Driver: options.Driver, Scope: options.Scope,
			EnableIPv4: options.EnableIPv4, EnableIPv6: options.EnableIPv6, IPAM: options.IPAM,
			Internal: options.Internal, Attachable: options.Attachable, Ingress: options.Ingress,
			ConfigOnly: options.ConfigOnly, ConfigFrom: configFrom, Options: options.Options,
			Labels: options.Labels,
		}
		if err := ApplyNetworkDefaults(&request, c.backend.RuntimeConfiguration().Network); err != nil {
			return client.NetworkCreateResult{}, err
		}
		options.EnableIPv4, options.EnableIPv6 = request.EnableIPv4, request.EnableIPv6
		options.Options, options.Labels = request.Options, request.Labels
	}
	return c.APIClient.NetworkCreate(ctx, name, options)
}

func (c *composeDefaultsClient) ImageBuild(ctx context.Context, context io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error) {
	options.BuildArgs = ApplyProxyBuildArgs(options.BuildArgs, c.backend.RuntimeConfiguration().ContainersProxy)
	return c.APIClient.ImageBuild(ctx, context, options)
}
