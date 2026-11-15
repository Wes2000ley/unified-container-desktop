package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

const linuxPortsLabel = "com.skyri.ucd.linux-published-ports"
const linuxPortReadyTimeout = 10 * time.Second

var linuxRelayIP = netip.MustParseAddr("127.0.0.1")
var linuxRelayIPv6 = netip.MustParseAddr("::1")

type linuxRelayKey struct {
	container string
	target    string
}

type linuxFamilyRelay struct {
	dockerPort uint16
	forward    *portForward
}

type linuxDockerRelayPorts struct {
	ipv4 map[string]uint16
	ipv6 map[string]uint16
}

func newLinuxDockerRelayPorts(capacity int) linuxDockerRelayPorts {
	return linuxDockerRelayPorts{ipv4: make(map[string]uint16, capacity), ipv6: make(map[string]uint16, capacity)}
}

func linuxPortRelayEnabled(host *container.HostConfig) bool {
	if host == nil {
		return false
	}
	mode := strings.ToLower(string(host.NetworkMode))
	return mode != "host" && mode != "none" && !strings.HasPrefix(mode, "container:")
}

func linuxPortStateIsActive(state string) bool {
	return strings.EqualFold(state, "running") || strings.EqualFold(state, "paused")
}

func prepareLinuxContainerPorts(p *api.ContainerCreateParams, requested []api.Port) error {
	if len(requested) == 0 {
		return nil
	}
	metadata, err := json.Marshal(requested)
	if err != nil {
		return fmt.Errorf("encode Linux published port metadata: %w", err)
	}
	if p.Config.Labels == nil {
		p.Config.Labels = make(map[string]string)
	}
	p.Config.Labels[linuxPortsLabel] = string(metadata)
	if p.Config.ExposedPorts == nil {
		p.Config.ExposedPorts = make(network.PortSet)
	}
	relayBindings := make(network.PortMap)
	for _, requestedPort := range requested {
		port, err := network.ParsePort(strconv.Itoa(int(requestedPort.Target)) + "/" + requestedPort.Protocol)
		if err != nil {
			return fmt.Errorf("parse Linux published port target: %w", err)
		}
		p.Config.ExposedPorts[port] = struct{}{}
		if _, exists := relayBindings[port]; !exists {
			// Docker uses IPv4 bindings on IPv4 networks and IPv6 bindings on
			// IPv6-only networks. Keep both private and let Docker omit the
			// unsupported address family for the selected network.
			relayBindings[port] = []network.PortBinding{
				{HostIP: linuxRelayIP, HostPort: "0"},
				{HostIP: linuxRelayIPv6, HostPort: "0"},
			}
		}
	}
	p.HostConfig.PortBindings = relayBindings
	return nil
}

func clearInheritedLinuxPortLabel(p *api.ContainerCreateParams) {
	if p.Config.Labels == nil {
		p.Config.Labels = make(map[string]string)
	}
	// Docker inherits image labels unless the create request overrides them.
	// Empty marks a committed managed container as an ordinary new container.
	p.Config.Labels[linuxPortsLabel] = ""
}

func parseLinuxPortMetadata(raw string) ([]api.Port, error) {
	if len(raw) > maxWindowsPortMetadata {
		return nil, fmt.Errorf("container %s label exceeds 1 MiB", linuxPortsLabel)
	}
	var ports []api.Port
	if err := json.Unmarshal([]byte(raw), &ports); err != nil || len(ports) == 0 || len(ports) > 1024 {
		return nil, fmt.Errorf("container has invalid %s label", linuxPortsLabel)
	}
	for index, port := range ports {
		if err := validateForwardPort(port); err != nil {
			return nil, fmt.Errorf("container has invalid %s entry %d: %w", linuxPortsLabel, index, err)
		}
		address, _ := netip.ParseAddr(port.HostIP)
		if address.Unmap().String() != port.HostIP {
			return nil, fmt.Errorf("container has noncanonical host address in %s entry %d", linuxPortsLabel, index)
		}
		if index > 0 && linuxPortLess(port, ports[index-1]) {
			return nil, fmt.Errorf("container has unsorted %s metadata", linuxPortsLabel)
		}
	}
	return ports, nil
}

