// Package dnsfilter filters IPv4 and IPv6 DNS records for Linux containers.
package dnsfilter

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	dockerclient "github.com/moby/moby/client"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	maxConfigBytes     = 16 << 10
	maxDNSMessageBytes = 65535
	maxConcurrent      = 32
	serverPort         = "53"
	policyLabel        = "com.ucd.dns-inhibition"
)

const (
	// IPv4Address is the UCD-managed IPv4 DNS listener address on Linux loopback.
	IPv4Address = "169.254.253.53"
	// IPv6Address is the UCD-managed IPv6 DNS listener address on Linux loopback.
	IPv6Address = "fd42:7763:b25b:ffff::53"
)

// Config controls which address family questions are filtered. Empty Upstreams
// reads the Linux host's resolv.conf, preserving WSL and VPN DNS behavior.
type Config struct {
	Inhibition            string   `json:"inhibition"`
	DefaultNetworkingMode string   `json:"defaultNetworkingMode"`
	Upstreams             []string `json:"upstreams,omitempty"`
}

// WriteConfig atomically publishes a validated DNS policy for the Linux helper.
// The helper notices updates without restarting; callers must give it a
// root-owned configuration path.
func WriteConfig(path string, config Config) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("DNS filter configuration path must be absolute")
	}
	if err := validate(config); err != nil {
		return err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode DNS filter settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create DNS filter settings directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dnsfilter-*")
	if err != nil {
		return fmt.Errorf("create DNS filter settings: %w", err)
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write DNS filter settings: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync DNS filter settings: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close DNS filter settings: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("publish DNS filter settings: %w", err)
	}
	return nil
}

func validate(config Config) error {
	switch config.Inhibition {
	case "none", "auto", "ipv4", "ipv6":
	default:
		return fmt.Errorf("DNS inhibition must be none, auto, ipv4, or ipv6")
	}
	switch config.DefaultNetworkingMode {
	case "dual-stack", "ipv4only", "ipv6only":
	default:
		return errors.New("default networking mode must be dual-stack, ipv4only, or ipv6only")
	}
	if len(config.Upstreams) > 16 {
		return errors.New("DNS filter accepts at most 16 upstream servers")
	}
	usableUpstreams := 0
	for _, upstream := range config.Upstreams {
		ip := net.ParseIP(upstream)
		if ip == nil {
			return fmt.Errorf("DNS upstream %q must be an IP address", upstream)
		}
		if !isOwnDNSAddress(ip) {
			usableUpstreams++
		}
	}
	if len(config.Upstreams) > 0 && usableUpstreams == 0 {
		return errors.New("DNS upstreams contain only UCD DNS listener addresses")
	}
	return nil
}

func isOwnDNSAddress(ip net.IP) bool {
	return ip.Equal(net.ParseIP(IPv4Address)) || ip.Equal(net.ParseIP(IPv6Address))
}

func filteredType(config Config, questionType dnsmessage.Type) bool {
	if config.Inhibition == "ipv4" {
		return questionType == dnsmessage.TypeA
	}
	if config.Inhibition == "ipv6" {
		return questionType == dnsmessage.TypeAAAA
	}
	if config.Inhibition != "auto" {
		return false
	}
	switch config.DefaultNetworkingMode {
	case "ipv4only":
		return questionType == dnsmessage.TypeAAAA
	case "ipv6only":
		return questionType == dnsmessage.TypeA
	default:
		return false
	}
}

func filteredAnswer(query []byte, config Config) ([]byte, bool, error) {
	var request dnsmessage.Message
	if err := request.Unpack(query); err != nil {
		return nil, false, fmt.Errorf("parse DNS query: %w", err)
	}
	if request.Header.Response || len(request.Questions) != 1 {
		return nil, false, errors.New("DNS request must contain one query question")
	}
	question := request.Questions[0]
	if question.Class != dnsmessage.ClassINET || !filteredType(config, question.Type) {
		return nil, false, nil
	}
	response := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 request.Header.ID,
			Response:           true,
			OpCode:             request.Header.OpCode,
			RecursionDesired:   request.Header.RecursionDesired,
			RecursionAvailable: true,
			CheckingDisabled:   request.Header.CheckingDisabled,
			RCode:              dnsmessage.RCodeSuccess,
		},
		Questions: []dnsmessage.Question{question},
	}
	answer, err := response.Pack()
	if err != nil {
		return nil, false, fmt.Errorf("encode filtered DNS response: %w", err)
	}
	return answer, true, nil
}

type settingsFile struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	data Config
	init bool
}

