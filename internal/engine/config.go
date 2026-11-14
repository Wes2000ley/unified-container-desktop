package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
)

type daemonSettings struct {
	Proxies             map[string]string            `json:"proxies,omitempty"`
	RegistryMirrors     []string                     `json:"registry-mirrors,omitempty"`
	BIP                 string                       `json:"bip,omitempty"`
	DNS                 []string                     `json:"dns,omitempty"`
	IPv6                *bool                        `json:"ipv6,omitempty"`
	FixedCIDRV6         string                       `json:"fixed-cidr-v6,omitempty"`
	DefaultAddressPools []daemonAddressPool          `json:"default-address-pools,omitempty"`
	DefaultNetworkOpts  map[string]map[string]string `json:"default-network-opts,omitempty"`
	ConfigureNetwork    bool                         `json:"-"`
}

type daemonAddressPool struct {
	Base string `json:"base"`
	Size int    `json:"size"`
}

const (
	linuxBridgeIPv6Subnet = "fd42:7763:b25b::/64"
	linuxCustomIPv6Pool   = "fd42:7763:b25c::/48"
	linuxIPv6SubnetSize   = 64
)

func validEndpoint(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Path == "" || u.Path == "/"
}

func validateEngineConfiguration(settings api.EngineConfiguration) error {
	if err := api.ValidateProxySettings("Docker", settings.DockerProxy); err != nil {
		return err
	}
	if err := api.ValidateProxySettings("container", settings.ContainersProxy); err != nil {
		return err
	}
	if err := api.ValidateNetworkSettings(settings.Network); err != nil {
		return err
	}
	for _, mirror := range settings.RegistryMirrors {
		if !validEndpoint(mirror) {
			return api.Fail("invalid_argument", "registry mirrors must be HTTP or HTTPS URLs without credentials, paths, queries, or fragments")
		}
	}
	return nil
}

func daemonSettingsForEngine(engineID string, settings api.EngineConfiguration) (daemonSettings, error) {
	if engineID != "windows" && engineID != "linux" {
		return daemonSettings{}, fmt.Errorf("unknown Docker engine %q", engineID)
	}
	if err := validateEngineConfiguration(settings); err != nil {
		return daemonSettings{}, err
	}
	proxy := map[string]string{}
	httpProxy, httpsProxy := settings.DockerProxy.HTTP, settings.DockerProxy.HTTPS
	if settings.DockerProxy.SOCKS5 != "" {
		bridge := api.DockerProxyAddress(engineID)
		if httpProxy == "" {
			httpProxy = bridge
		}
		if httpsProxy == "" {
			httpsProxy = bridge
		}
	}
	if httpProxy != "" {
		proxy["http-proxy"] = httpProxy
	}
	if httpsProxy != "" {
		proxy["https-proxy"] = httpsProxy
	}
	if settings.DockerProxy.NoProxy != "" {
		proxy["no-proxy"] = settings.DockerProxy.NoProxy
	}
	result := daemonSettings{Proxies: proxy, RegistryMirrors: append([]string(nil), settings.RegistryMirrors...)}
	if engineID != "linux" {
		return result, nil
	}
	result.ConfigureNetwork = true
	var ipv6 bool
	switch settings.Network.DefaultNetworkingMode {
	case api.NetworkModeDualStack, api.NetworkModeIPv6Only:
		ipv6 = true
	case api.NetworkModeIPv4Only:
	default:
		return daemonSettings{}, fmt.Errorf("unsupported default networking mode %q", settings.Network.DefaultNetworkingMode)
	}
	// The daemon's ipv6 setting only affects Docker's built-in bridge. It cannot
	// disable IPv4 there; per-network create paths apply the selected mode to
	// user-defined bridge networks.
	result.IPv6 = &ipv6
	if settings.Network.DefaultPortBinding == api.DefaultPortBindingLocal {
		result.DefaultNetworkOpts = map[string]map[string]string{
			"bridge": {"com.docker.network.bridge.host_binding_ipv4": "127.0.0.1"},
		}
	}
	if settings.Network.DockerSubnet != "" {
		prefix, _ := netip.ParsePrefix(settings.Network.DockerSubnet)
		gateway := prefix.Addr().Next()
		if !gateway.IsValid() || !prefix.Contains(gateway) {
			return daemonSettings{}, fmt.Errorf("Docker subnet has no usable gateway")
		}
		result.BIP = fmt.Sprintf("%s/%d", gateway, prefix.Bits())
	}
	if ipv6 {
		result.FixedCIDRV6 = linuxBridgeIPv6Subnet
	}
	result.DNS = []string{dnsfilter.IPv4Address, dnsfilter.IPv6Address}
	result.DefaultAddressPools = append(defaultIPv4AddressPools(), daemonAddressPool{Base: linuxCustomIPv6Pool, Size: linuxIPv6SubnetSize})
	return result, nil
}