func linuxPortLess(a, b api.Port) bool {
	if a.Target != b.Target {
		return a.Target < b.Target
	}
	if a.Protocol != b.Protocol {
		return a.Protocol < b.Protocol
	}
	if a.HostIP != b.HostIP {
		return a.HostIP < b.HostIP
	}
	return a.Published < b.Published
}

func linuxPortKey(target uint16, protocol string) string {
	return strconv.Itoa(int(target)) + "/" + strings.ToLower(protocol)
}

func linuxRequestedPort(requested []api.Port, target uint16, protocol string) bool {
	key := linuxPortKey(target, protocol)
	for _, port := range requested {
		if linuxPortKey(port.Target, port.Protocol) == key {
			return true
		}
	}
	return false
}

func relayPortsFromSummary(summary container.Summary, requested []api.Port) (linuxDockerRelayPorts, error) {
	wanted := make(map[string]struct{})
	for _, port := range requested {
		wanted[linuxPortKey(port.Target, port.Protocol)] = struct{}{}
	}
	relayPorts := newLinuxDockerRelayPorts(len(wanted))
	for _, published := range summary.Ports {
		key := linuxPortKey(published.PrivatePort, published.Type)
		if _, ok := wanted[key]; !ok || !published.IP.IsValid() || published.PublicPort == 0 {
			continue
		}
		if err := relayPorts.add(key, published.IP, published.PublicPort); err != nil {
			return linuxDockerRelayPorts{}, fmt.Errorf("container %s has multiple Linux relay ports for %s", summary.ID, key)
		}
	}
	for key := range wanted {
		if relayPorts.ipv4[key] == 0 && relayPorts.ipv6[key] == 0 {
			return linuxDockerRelayPorts{}, fmt.Errorf("container %s is missing its Linux loopback relay for %s", summary.ID, key)
		}
	}
	return relayPorts, nil
}

func (ports linuxDockerRelayPorts) add(key string, ip netip.Addr, port uint16) error {
	address := ip.Unmap()
	var family map[string]uint16
	switch address {
	case linuxRelayIP:
		family = ports.ipv4
	case linuxRelayIPv6:
		family = ports.ipv6
	default:
		return nil
	}
	if previous := family[key]; previous != 0 && previous != port {
		return fmt.Errorf("multiple ports in one address family")
	}
	family[key] = port
	return nil
}

func (b *Backend) hostRelayPorts(containerID string, requested []api.Port, docker linuxDockerRelayPorts) (map[string]uint16, error) {
	ports := make(map[string]uint16)
	for _, port := range requested {
		key := linuxPortKey(port.Target, port.Protocol)
		if _, done := ports[key]; done {
			continue
		}
		if ipv4 := docker.ipv4[key]; ipv4 != 0 {
			ports[key] = ipv4
			b.closeLinuxFamilyRelay(linuxRelayKey{container: containerID, target: key})
			continue
		}
		ipv6 := docker.ipv6[key]
		if ipv6 == 0 {
			return nil, fmt.Errorf("container %s is missing a loopback relay for %s", containerID, key)
		}
		relayKey := linuxRelayKey{container: containerID, target: key}
		relayPort, err := b.ensureLinuxFamilyRelay(relayKey, port, ipv6)
		if err != nil {
			return nil, err
		}
		ports[key] = relayPort
	}
	return ports, nil
}

func (b *Backend) ensureLinuxFamilyRelay(key linuxRelayKey, target api.Port, dockerPort uint16) (uint16, error) {
	b.linuxPortsMu.Lock()
	if current, ok := b.linuxRelayPorts[key]; ok && current.dockerPort == dockerPort {
		port := current.forward.port.Published
		b.linuxPortsMu.Unlock()
		return port, nil
	}
	b.linuxPortsMu.Unlock()

	forward, err := startPortForward(api.Port{
		HostIP:   linuxRelayIP.String(),
		Target:   target.Target,
		Protocol: target.Protocol,
	}, net.JoinHostPort(linuxRelayIPv6.String(), strconv.Itoa(int(dockerPort))))
	if err != nil {
		return 0, fmt.Errorf("create IPv4 shim for IPv6-only Linux port %s: %w", linuxPortKey(target.Target, target.Protocol), err)
	}

	b.linuxPortsMu.Lock()
	if b.linuxRelayPorts == nil {
		b.linuxRelayPorts = make(map[linuxRelayKey]linuxFamilyRelay)
	}
	if current, ok := b.linuxRelayPorts[key]; ok && current.dockerPort == dockerPort {
		port := current.forward.port.Published
		b.linuxPortsMu.Unlock()
		forward.close()
		return port, nil
	}
	previous := b.linuxRelayPorts[key]
	b.linuxRelayPorts[key] = linuxFamilyRelay{dockerPort: dockerPort, forward: forward}
	b.linuxPortsMu.Unlock()
	if previous.forward != nil {
		previous.forward.close()
	}
	return forward.port.Published, nil
}