func (f *settingsFile) load() (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Inhibition: "auto", DefaultNetworkingMode: "ipv4only"}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("stat DNS filter settings: %w", err)
	}
	if info.Size() > maxConfigBytes {
		return Config{}, errors.New("DNS filter settings exceed 16 KiB")
	}
	if f.init && info.ModTime().Equal(f.mod) {
		return f.data, nil
	}
	file, err := os.Open(f.path)
	if err != nil {
		return Config{}, fmt.Errorf("open DNS filter settings: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxConfigBytes+1))
	decoder.DisallowUnknownFields()
	var next Config
	if err := decoder.Decode(&next); err != nil {
		return Config{}, fmt.Errorf("decode DNS filter settings: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Config{}, errors.New("DNS filter settings must contain one JSON object")
	}
	if err := validate(next); err != nil {
		return Config{}, err
	}
	next.Upstreams = append([]string(nil), next.Upstreams...)
	f.data, f.mod, f.init = next, info.ModTime(), true
	return next, nil
}

type resolverFile struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	data []string
	init bool
}

func (f *resolverFile) servers(configured []string) ([]string, error) {
	if len(configured) != 0 {
		servers := make([]string, 0, len(configured))
		for _, upstream := range configured {
			ip := net.ParseIP(upstream)
			if ip == nil {
				return nil, fmt.Errorf("configured DNS upstream %q must be an IP address", upstream)
			}
			if !isOwnDNSAddress(ip) {
				servers = append(servers, net.JoinHostPort(upstream, serverPort))
			}
		}
		if len(servers) == 0 {
			return nil, errors.New("configured DNS upstreams contain only UCD DNS listener addresses")
		}
		return servers, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	info, err := os.Stat(f.path)
	if err != nil {
		return nil, fmt.Errorf("stat Linux resolver configuration: %w", err)
	}
	if f.init && f.mod.Equal(info.ModTime()) {
		return append([]string(nil), f.data...), nil
	}
	file, err := os.Open(f.path)
	if err != nil {
		return nil, fmt.Errorf("open Linux resolver configuration: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 64<<10))
	var servers []string
	selfResolver := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "nameserver" {
			ip := net.ParseIP(fields[1])
			if ip == nil {
				continue
			}
			if isOwnDNSAddress(ip) {
				selfResolver = true
				continue
			}
			servers = append(servers, net.JoinHostPort(fields[1], serverPort))
			if len(servers) == 8 {
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Linux resolver configuration: %w", err)
	}
	if len(servers) == 0 {
		if selfResolver {
			return nil, errors.New("Linux resolver configuration points only to UCD DNS listeners; configure another upstream to avoid a forwarding loop")
		}
		return nil, errors.New("Linux resolver configuration has no usable nameserver")
	}
	f.data, f.mod, f.init = append([]string(nil), servers...), info.ModTime(), true
	return servers, nil
}

type exchangeFunc func(context.Context, []byte, bool, []string) ([]byte, error)

type networkPolicy struct {
	subnet  netip.Prefix
	gateway netip.Addr
	mode    string
	ipv4    bool
	ipv6    bool
}

type networkPolicies struct {
	docker *dockerclient.Client
	mu     sync.Mutex
	until  time.Time
	items  []networkPolicy
}

func (p *networkPolicies) lookupWithDefault(ctx context.Context, source net.Addr, defaultMode string) (networkPolicy, bool, error) {
	if p == nil || p.docker == nil || source == nil {
		return networkPolicy{}, false, nil
	}
	address, _, err := net.SplitHostPort(source.String())
	if err != nil {
		return networkPolicy{}, false, fmt.Errorf("parse DNS client address: %w", err)
	}
	ip, err := netip.ParseAddr(strings.Trim(address, "[]"))
	if err != nil {
		return networkPolicy{}, false, fmt.Errorf("parse DNS client IP address: %w", err)
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	items, err := p.snapshot(ctx)
	if err != nil {
		return networkPolicy{}, false, err
	}
	selected, found, err := selectPolicy(items, ip)
	if err != nil {
		return networkPolicy{}, false, err
	}
	if found && selected.mode == "" {
		selected.mode = defaultMode
	}
	return selected, found, nil
}

func selectPolicy(items []networkPolicy, ip netip.Addr) (networkPolicy, bool, error) {
	var selected networkPolicy
	found := false
	for _, item := range items {
		if !item.subnet.Contains(ip) {
			continue
		}
		if !found || item.subnet.Bits() > selected.subnet.Bits() {
			selected, found = item, true
			continue
		}
		if item.subnet.Bits() == selected.subnet.Bits() && (selected.mode != item.mode || selected.ipv4 != item.ipv4 || selected.ipv6 != item.ipv6 || selected.gateway != item.gateway) {
			return networkPolicy{}, false, errors.New("DNS client address matches conflicting Docker network policies")
		}
	}
	return selected, found, nil
}

func (p *networkPolicies) snapshot(ctx context.Context) ([]networkPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.until) {
		return append([]networkPolicy(nil), p.items...), nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := p.docker.NetworkList(requestCtx, dockerclient.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list Docker networks for DNS policy: %w", err)
	}
	items := make([]networkPolicy, 0, len(result.Items))
	for _, summary := range result.Items {
		if summary.Driver != "bridge" {
			continue
		}
		mode, exists := summary.Labels[policyLabel]
		if exists && mode != "none" && mode != "auto" && mode != "ipv4" && mode != "ipv6" {
			return nil, fmt.Errorf("Docker network %q has an invalid %s label", summary.Name, policyLabel)
		}
		for _, config := range summary.IPAM.Config {
			if !config.Subnet.IsValid() {
				continue
			}
			items = append(items, networkPolicy{
				subnet: config.Subnet.Masked(), mode: mode,
				gateway: config.Gateway,
				ipv4:    summary.EnableIPv4, ipv6: summary.EnableIPv6,
			})
		}
	}
	p.items = items
	p.until = time.Now().Add(time.Second)
	return append([]networkPolicy(nil), items...), nil
}

// serve owns both listener goroutines; cancellation closes listeners and joins
// them before it returns.
func serve(ctx context.Context, address string, settings *settingsFile, resolvers *resolverFile, policies *networkPolicies, forward exchangeFunc, started chan<- struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		return fmt.Errorf("listen for container DNS over UDP on %s: %w", address, err)
	}
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		_ = udp.Close()
		return fmt.Errorf("listen for container DNS over TCP on %s: %w", address, err)
	}
	if err := ctx.Err(); err != nil {
		_ = udp.Close()
		_ = tcp.Close()
		return err
	}
	started <- struct{}{}
	closeListeners := func() {
		_ = udp.Close()
		_ = tcp.Close()
	}
	serverCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(serverCtx, closeListeners)
	defer func() {
		cancel()
		stop()
		closeListeners()
	}()
	results := make(chan error, 2)
	go func() { results <- serveUDP(serverCtx, udp, settings, resolvers, policies, forward) }()
	go func() { results <- serveTCP(serverCtx, tcp, settings, resolvers, policies, forward) }()
	first := <-results
	cancel()
	closeListeners()
	second := <-results
	if ctx.Err() != nil {
		return nil
	}
	if first != nil && !errors.Is(first, net.ErrClosed) && !errors.Is(first, context.Canceled) {
		return first
	}
	if second != nil && !errors.Is(second, net.ErrClosed) && !errors.Is(second, context.Canceled) {
		return second
	}
	return errors.New("DNS listener stopped unexpectedly")
}

func serveUDP(ctx context.Context, listener net.PacketConn, settings *settingsFile, resolvers *resolverFile, policies *networkPolicies, forward exchangeFunc) error {
	semaphore := make(chan struct{}, maxConcurrent)
	var workers sync.WaitGroup
	defer workers.Wait()
	buffer := make([]byte, maxDNSMessageBytes)
	for {
		n, addr, err := listener.ReadFrom(buffer)
		if err != nil {
			return err
		}
		packet := append([]byte(nil), buffer[:n]...)
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		workers.Add(1)
		go func(addr net.Addr, packet []byte) {
			defer workers.Done()
			defer func() { <-semaphore }()
			requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			response, err := resolve(requestCtx, packet, addr, false, settings, resolvers, policies, forward)
			if err != nil {
				response = failureResponse(packet)
			}
			if len(response) != 0 {
				_, _ = listener.WriteTo(response, addr)
			}
		}(addr, packet)
	}
}

func serveTCP(ctx context.Context, listener net.Listener, settings *settingsFile, resolvers *resolverFile, policies *networkPolicies, forward exchangeFunc) error {
	semaphore := make(chan struct{}, maxConcurrent)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			_ = conn.Close()
			return ctx.Err()
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-semaphore }()
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			for {
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				var length [2]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					return
				}
				size := int(binary.BigEndian.Uint16(length[:]))
				if size == 0 || size > maxDNSMessageBytes {
					return
				}
				query := make([]byte, size)
				if _, err := io.ReadFull(conn, query); err != nil {
					return
				}
				requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				response, err := resolve(requestCtx, query, conn.RemoteAddr(), true, settings, resolvers, policies, forward)
				cancel()
				if err != nil {
					response = failureResponse(query)
				}
				if len(response) == 0 || len(response) > 65535 {
					return
				}
				binary.BigEndian.PutUint16(length[:], uint16(len(response)))
				if err := writeAll(conn, append(length[:], response...)); err != nil {
					return
				}
			}
		}()
	}
}

