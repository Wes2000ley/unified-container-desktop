package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

// Windows NAT does not accept a specific host IP for published ports. Compose
// creates the containers without native bindings; these labels retain the
// requested bindings for the controller's listeners.
const windowsPortsLabel = "com.skyri.ucd.published-ports"

func windowsRuntimeProject(project *types.Project) (*types.Project, error) {
	runtimeProject := *project
	runtimeProject.Services = make(types.Services, len(project.Services))
	for _, name := range project.ServiceNames() {
		service := project.Services[name]
		service.Labels = maps.Clone(service.Labels)
		if _, exists := service.Labels[windowsPortsLabel]; exists {
			return nil, fmt.Errorf("service %s uses reserved label %s", name, windowsPortsLabel)
		}
		service.Expose = append([]string(nil), service.Expose...)
		ports := make([]api.Port, 0, len(service.Ports))
		for _, p := range service.Ports {
			protocol := strings.ToLower(p.Protocol)
			if protocol == "" {
				protocol = "tcp"
			}
			if protocol != "tcp" && protocol != "udp" {
				return nil, fmt.Errorf("service %s: published port protocol %q is unsupported; use tcp or udp", name, p.Protocol)
			}
			if p.Target == 0 || p.Target > 65535 {
				return nil, fmt.Errorf("service %s has invalid target port %d", name, p.Target)
			}
			published := uint64(0)
			if p.Published != "" {
				var err error
				published, err = strconv.ParseUint(p.Published, 10, 16)
				if err != nil {
					return nil, fmt.Errorf("service %s has invalid published port %q; use a single port", name, p.Published)
				}
			}
			host := p.HostIP
			if host == "" {
				host = "127.0.0.1"
			}
			address, err := netip.ParseAddr(host)
			if err != nil || address.Zone() != "" {
				return nil, fmt.Errorf("service %s has invalid published host IP %q", name, host)
			}
			ports = append(ports, api.Port{HostIP: address.Unmap().String(), Published: uint16(published), Target: uint16(p.Target), Protocol: protocol})
			service.Expose = append(service.Expose, strconv.Itoa(int(p.Target))+"/"+protocol)
		}
		service.Ports = nil
		if len(ports) > 0 {
			encoded, err := json.Marshal(ports)
			if err != nil {
				return nil, fmt.Errorf("encode service %s port metadata: %w", name, err)
			}
			if service.Labels == nil {
				service.Labels = types.Labels{}
			}
			service.Labels[windowsPortsLabel] = string(encoded)
		}
		runtimeProject.Services[name] = service
	}
	return &runtimeProject, nil
}

type forwardKey struct {
	container string
	index     int
}
type desiredForward struct {
	port      api.Port
	target    string
	container container.Summary
}

type portReconcileFailure struct {
	container container.Summary
	err       error
}

type portReconcileResult struct {
	global   error
	failures []portReconcileFailure
}

func (r portReconcileResult) errFor(matches func(container.Summary) bool) error {
	errs := []error{r.global}
	for _, failure := range r.failures {
		if matches == nil || matches(failure.container) {
			errs = append(errs, failure.err)
		}
	}
	return errors.Join(errs...)
}

// An action returns only errors for its container or project. The event loop
// still receives every failure and retries against its service-owned context.
func (b *Backend) reconcileActionPorts(ctx context.Context, matches func(container.Summary) bool) error {
	result := b.windowsPortSnapshot(ctx, false)
	if err := result.errFor(nil); err != nil {
		if ctx.Err() == nil {
			b.events.Publish(api.Event{Engine: b.id, Type: "engine.port_error", Message: "reconcile published ports: " + err.Error()})
		}
		select {
		case b.portReconcile <- struct{}{}:
		default:
		}
	}
	return result.errFor(matches)
}

type portForward struct {
	port        api.Port
	target      string
	cancel      context.CancelFunc
	tcp         *net.TCPListener
	udp         *net.UDPConn
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
	sessions    map[string]*net.UDPConn
}

func validateForwardPort(p api.Port) error {
	address, err := netip.ParseAddr(p.HostIP)
	if err != nil || address.Zone() != "" || p.Target == 0 || (p.Protocol != "tcp" && p.Protocol != "udp") {
		return fmt.Errorf("invalid published port metadata: host %q, target %d, protocol %q", p.HostIP, p.Target, p.Protocol)
	}
	return nil
}

// reconcileWindowsPorts takes a fresh Docker snapshot after subscription and
// lifecycle events. If Docker cannot verify the endpoints, old listeners close.
func (b *Backend) reconcileWindowsPorts(ctx context.Context) error {
	return b.windowsPortSnapshot(ctx, true).errFor(nil)
}