func (b *Backend) closeLinuxFamilyRelay(key linuxRelayKey) {
	b.linuxPortsMu.Lock()
	previous := b.linuxRelayPorts[key]
	delete(b.linuxRelayPorts, key)
	b.linuxPortsMu.Unlock()
	if previous.forward != nil {
		previous.forward.close()
	}
}

func (b *Backend) pruneLinuxFamilyRelays(desired map[linuxRelayKey]uint16) {
	b.linuxPortsMu.Lock()
	var stale []*portForward
	for key, current := range b.linuxRelayPorts {
		if port, ok := desired[key]; ok && port == current.dockerPort {
			continue
		}
		delete(b.linuxRelayPorts, key)
		stale = append(stale, current.forward)
	}
	b.linuxPortsMu.Unlock()
	for _, forward := range stale {
		forward.close()
	}
}

func (b *Backend) closeLinuxPortRelays() {
	b.linuxPortsMu.Lock()
	var forwards []*portForward
	for _, relay := range b.linuxRelayPorts {
		forwards = append(forwards, relay.forward)
	}
	b.linuxRelayPorts = nil
	b.linuxPortState = nil
	b.signalLinuxPortStateLocked()
	b.linuxPortsMu.Unlock()
	for _, forward := range forwards {
		forward.close()
	}
}

func (b *Backend) currentLinuxPortBindings(ctx context.Context) ([]api.LinuxPortBinding, error) {
	if b.id != "linux" {
		return nil, api.Fail("invalid_argument", "Linux published ports are available only on the Linux engine")
	}
	result, err := b.docker.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		b.closeLinuxPortRelays()
		return nil, fmt.Errorf("list running Linux containers for published ports: %w", err)
	}
	bindings := make([]api.LinuxPortBinding, 0)
	desiredRelays := make(map[linuxRelayKey]uint16)
	failed := true
	defer func() {
		if failed {
			b.closeLinuxPortRelays()
		}
	}()
	for _, summary := range result.Items {
		if !linuxPortStateIsActive(string(summary.State)) {
			continue
		}
		raw, managed := summary.Labels[linuxPortsLabel]
		if !managed || raw == "" {
			continue
		}
		requested, err := parseLinuxPortMetadata(raw)
		if err != nil {
			return nil, fmt.Errorf("container %s: %w", summary.ID, err)
		}
		dockerRelays, err := relayPortsFromSummary(summary, requested)
		if err != nil {
			return nil, err
		}
		relays, err := b.hostRelayPorts(summary.ID, requested, dockerRelays)
		if err != nil {
			return nil, err
		}
		for target, port := range dockerRelays.ipv6 {
			if dockerRelays.ipv4[target] == 0 {
				desiredRelays[linuxRelayKey{container: summary.ID, target: target}] = port
			}
		}
		for index, port := range requested {
			bindings = append(bindings, api.LinuxPortBinding{
				Container: summary.ID,
				Index:     index,
				Port:      port,
				RelayPort: relays[linuxPortKey(port.Target, port.Protocol)],
			})
		}
	}
	sortLinuxPortBindings(bindings)
	b.pruneLinuxFamilyRelays(desiredRelays)
	b.pruneLinuxPortState(bindings)
	failed = false
	return bindings, nil
}

func (b *Backend) linuxPortBindings(ctx context.Context) (api.LinuxPortBindings, error) {
	bindings, err := b.currentLinuxPortBindings(ctx)
	if err != nil {
		return api.LinuxPortBindings{}, err
	}
	return api.LinuxPortBindings{Bindings: bindings}, nil
}

func sortLinuxPortBindings(bindings []api.LinuxPortBinding) {
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Container == bindings[j].Container {
			return bindings[i].Index < bindings[j].Index
		}
		return bindings[i].Container < bindings[j].Container
	})
}