func resolve(ctx context.Context, query []byte, source net.Addr, tcp bool, settings *settingsFile, resolvers *resolverFile, policies *networkPolicies, forward exchangeFunc) ([]byte, error) {
	config, err := settings.load()
	if err != nil {
		return nil, err
	}
	policy, found, err := policies.lookupWithDefault(ctx, source, config.Inhibition)
	if err != nil {
		return nil, err
	} else if found {
		config, err = applyNetworkPolicy(config, policy)
		if err != nil {
			return nil, err
		}
	}
	answer, filtered, err := filteredAnswer(query, config)
	if err != nil || filtered {
		return answer, err
	}
	if answer, handled, err := dockerHostAnswer(query, filepath.Join(filepath.Dir(settings.path), "hostaliases.json"), policy.gateway); handled {
		return answer, err
	}
	upstreams, err := resolvers.servers(config.Upstreams)
	if err != nil {
		return nil, err
	}
	return forward(ctx, query, tcp, upstreams)
}

func applyNetworkPolicy(config Config, policy networkPolicy) (Config, error) {
	config.Inhibition = policy.mode
	if policy.mode != "auto" {
		return config, nil
	}
	switch {
	case policy.ipv4 && !policy.ipv6:
		config.DefaultNetworkingMode = "ipv4only"
	case !policy.ipv4 && policy.ipv6:
		config.DefaultNetworkingMode = "ipv6only"
	case policy.ipv4 && policy.ipv6:
		config.DefaultNetworkingMode = "dual-stack"
	default:
		return Config{}, errors.New("Docker network has both IPv4 and IPv6 disabled")
	}
	return config, nil
}