func (b *Backend) windowsPortSnapshot(ctx context.Context, closeOnListFailure bool) portReconcileResult {
	var snapshot portReconcileResult
	if b.id != "windows" {
		return snapshot
	}
	b.portMu.Lock()
	defer b.portMu.Unlock()
	if err := ctx.Err(); err != nil {
		snapshot.global = err
		return snapshot
	}
	result, err := b.docker.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		if ctx.Err() != nil {
			snapshot.global = ctx.Err()
			return snapshot
		}
		if closeOnListFailure {
			b.closePortsLocked()
		}
		snapshot.global = fmt.Errorf("list running containers: %w", err)
		return snapshot
	}
	desired := make(map[forwardKey]desiredForward)
	drivers := make(map[string]string)
	for _, c := range result.Items {
		if err := ctx.Err(); err != nil {
			snapshot.global = err
			return snapshot
		}
		raw, ok := c.Labels[windowsPortsLabel]
		if !ok || string(c.State) != "running" {
			continue
		}
		ports, endpoint, err := b.containerForwardEndpoint(ctx, c, raw, drivers)
		if ctx.Err() != nil {
			snapshot.global = ctx.Err()
			return snapshot
		}
		if err != nil {
			snapshot.failures = append(snapshot.failures, portReconcileFailure{container: c, err: err})
			continue
		}
		for i, port := range ports {
			key := forwardKey{container: c.ID, index: i}
			desired[key] = desiredForward{port: port, target: net.JoinHostPort(endpoint.String(), strconv.Itoa(int(port.Target))), container: c}
		}
	}
	// Never apply a partially collected snapshot after request cancellation.
	if err := ctx.Err(); err != nil {
		snapshot.global = err
		return snapshot
	}
	for key, f := range b.forwards {
		want, ok := desired[key]
		// Published zero means an OS-assigned port, so compare against the
		// requested value stored separately in the desired binding key.
		if !ok || f.target != want.target || f.port.HostIP != want.port.HostIP || f.port.Target != want.port.Target || f.port.Protocol != want.port.Protocol || (want.port.Published != 0 && f.port.Published != want.port.Published) {
			f.close()
			delete(b.forwards, key)
		}
	}
	if b.forwards == nil {
		b.forwards = make(map[forwardKey]*portForward)
	}
	keys := make([]forwardKey, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].container == keys[j].container {
			return keys[i].index < keys[j].index
		}
		return keys[i].container < keys[j].container
	})
	for _, key := range keys {
		if _, ok := b.forwards[key]; ok {
			continue
		}
		want := desired[key]
		f, err := startPortForward(want.port, want.target)
		if err != nil {
			snapshot.failures = append(snapshot.failures, portReconcileFailure{container: want.container, err: fmt.Errorf("container %s cannot bind %s %s:%d: %w", key.container, want.port.Protocol, want.port.HostIP, want.port.Published, err)})
			continue
		}
		b.forwards[key] = f
	}
	return snapshot
}

func (b *Backend) containerForwardEndpoint(ctx context.Context, c container.Summary, raw string, drivers map[string]string) ([]api.Port, netip.Addr, error) {
	var ports []api.Port
	if err := json.Unmarshal([]byte(raw), &ports); err != nil || len(ports) == 0 || len(ports) > 1024 {
		return nil, netip.Addr{}, fmt.Errorf("container %s has invalid UCD published port metadata", c.ID)
	}
	for _, port := range ports {
		if err := validateForwardPort(port); err != nil {
			return nil, netip.Addr{}, fmt.Errorf("container %s: %w", c.ID, err)
		}
	}
	inspection, err := b.docker.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, netip.Addr{}, fmt.Errorf("inspect container %s: %w", c.ID, err)
	}
	if inspection.Container.NetworkSettings == nil {
		return nil, netip.Addr{}, fmt.Errorf("container %s has no NAT network endpoint", c.ID)
	}
	networks := inspection.Container.NetworkSettings.Networks
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	var endpoint netip.Addr
	for _, name := range names {
		ep := networks[name]
		if ep == nil || ep.NetworkID == "" {
			continue
		}
		driver, found := drivers[ep.NetworkID]
		if !found {
			network, err := b.docker.NetworkInspect(ctx, ep.NetworkID, client.NetworkInspectOptions{})
			if err != nil {
				return nil, netip.Addr{}, fmt.Errorf("inspect network %s: %w", name, err)
			}
			driver = network.Network.Driver
			drivers[ep.NetworkID] = driver
		}
		if driver != "nat" {
			continue
		}
		ip := ep.IPAddress.Unmap()
		if !ip.Is4() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			return nil, netip.Addr{}, fmt.Errorf("container %s has an invalid NAT address %q", c.ID, ip)
		}
		if endpoint.IsValid() {
			return nil, netip.Addr{}, fmt.Errorf("container %s has multiple NAT endpoints; attach it to a single NAT network", c.ID)
		}
		endpoint = ip
	}
	if !endpoint.IsValid() {
		return nil, netip.Addr{}, fmt.Errorf("container %s has no usable NAT endpoint", c.ID)
	}
	return ports, endpoint, nil
}