func activeLinuxPortValid(active, current api.LinuxPortBinding) bool {
	return active.Container == current.Container && active.Index == current.Index &&
		active.RelayPort == current.RelayPort && active.Port.HostIP == current.Port.HostIP &&
		active.Port.Target == current.Port.Target && active.Port.Protocol == current.Port.Protocol &&
		active.Port.Published != 0 && (current.Port.Published == 0 || active.Port.Published == current.Port.Published)
}

func (b *Backend) pruneLinuxPortState(current []api.LinuxPortBinding) {
	currentByKey := make(map[forwardKey]api.LinuxPortBinding, len(current))
	for _, binding := range current {
		currentByKey[forwardKey{container: binding.Container, index: binding.Index}] = binding
	}
	b.linuxPortsMu.Lock()
	defer b.linuxPortsMu.Unlock()
	changed := false
	for key, active := range b.linuxPortState {
		want, exists := currentByKey[key]
		if !exists || !activeLinuxPortValid(active, want) {
			delete(b.linuxPortState, key)
			changed = true
		}
	}
	if changed {
		b.signalLinuxPortStateLocked()
	}
}

func (b *Backend) updateLinuxPortState(ctx context.Context, req api.Request) error {
	if b.id != "linux" {
		return api.Fail("invalid_argument", "Linux published port state is available only on the Linux engine")
	}
	state, err := api.Decode[api.LinuxPortBindings](req)
	if err != nil {
		return err
	}
	if len(state.Bindings) == 0 {
		b.closeLinuxPortRelays()
		return nil
	}
	current, err := b.currentLinuxPortBindings(ctx)
	if err != nil {
		return err
	}
	currentByKey := make(map[forwardKey]api.LinuxPortBinding, len(current))
	for _, binding := range current {
		currentByKey[forwardKey{container: binding.Container, index: binding.Index}] = binding
	}
	active := make(map[forwardKey]api.LinuxPortBinding, len(state.Bindings))
	for _, binding := range state.Bindings {
		key := forwardKey{container: binding.Container, index: binding.Index}
		if binding.Container == "" || binding.Index < 0 {
			return api.Fail("invalid_argument", "Linux published port state requires a container ID and nonnegative index")
		}
		if _, duplicate := active[key]; duplicate {
			return api.Fail("invalid_argument", "Linux published port state contains a duplicate container binding index")
		}
		want, exists := currentByKey[key]
		if !exists || binding.RelayPort == 0 || binding.RelayPort != want.RelayPort ||
			binding.Port.HostIP != want.Port.HostIP || binding.Port.Target != want.Port.Target ||
			binding.Port.Protocol != want.Port.Protocol || binding.Port.Published == 0 ||
			(want.Port.Published != 0 && binding.Port.Published != want.Port.Published) {
			return api.Fail("invalid_argument", "Linux published port state does not match a running managed container binding")
		}
		active[key] = binding
	}
	b.replaceLinuxPortState(active)
	return nil
}

func (b *Backend) replaceLinuxPortState(state map[forwardKey]api.LinuxPortBinding) {
	b.linuxPortsMu.Lock()
	b.linuxPortState = state
	b.signalLinuxPortStateLocked()
	b.linuxPortsMu.Unlock()
}

func (b *Backend) signalLinuxPortStateLocked() {
	if b.linuxPortChanged != nil {
		close(b.linuxPortChanged)
	}
	b.linuxPortChanged = make(chan struct{})
}

func (b *Backend) waitLinuxPortState(ctx context.Context, expected []api.LinuxPortBinding) error {
	if len(expected) == 0 {
		return nil
	}
	timer := time.NewTimer(linuxPortReadyTimeout)
	defer timer.Stop()
	for {
		b.linuxPortsMu.Lock()
		if b.linuxPortChanged == nil {
			b.linuxPortChanged = make(chan struct{})
		}
		changed := b.linuxPortChanged
		ready := true
		for _, want := range expected {
			active, ok := b.linuxPortState[forwardKey{container: want.Container, index: want.Index}]
			if !ok || !activeLinuxPortValid(active, want) {
				ready = false
				break
			}
		}
		b.linuxPortsMu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("UCD could not bind all Windows host ports within %s; check for port conflicts and engine.port_error events", linuxPortReadyTimeout)
		case <-changed:
		}
	}
}