func failureResponse(query []byte) []byte {
	if len(query) < 2 {
		return nil
	}
	var request dnsmessage.Message
	if err := request.Unpack(query); err != nil || request.Header.Response || len(request.Questions) != 1 {
		return nil
	}
	response := dnsmessage.Message{Header: dnsmessage.Header{
		ID: request.Header.ID, Response: true, OpCode: request.Header.OpCode,
		RecursionDesired: request.Header.RecursionDesired, RecursionAvailable: true,
		RCode: dnsmessage.RCodeServerFailure,
	}, Questions: request.Questions}
	answer, _ := response.Pack()
	return answer
}

func exchange(ctx context.Context, query []byte, tcp bool, upstreams []string) ([]byte, error) {
	var lastErr error
	for _, upstream := range upstreams {
		answer, err := exchangeOne(ctx, query, tcp, upstream)
		if err == nil {
			return answer, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		return nil, errors.New("no DNS upstreams are configured")
	}
	return nil, fmt.Errorf("query Linux DNS upstreams: %w", lastErr)
}

func exchangeOne(ctx context.Context, query []byte, tcp bool, upstream string) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(requestCtx, network, upstream)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	if contextDeadline, ok := requestCtx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
	if tcp {
		if len(query) > 65535 {
			return nil, errors.New("DNS query is too large")
		}
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(query)))
		if err := writeAll(conn, append(length[:], query...)); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil, err
		}
		size := int(binary.BigEndian.Uint16(length[:]))
		if size == 0 || size > 65535 {
			return nil, errors.New("invalid DNS TCP response length")
		}
		answer := make([]byte, size)
		if _, err := io.ReadFull(conn, answer); err != nil {
			return nil, err
		}
		if _, err := validateUpstreamAnswer(query, answer); err != nil {
			return nil, err
		}
		return answer, nil
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	answer := make([]byte, maxDNSMessageBytes)
	count, err := conn.Read(answer)
	if err != nil {
		return nil, err
	}
	answer = answer[:count]
	response, err := validateUpstreamAnswer(query, answer)
	if err != nil {
		return nil, err
	}
	if !response.Header.Truncated {
		return answer, nil
	}
	return exchangeOne(ctx, query, true, upstream)
}

func validateUpstreamAnswer(query, answer []byte) (dnsmessage.Message, error) {
	var request, response dnsmessage.Message
	if err := request.Unpack(query); err != nil {
		return dnsmessage.Message{}, fmt.Errorf("parse DNS query: %w", err)
	}
	if err := response.Unpack(answer); err != nil {
		return dnsmessage.Message{}, fmt.Errorf("parse DNS upstream response: %w", err)
	}
	if !response.Header.Response || response.Header.ID != request.Header.ID || response.Header.OpCode != request.Header.OpCode {
		return dnsmessage.Message{}, errors.New("DNS upstream returned an invalid response identifier or flag")
	}
	if len(response.Questions) != len(request.Questions) {
		return dnsmessage.Message{}, errors.New("DNS upstream returned a mismatched question count")
	}
	for i, question := range request.Questions {
		got := response.Questions[i]
		if !strings.EqualFold(got.Name.String(), question.Name.String()) || got.Type != question.Type || got.Class != question.Class {
			return dnsmessage.Message{}, errors.New("DNS upstream returned a mismatched question")
		}
	}
	return response, nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