func defaultIPv4AddressPools() []daemonAddressPool {
	return []daemonAddressPool{
		{Base: "172.17.0.0/16", Size: 16},
		{Base: "172.18.0.0/16", Size: 16},
		{Base: "172.19.0.0/16", Size: 16},
		{Base: "172.20.0.0/14", Size: 16},
		{Base: "172.24.0.0/14", Size: 16},
		{Base: "172.28.0.0/14", Size: 16},
		{Base: "192.168.0.0/16", Size: 20},
	}
}

func (b *Backend) configure(req api.Request) (any, error) {
	settings, err := api.Decode[api.EngineConfiguration](req)
	if err != nil {
		return nil, err
	}
	if err := validateEngineConfiguration(settings); err != nil {
		return nil, err
	}
	if b.id == "linux" && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("Linux daemon settings can only be changed by the Linux helper")
	}
	daemon, err := daemonSettingsForEngine(b.id, settings)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(b.dataRoot, "config", "docker-daemon.json")
	dnsPath := filepath.Join(b.dataRoot, "config", "dnsfilter.json")
	if b.id == "linux" {
		path = "/etc/docker/daemon.json"
	}
	runtimePath := runtimeConfigurationPath(b.dataRoot)
	runtimeData, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode runtime settings: %w", err)
	}
	runtimeData = append(runtimeData, '\n')

	b.configMu.Lock()
	defer b.configMu.Unlock()
	oldDaemon, hadDaemon, err := readConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("read previous Docker daemon settings: %w", err)
	}
	oldRuntime, hadRuntime, err := readConfigFile(runtimePath)
	if err != nil {
		return nil, fmt.Errorf("read previous runtime settings: %w", err)
	}
	var oldDNS []byte
	var hadDNS bool
	if b.id == "linux" {
		oldDNS, hadDNS, err = readConfigFile(dnsPath)
		if err != nil {
			return nil, fmt.Errorf("read previous DNS-filter settings: %w", err)
		}
	}
	rollback := func() error {
		var failures []error
		if b.id == "linux" {
			if e := restoreConfigFile(dnsPath, oldDNS, hadDNS); e != nil {
				failures = append(failures, fmt.Errorf("restore DNS-filter settings: %w", e))
			}
		}
		if e := restoreConfigFile(path, oldDaemon, hadDaemon); e != nil {
			failures = append(failures, fmt.Errorf("restore Docker daemon settings: %w", e))
		}
		if e := restoreConfigFile(runtimePath, oldRuntime, hadRuntime); e != nil {
			failures = append(failures, fmt.Errorf("restore runtime settings: %w", e))
		}
		return errors.Join(failures...)
	}
	if b.id == "linux" {
		filterConfig := dnsfilter.Config{
			Inhibition:            settings.Network.DNSInhibition,
			DefaultNetworkingMode: settings.Network.DefaultNetworkingMode,
		}
		if err := dnsfilter.WriteConfig(dnsPath, filterConfig); err != nil {
			return nil, fmt.Errorf("write DNS-filter settings: %w", err)
		}
	}
	if err := writeDaemonSettings(path, daemon); err != nil {
		return nil, errors.Join(fmt.Errorf("write Docker daemon settings: %w", err), rollback())
	}
	updatedDaemon, hasUpdatedDaemon, err := readConfigFile(path)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read updated Docker daemon settings: %w", err), rollback())
	}
	restartRequired := hadDaemon != hasUpdatedDaemon || !bytes.Equal(oldDaemon, updatedDaemon)
	if err := writeConfigFile(runtimePath, runtimeData); err != nil {
		return nil, errors.Join(fmt.Errorf("write runtime settings: %w", err), rollback())
	}
	b.runtimeConfig = settings
	b.runtimeConfig.RegistryMirrors = append([]string(nil), settings.RegistryMirrors...)
	return map[string]bool{"restartRequired": restartRequired}, nil
}