func (b *Backend) waitLinuxContainerPorts(ctx context.Context, id string) error {
	if b.id != "linux" {
		return nil
	}
	result, err := b.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			// AutoRemove may delete a one-shot container after Docker has
			// successfully started it. Its exit result remains Docker's contract.
			return nil
		}
		return fmt.Errorf("inspect container published ports: %w", err)
	}
	inspection := result.Container
	if inspection.Config == nil || inspection.Config.Labels == nil {
		return nil
	}
	raw, managed := inspection.Config.Labels[linuxPortsLabel]
	if !managed || raw == "" {
		return nil
	}
	requested, err := parseLinuxPortMetadata(raw)
	if err != nil {
		return err
	}
	if inspection.State == nil || !inspection.State.Running {
		// A one-shot process can exit before its host listener becomes useful.
		return nil
	}
	var currentPorts network.PortMap
	if inspection.NetworkSettings != nil {
		currentPorts = inspection.NetworkSettings.Ports
	}
	dockerRelays, err := relayPortsFromPortMap(currentPorts, requested)
	if err != nil {
		return err
	}
	relays, err := b.hostRelayPorts(inspection.ID, requested, dockerRelays)
	if err != nil {
		return err
	}
	expected := make([]api.LinuxPortBinding, 0, len(requested))
	for index, port := range requested {
		relayPort := relays[linuxPortKey(port.Target, port.Protocol)]
		if relayPort == 0 {
			return fmt.Errorf("container %s has no active Linux loopback relay for %s", inspection.ID, linuxPortKey(port.Target, port.Protocol))
		}
		expected = append(expected, api.LinuxPortBinding{Container: inspection.ID, Index: index, Port: port, RelayPort: relayPort})
	}
	if err := b.waitLinuxPortState(ctx, expected); err != nil {
		return fmt.Errorf("container %s was started but its published ports are not ready: %w", inspection.ID, err)
	}
	return nil
}

func (b *Backend) waitLinuxProjectPorts(ctx context.Context, project string) error {
	if b.id != "linux" {
		return nil
	}
	result, err := b.docker.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		b.closeLinuxPortRelays()
		return fmt.Errorf("list Compose containers for published port readiness: %w", err)
	}
	expected := make([]api.LinuxPortBinding, 0)
	for _, summary := range result.Items {
		if summary.Labels["com.docker.compose.project"] != project {
			continue
		}
		raw, managed := summary.Labels[linuxPortsLabel]
		if !managed || raw == "" {
			continue
		}
		requested, err := parseLinuxPortMetadata(raw)
		if err != nil {
			return fmt.Errorf("container %s: %w", summary.ID, err)
		}
		dockerRelays, err := relayPortsFromSummary(summary, requested)
		if err != nil {
			return err
		}
		relays, err := b.hostRelayPorts(summary.ID, requested, dockerRelays)
		if err != nil {
			return err
		}
		for index, port := range requested {
			expected = append(expected, api.LinuxPortBinding{Container: summary.ID, Index: index, Port: port, RelayPort: relays[linuxPortKey(port.Target, port.Protocol)]})
		}
	}
	if err := b.waitLinuxPortState(ctx, expected); err != nil {
		return fmt.Errorf("Compose project %s was started but its published ports are not ready: %w", project, err)
	}
	return nil
}

func (b *Backend) activeLinuxPortBindings(containerID string, requested []api.Port, relays map[string]uint16) []api.LinuxPortBinding {
	b.linuxPortsMu.Lock()
	defer b.linuxPortsMu.Unlock()
	var active []api.LinuxPortBinding
	changed := false
	for index, original := range requested {
		key := forwardKey{container: containerID, index: index}
		binding, exists := b.linuxPortState[key]
		if !exists {
			continue
		}
		current := api.LinuxPortBinding{
			Container: containerID,
			Index:     index,
			Port:      original,
			RelayPort: relays[linuxPortKey(original.Target, original.Protocol)],
		}
		if !activeLinuxPortValid(binding, current) {
			delete(b.linuxPortState, key)
			changed = true
			continue
		}
		active = append(active, binding)
	}
	if changed {
		b.signalLinuxPortStateLocked()
	}
	return active
}