func (b *Backend) closePortsLocked() {
	for key, f := range b.forwards {
		f.close()
		delete(b.forwards, key)
	}
}
func (b *Backend) closeWindowsPorts() {
	b.portMu.Lock()
	defer b.portMu.Unlock()
	b.closePortsLocked()
}
func (b *Backend) forwardedPorts(container string) []api.Port {
	b.portMu.Lock()
	defer b.portMu.Unlock()
	var keys []forwardKey
	for key := range b.forwards {
		if key.container == container {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].index < keys[j].index })
	ports := make([]api.Port, 0, len(keys))
	for _, key := range keys {
		ports = append(ports, b.forwards[key].port)
	}
	return ports
}

func startPortForward(port api.Port, target string) (*portForward, error) {
	if err := validateForwardPort(port); err != nil {
		return nil, err
	}
	endpoint, err := netip.ParseAddrPort(target)
	if err != nil || !endpoint.Addr().IsValid() || endpoint.Port() == 0 {
		return nil, fmt.Errorf("invalid container endpoint %q", target)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &portForward{port: port, target: target, cancel: cancel, connections: make(map[net.Conn]struct{}), sessions: make(map[string]*net.UDPConn)}
	address := net.JoinHostPort(port.HostIP, strconv.Itoa(int(port.Published)))
	if port.Protocol == "tcp" {
		addr, err := net.ResolveTCPAddr("tcp", address)
		if err != nil {
			cancel()
			return nil, err
		}
		f.tcp, err = net.ListenTCP("tcp", addr)
		if err != nil {
			cancel()
			return nil, err
		}
		f.port.Published = uint16(f.tcp.Addr().(*net.TCPAddr).Port)
		go f.serveTCP(ctx)
	} else {
		addr, err := net.ResolveUDPAddr("udp", address)
		if err != nil {
			cancel()
			return nil, err
		}
		f.udp, err = net.ListenUDP("udp", addr)
		if err != nil {
			cancel()
			return nil, err
		}
		f.port.Published = uint16(f.udp.LocalAddr().(*net.UDPAddr).Port)
		go f.serveUDP()
	}
	return f, nil
}

func (f *portForward) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.connections[conn] = struct{}{}
	return true
}
func (f *portForward) untrack(conn net.Conn) {
	f.mu.Lock()
	delete(f.connections, conn)
	f.mu.Unlock()
	_ = conn.Close()
}
func (f *portForward) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	f.cancel()
	if f.tcp != nil {
		_ = f.tcp.Close()
	}
	if f.udp != nil {
		_ = f.udp.Close()
	}
	for c := range f.connections {
		_ = c.Close()
	}
	for _, c := range f.sessions {
		_ = c.Close()
	}
}
func (f *portForward) serveTCP(ctx context.Context) {
	for {
		incoming, err := f.tcp.AcceptTCP()
		if err != nil {
			return
		}
		if !f.track(incoming) {
			_ = incoming.Close()
			return
		}
		go func() {
			defer f.untrack(incoming)
			dialer := net.Dialer{Timeout: 5 * time.Second}
			upstream, err := dialer.DialContext(ctx, "tcp", f.target)
			if err != nil {
				return
			}
			if !f.track(upstream) {
				_ = upstream.Close()
				return
			}
			defer f.untrack(upstream)
			done := make(chan struct{})
			go func() {
				_, _ = io.Copy(upstream, incoming)
				if c, ok := upstream.(*net.TCPConn); ok {
					_ = c.CloseWrite()
				}
				close(done)
			}()
			_, _ = io.Copy(incoming, upstream)
			_ = incoming.CloseWrite()
			<-done
		}()
	}
}

const udpIdleTimeout = 60 * time.Second
const maxUDPSessions = 1024

func (f *portForward) serveUDP() {
	buffer := make([]byte, 65535)
	for {
		n, remote, err := f.udp.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		key := remote.String()
		f.mu.Lock()
		upstream := f.sessions[key]
		if upstream == nil && !f.closed && len(f.sessions) < maxUDPSessions {
			target, resolveErr := net.ResolveUDPAddr("udp", f.target)
			if resolveErr == nil {
				upstream, _ = net.DialUDP("udp", nil, target)
			}
			if upstream != nil {
				f.sessions[key] = upstream
				go f.readUDPReplies(key, remote, upstream)
			}
		}
		if upstream != nil {
			_ = upstream.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		}
		f.mu.Unlock()
		if upstream != nil {
			_, _ = upstream.Write(buffer[:n])
		}
	}
}
func (f *portForward) readUDPReplies(key string, remote *net.UDPAddr, upstream *net.UDPConn) {
	defer func() {
		f.mu.Lock()
		if f.sessions[key] == upstream {
			delete(f.sessions, key)
		}
		f.mu.Unlock()
		_ = upstream.Close()
	}()
	buffer := make([]byte, 65535)
	for {
		n, err := upstream.Read(buffer)
		if err != nil {
			return
		}
		_ = upstream.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		if _, err = f.udp.WriteToUDP(buffer[:n], remote); err != nil {
			return
		}
	}
}