func writeDaemonSettings(path string, settings daemonSettings) error {
	dir := filepath.Dir(path)
	root, err := openConfigRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	if err := noSymlinks(root, name); err != nil {
		return err
	}
	config := make(map[string]json.RawMessage)
	data, err := root.ReadFile(name)
	if err == nil {
		if err = json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("parse existing daemon configuration: %w", err)
		}
		if config == nil {
			return fmt.Errorf("existing daemon configuration must be a JSON object")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := updateProxyConfig(config, settings.Proxies); err != nil {
		return err
	}
	if err := updateConfigField(config, "registry-mirrors", settings.RegistryMirrors); err != nil {
		return err
	}
	if settings.ConfigureNetwork {
		if err := updateConfigField(config, "bip", optionalString(settings.BIP)); err != nil {
			return err
		}
		if err := updateConfigField(config, "dns", settings.DNS); err != nil {
			return err
		}
		if err := updateConfigField(config, "ipv6", settings.IPv6); err != nil {
			return err
		}
		if err := updateConfigField(config, "fixed-cidr-v6", optionalString(settings.FixedCIDRV6)); err != nil {
			return err
		}
		if err := updateConfigField(config, "default-address-pools", settings.DefaultAddressPools); err != nil {
			return err
		}
		if err := updateDefaultNetworkOptions(config, settings.DefaultNetworkOpts); err != nil {
			return err
		}
	}
	data, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return writeConfigData(root, name, append(data, '\n'))
}

func optionalString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func updateConfigField(config map[string]json.RawMessage, key string, value any) error {
	if value == nil {
		delete(config, key)
		return nil
	}
	switch value := value.(type) {
	case []string:
		if len(value) == 0 {
			delete(config, key)
			return nil
		}
	case []daemonAddressPool:
		if len(value) == 0 {
			delete(config, key)
			return nil
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode daemon setting %s: %w", key, err)
	}
	config[key] = encoded
	return nil
}

func updateProxyConfig(config map[string]json.RawMessage, values map[string]string) error {
	const key = "proxies"
	managed := map[string]bool{"http-proxy": true, "https-proxy": true, "no-proxy": true}
	current := make(map[string]json.RawMessage)
	if raw, exists := config[key]; exists {
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("parse existing daemon proxies: %w", err)
		}
	}
	if current == nil {
		current = make(map[string]json.RawMessage)
	}
	for name := range managed {
		delete(current, name)
	}
	for name, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode daemon proxy %s: %w", name, err)
		}
		current[name] = encoded
	}
	if len(current) == 0 {
		delete(config, key)
		return nil
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode daemon proxies: %w", err)
	}
	config[key] = encoded
	return nil
}

func updateDefaultNetworkOptions(config map[string]json.RawMessage, values map[string]map[string]string) error {
	const key = "default-network-opts"
	current := make(map[string]map[string]string)
	if raw, exists := config[key]; exists {
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("parse existing default network options: %w", err)
		}
	}
	if current == nil {
		current = make(map[string]map[string]string)
	}
	managed := []string{
		"com.docker.network.enable_ipv4",
		"com.docker.network.enable_ipv6",
		"com.docker.network.bridge.host_binding_ipv4",
	}
	for driver, options := range values {
		driverOptions := current[driver]
		if driverOptions == nil {
			driverOptions = make(map[string]string)
		}
		if driver == "bridge" {
			for _, name := range managed {
				delete(driverOptions, name)
			}
		}
		for name, value := range options {
			driverOptions[name] = value
		}
		if len(driverOptions) == 0 {
			delete(current, driver)
		} else {
			current[driver] = driverOptions
		}
	}
	if len(values["bridge"]) == 0 {
		if driverOptions := current["bridge"]; driverOptions != nil {
			for _, name := range managed {
				delete(driverOptions, name)
			}
			if len(driverOptions) == 0 {
				delete(current, "bridge")
			}
		}
	}
	if len(current) == 0 {
		delete(config, key)
		return nil
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode default network options: %w", err)
	}
	config[key] = encoded
	return nil
}

func loadRuntimeConfiguration(root string) (api.EngineConfiguration, error) {
	settings := api.EngineConfiguration{Network: api.DefaultNetworkSettings()}
	data, err := os.ReadFile(runtimeConfigurationPath(root))
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil {
		return api.EngineConfiguration{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return api.EngineConfiguration{}, fmt.Errorf("decode runtime settings: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return api.EngineConfiguration{}, fmt.Errorf("runtime settings must contain one JSON object")
	}
	if err := validateEngineConfiguration(settings); err != nil {
		return api.EngineConfiguration{}, fmt.Errorf("invalid runtime settings: %w", err)
	}
	return settings, nil
}

func runtimeConfigurationPath(root string) string {
	return filepath.Join(root, "config", "runtime-settings.json")
}

func readConfigFile(path string) ([]byte, bool, error) {
	root, err := openConfigRoot(filepath.Dir(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer root.Close()
	name := filepath.Base(path)
	if err := noSymlinks(root, name); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, err := root.ReadFile(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func writeConfigFile(path string, data []byte) error {
	root, err := openConfigRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	return writeConfigData(root, filepath.Base(path), data)
}

func restoreConfigFile(path string, data []byte, existed bool) error {
	if existed {
		return writeConfigFile(path, data)
	}
	root, err := openConfigRoot(filepath.Dir(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	if err := noSymlinks(root, name); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := root.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func openConfigRoot(dir string) (*os.Root, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("configuration directory must be a real directory")
	}
	return os.OpenRoot(dir)
}

func writeConfigData(root *os.Root, name string, data []byte) error {
	if err := noSymlinks(root, name); err != nil {
		return err
	}
	temp, err := randomName()
	if err != nil {
		return err
	}
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := noSymlinks(root, name); err != nil {
		return err
	}
	return root.Rename(temp, name)
}