func relayPortsFromPortMap(ports network.PortMap, requested []api.Port) (linuxDockerRelayPorts, error) {
	wanted := make(map[string]struct{})
	for _, port := range requested {
		wanted[linuxPortKey(port.Target, port.Protocol)] = struct{}{}
	}
	relays := newLinuxDockerRelayPorts(len(wanted))
	for port, bindings := range ports {
		key := linuxPortKey(port.Num(), string(port.Proto()))
		if _, ok := wanted[key]; !ok {
			continue
		}
		for _, binding := range bindings {
			if !binding.HostIP.IsValid() {
				continue
			}
			portNumber, err := strconv.ParseUint(binding.HostPort, 10, 16)
			if err != nil || portNumber == 0 {
				continue
			}
			if err := relays.add(key, binding.HostIP, uint16(portNumber)); err != nil {
				return linuxDockerRelayPorts{}, fmt.Errorf("multiple Linux relay ports for %s", key)
			}
		}
	}
	return relays, nil
}

func (b *Backend) decorateLinuxManagedInspectPorts(inspect *container.InspectResponse, raw string) error {
	requested, err := parseLinuxPortMetadata(raw)
	if err != nil {
		return err
	}
	requestedBindings, managedKeys, err := dockerPortBindings(requested)
	if err != nil {
		return err
	}
	if inspect.HostConfig == nil {
		inspect.HostConfig = &container.HostConfig{}
	}
	if inspect.HostConfig.PortBindings == nil {
		inspect.HostConfig.PortBindings = make(network.PortMap)
	}
	for key, rawBindings := range requestedBindings {
		port, err := network.ParsePort(key)
		if err != nil {
			return fmt.Errorf("parse requested Docker port binding %s: %w", key, err)
		}
		var bindings []struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		}
		if err := json.Unmarshal(rawBindings, &bindings); err != nil {
			return fmt.Errorf("decode requested Docker port binding %s: %w", key, err)
		}
		publicBindings := make([]network.PortBinding, 0, len(bindings))
		for _, binding := range bindings {
			hostIP, err := netip.ParseAddr(binding.HostIP)
			if err != nil {
				return fmt.Errorf("parse requested host address %q: %w", binding.HostIP, err)
			}
			publicBindings = append(publicBindings, network.PortBinding{HostIP: hostIP, HostPort: binding.HostPort})
		}
		inspect.HostConfig.PortBindings[port] = publicBindings
	}
	if inspect.NetworkSettings == nil {
		inspect.NetworkSettings = &container.NetworkSettings{}
	}
	if inspect.NetworkSettings.Ports == nil {
		inspect.NetworkSettings.Ports = make(network.PortMap)
	}
	dockerRelays, err := relayPortsFromPortMap(inspect.NetworkSettings.Ports, requested)
	if err != nil {
		return err
	}
	var relays map[string]uint16
	if inspect.State != nil && inspect.State.Running {
		relays, err = b.hostRelayPorts(inspect.ID, requested, dockerRelays)
		if err != nil {
			return err
		}
	}
	for key := range managedKeys {
		port, err := network.ParsePort(key)
		if err != nil {
			return fmt.Errorf("parse managed Docker port binding %s: %w", key, err)
		}
		delete(inspect.NetworkSettings.Ports, port)
	}
	if inspect.State != nil && inspect.State.Running {
		for _, active := range b.activeLinuxPortBindings(inspect.ID, requested, relays) {
			port, err := network.ParsePort(linuxPortKey(active.Port.Target, active.Port.Protocol))
			if err != nil {
				return fmt.Errorf("parse active Docker port binding: %w", err)
			}
			hostIP, err := netip.ParseAddr(active.Port.HostIP)
			if err != nil {
				return fmt.Errorf("parse active host address %q: %w", active.Port.HostIP, err)
			}
			inspect.NetworkSettings.Ports[port] = append(inspect.NetworkSettings.Ports[port], network.PortBinding{
				HostIP: hostIP, HostPort: strconv.Itoa(int(active.Port.Published)),
			})
		}
	}
	return nil
}

// StartHostPortForward binds a native host endpoint and forwards it to target.
// The returned close function releases the listener and its active sessions.
func StartHostPortForward(port api.Port, target string) (api.Port, func(), error) {
	forward, err := startPortForward(port, target)
	if err != nil {
		return api.Port{}, nil, err
	}
	return forward.port, forward.close, nil
}
