package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/skyri/ucd/internal/api"
)

func windowsTestBackend(t *testing.T, handler http.HandlerFunc) *Backend {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	b, err := New("windows", "tcp://"+strings.TrimPrefix(server.URL, "http://"), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	b.hostIPv4 = testHostIPv4
	return b
}

func TestWindowsComposeRuntimePorts(t *testing.T) {
	b := windowsTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Docker request %s", r.URL)
		w.WriteHeader(500)
	})
	source := "services:\n  web:\n    image: example\n    ports:\n      - '8080:80'\n      - '127.0.0.2:5353:53/udp'\n"
	value, err := call(t, b, "compose.import", api.ComposeParams{Project: api.ProjectPart{Engine: "windows", Name: "demo"}, YAML: source})
	if err != nil {
		t.Fatal(err)
	}
	part := value.(api.ProjectPart)
	project, err := b.loadProject(context.Background(), part)
	if err != nil {
		t.Fatal(err)
	}
	runtimeProject, err := windowsRuntimeProject(project)
	if err != nil {
		t.Fatal(err)
	}
	original, transformed := project.Services["web"], runtimeProject.Services["web"]
	if len(original.Ports) != 2 || len(transformed.Ports) != 0 || !reflect.DeepEqual([]string(transformed.Expose), []string{"80/tcp", "53/udp"}) {
		t.Fatalf("original ports %#v; runtime ports %#v, expose %#v", original.Ports, transformed.Ports, transformed.Expose)
	}
	var ports []api.Port
	if err := json.Unmarshal([]byte(transformed.Labels[windowsPortsLabel]), &ports); err != nil {
		t.Fatal(err)
	}
	want := []api.Port{{HostIP: "127.0.0.1", Published: 8080, Target: 80, Protocol: "tcp"}, {HostIP: "127.0.0.2", Published: 5353, Target: 53, Protocol: "udp"}}
	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("metadata %#v, want %#v", ports, want)
	}
	if _, ok := original.Labels[windowsPortsLabel]; ok {
		t.Fatal("runtime metadata changed source model")
	}
	if data, err := os.ReadFile(filepath.Join(part.Directory, "compose.yaml")); err != nil || string(data) != source {
		t.Fatalf("source file changed: %q %v", data, err)
	}
	reported, err := call(t, b, "compose.ports", api.ComposeParams{Project: part})
	if err != nil || !reflect.DeepEqual(reported, want) {
		t.Fatalf("reported ports %#v: %v", reported, err)
	}

	cases := []struct {
		name  string
		port  types.ServicePortConfig
		label bool
	}{
		{"protocol", types.ServicePortConfig{Target: 80, Protocol: "sctp"}, false},
		{"host", types.ServicePortConfig{Target: 80, HostIP: "example.test"}, false},
		{"target", types.ServicePortConfig{Target: 0}, false},
		{"range", types.ServicePortConfig{Target: 80, Published: "8000-8001"}, false},
		{"reserved label", types.ServicePortConfig{Target: 80}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := types.ServiceConfig{}
			svc.Ports = []types.ServicePortConfig{tc.port}
			if tc.label {
				svc.Labels = types.Labels{windowsPortsLabel: "spoof"}
			}
			_, err := windowsRuntimeProject(&types.Project{Services: types.Services{"web": svc}})
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestWindowsTCPAndUDPForwarding(t *testing.T) {
	tcpServer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpServer.Close()
	go func() {
		for {
			c, err := tcpServer.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	udpServer, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpServer.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, peer, err := udpServer.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = udpServer.WriteTo(buf[:n], peer)
		}
	}()
	for _, tc := range []struct{ protocol, target string }{{"tcp", tcpServer.Addr().String()}, {"udp", udpServer.LocalAddr().String()}} {
		t.Run(tc.protocol, func(t *testing.T) {
			f, err := startPortForward(api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: tc.protocol}, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(f.close)
			var boundIP net.IP
			if tc.protocol == "tcp" {
				boundIP = f.tcp.Addr().(*net.TCPAddr).IP
			} else {
				boundIP = f.udp.LocalAddr().(*net.UDPAddr).IP
			}
			if !boundIP.Equal(net.ParseIP("127.0.0.1")) {
				f.close()
				t.Fatal("listener did not bind the requested loopback address")
			}
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(f.port.Published)))
			c, err := net.Dial(tc.protocol, address)
			if err != nil {
				f.close()
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			for _, message := range []string{"first", "second"} {
				if _, err := c.Write([]byte(message)); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(message))
				if _, err := io.ReadFull(c, got); err != nil || string(got) != message {
					t.Fatalf("reply %q: %v", got, err)
				}
			}
			f.close()
			if tc.protocol == "tcp" {
				buf := make([]byte, 1)
				if _, err := c.Read(buf); err == nil {
					t.Fatal("active TCP connection survived close")
				}
				replacement, err := net.Listen("tcp", address)
				if err != nil {
					t.Fatalf("listener not released: %v", err)
				}
				replacement.Close()
			} else {
				replacement, err := net.ListenPacket("udp", address)
				if err != nil {
					t.Fatalf("listener not released: %v", err)
				}
				replacement.Close()
			}
		})
	}
	for _, tc := range []struct {
		port   api.Port
		target string
	}{
		{api.Port{HostIP: "not-an-ip", Target: 80, Protocol: "tcp"}, "127.0.0.1:80"},
		{api.Port{HostIP: "127.0.0.1", Target: 0, Protocol: "tcp"}, "127.0.0.1:80"},
		{api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "sctp"}, "127.0.0.1:80"},
		{api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, "example.test:80"},
		{api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, "127.0.0.1:0"},
	} {
		if f, err := startPortForward(tc.port, tc.target); err == nil {
			f.close()
			t.Errorf("accepted invalid forward %#v -> %s", tc.port, tc.target)
		}
	}
}

type fakeWindowsDaemon struct {
	mu             sync.Mutex
	running        bool
	badList        bool
	badSecond      bool
	badLabel       string
	ip             string
	label          string
	streams        chan chan string
	blockInspect   bool
	inspectStarted chan struct{}
}

func (d *fakeWindowsDaemon) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_ping" {
		w.Header().Set("API-Version", "1.45")
		w.WriteHeader(200)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.45")
	d.mu.Lock()
	running, badList, badSecond, badLabel, ip, label, blockInspect := d.running, d.badList, d.badSecond, d.badLabel, d.ip, d.label, d.blockInspect
	d.mu.Unlock()
	switch path {
	case "/containers/json":
		if badList {
			http.Error(w, "offline", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if running {
			items := []any{map[string]any{"Id": "abc", "Names": []string{"/demo-web-1"}, "State": "running", "Labels": map[string]string{windowsPortsLabel: label, "com.docker.compose.project": "demo"}, "Ports": []any{map[string]any{"PrivatePort": 80, "Type": "tcp"}, map[string]any{"PrivatePort": 53, "Type": "udp"}}}}
			if badSecond {
				if badLabel == "" {
					badLabel = "invalid"
				}
				items = append(items, map[string]any{"Id": "bad", "Names": []string{"/broken"}, "State": "running", "Labels": map[string]string{windowsPortsLabel: badLabel, "com.docker.compose.project": "other"}})
			}
			_ = json.NewEncoder(w).Encode(items)
		} else {
			fmt.Fprint(w, "[]")
		}
	case "/containers/abc/json", "/containers/bad/json":
		if blockInspect {
			select {
			case d.inspectStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"NetworkSettings": map[string]any{"Networks": map[string]any{"demo_default": map[string]any{"NetworkID": "nat-id", "IPAddress": ip}}}})
	case "/containers/abc/start", "/containers/bad/start", "/containers/broken/start":
		w.WriteHeader(http.StatusNoContent)
	case "/networks/nat-id":
		fmt.Fprint(w, `{"Id":"nat-id","Driver":"nat"}`)
	case "/events":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		ch := make(chan string, 8)
		select {
		case d.streams <- ch:
		case <-r.Context().Done():
			return
		}
		for {
			select {
			case event := <-ch:
				if event == "disconnect" {
					return
				}
				fmt.Fprintln(w, event)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	default:
		http.Error(w, "unexpected "+path, 500)
	}
}
func (d *fakeWindowsDaemon) set(running, badList bool, ip, label string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running, d.badList, d.ip, d.label = running, badList, ip, label
}
func portLabel(t *testing.T, ports ...api.Port) string {
	t.Helper()
	raw, err := json.Marshal(ports)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestWindowsReconcileInventoryAndFailure(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, api.Port{HostIP: "127.0.0.1", Target: 53, Protocol: "udp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 2)}
	d.set(true, false, "10.11.12.13", label)
	b := windowsTestBackend(t, d.serve)
	if err := b.reconcileWindowsPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	ports := b.forwardedPorts("abc")
	if len(ports) != 2 || ports[0].Published == 0 || ports[1].Published == 0 {
		t.Fatalf("ports %#v", ports)
	}
	if err := b.reconcileWindowsPorts(context.Background()); err != nil || !reflect.DeepEqual(ports, b.forwardedPorts("abc")) {
		t.Fatalf("unstable reconciliation: %v", err)
	}
	d.mu.Lock()
	d.badSecond = true
	d.mu.Unlock()
	if err := b.reconcileWindowsPorts(context.Background()); err == nil || !reflect.DeepEqual(ports, b.forwardedPorts("abc")) {
		t.Fatalf("another container disrupted healthy listeners: %v", err)
	}
	d.mu.Lock()
	d.badSecond = false
	d.mu.Unlock()
	rows, err := call(t, b, "container.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	containers := rows.([]api.Container)
	if len(containers) != 1 || !reflect.DeepEqual(containers[0].Ports, ports) {
		t.Fatalf("inventory %#v", containers)
	}
	d.set(false, false, "", label)
	if err := b.reconcileWindowsPorts(context.Background()); err != nil || len(b.forwardedPorts("abc")) != 0 {
		t.Fatalf("stopped reconciliation: %v", err)
	}
	d.set(true, false, "127.0.0.1", label)
	if err := b.reconcileWindowsPorts(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid NAT address") {
		t.Fatalf("bad NAT endpoint accepted: %v", err)
	}
	d.set(true, false, "10.11.12.13", label)
	if err := b.reconcileWindowsPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.set(true, true, "", label)
	if err := b.reconcileWindowsPorts(context.Background()); err == nil || len(b.forwardedPorts("abc")) != 0 {
		t.Fatalf("offline cleanup: %v", err)
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	conflict := portLabel(t, api.Port{HostIP: "127.0.0.1", Published: uint16(occupied.Addr().(*net.TCPAddr).Port), Target: 80, Protocol: "tcp"})
	d.set(true, false, "10.11.12.13", conflict)
	if err := b.reconcileWindowsPorts(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot bind") {
		t.Fatalf("port conflict accepted: %v", err)
	}
}

func TestWindowsBackendCloseReleasesListener(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 1)}
	d.set(true, false, "10.11.12.13", label)
	b := windowsTestBackend(t, d.serve)
	if err := b.reconcileWindowsPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	ports := b.forwardedPorts("abc")
	if len(ports) != 1 {
		t.Fatalf("ports %#v", ports)
	}
	address := net.JoinHostPort(ports[0].HostIP, strconv.Itoa(int(ports[0].Published)))
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("Backend.Close did not release listener: %v", err)
	}
	listener.Close()
}

func TestWindowsEventsReconcileAndReconnect(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 4)}
	d.set(true, false, "10.11.12.13", label)
	b := windowsTestBackend(t, d.serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	}()
	var first chan string
	select {
	case first = <-d.streams:
	case <-time.After(5 * time.Second):
		t.Fatal("no event stream")
	}
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 1 })
	d.set(false, false, "", label)
	first <- `{"Type":"container","Action":"stop","Actor":{"ID":"abc"}}`
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 0 })
	d.set(true, false, "10.11.12.13", label)
	first <- `{"Type":"container","Action":"start","Actor":{"ID":"abc"}}`
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 1 })
	first <- "disconnect"
	var second chan string
	select {
	case second = <-d.streams:
	case <-time.After(6 * time.Second):
		t.Fatal("no event reconnect")
	}
	_ = second
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 1 })
	cancel()
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 0 })
}

func TestWindowsEventsRetryFailedBindingWithoutDisconnect(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 4)}
	d.set(true, false, "127.0.0.1", label)
	b := windowsTestBackend(t, d.serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	}()
	select {
	case <-d.streams:
	case <-time.After(5 * time.Second):
		t.Fatal("no event stream")
	}
	waitFor(t, func() bool {
		for _, event := range b.events.Since(0) {
			if event.Type == "engine.port_error" {
				return true
			}
		}
		return false
	})
	d.set(true, false, "10.11.12.13", label)
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 1 })
	waitFor(t, func() bool {
		for _, event := range b.events.Since(0) {
			if event.Type == "engine.ports_reconciled" {
				return true
			}
		}
		return false
	})
	select {
	case <-d.streams:
		t.Fatal("binding retry unnecessarily disconnected the event stream")
	default:
	}
}

type stopCompose struct{ composeapi.Compose }

func (c stopCompose) Stop(context.Context, string, composeapi.StopOptions) error { return nil }

func TestWindowsActionScopesForwardingErrors(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 1)}
	d.set(true, false, "10.11.12.13", label)
	b := windowsTestBackend(t, d.serve)
	yaml := "services:\n  web:\n    image: example\n"
	parts := make(map[string]api.ProjectPart)
	for _, name := range []string{"demo", "other"} {
		value, err := call(t, b, "compose.import", api.ComposeParams{Project: api.ProjectPart{Engine: "windows", Name: name}, YAML: yaml})
		if err != nil {
			t.Fatal(err)
		}
		parts[name] = value.(api.ProjectPart)
	}
	b.compose = stopCompose{Compose: b.compose}
	if err := b.reconcileWindowsPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	previous := b.forwardedPorts("abc")
	d.mu.Lock()
	d.badSecond = true
	d.mu.Unlock()

	for _, method := range []struct {
		name   string
		params any
	}{
		{"container.action", api.ContainerParams{ID: "abc", Action: "start"}},
		{"compose.action", api.ComposeParams{Project: parts["demo"], Action: "stop"}},
	} {
		if _, err := call(t, b, method.name, method.params); err != nil {
			t.Fatalf("unrelated bad metadata broke %s: %v", method.name, err)
		}
	}
	if !reflect.DeepEqual(previous, b.forwardedPorts("abc")) {
		t.Fatal("unrelated failure disturbed established listener")
	}
	if _, err := call(t, b, "container.action", api.ContainerParams{ID: "broken", Action: "start"}); err == nil || !strings.Contains(err.Error(), "invalid UCD published port metadata") {
		t.Fatalf("target name did not report its forwarding error: %v", err)
	}
	if _, err := call(t, b, "compose.action", api.ComposeParams{Project: parts["other"], Action: "stop"}); err == nil || !strings.Contains(err.Error(), "invalid UCD published port metadata") {
		t.Fatalf("target project did not report its forwarding error: %v", err)
	}
	var sawError bool
	for _, event := range b.events.Since(0) {
		if event.Type == "engine.port_error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("unrelated failure was not reported to engine diagnostics")
	}
	select {
	case <-b.portReconcile:
	default:
		t.Fatal("unrelated failure did not schedule event-loop retry")
	}

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	conflict := portLabel(t, api.Port{HostIP: "127.0.0.1", Published: uint16(occupied.Addr().(*net.TCPAddr).Port), Target: 80, Protocol: "tcp"})
	d.mu.Lock()
	d.badLabel = conflict
	d.mu.Unlock()
	if _, err := call(t, b, "container.action", api.ContainerParams{ID: "abc", Action: "start"}); err != nil {
		t.Fatalf("unrelated binding conflict broke action: %v", err)
	}
	if _, err := call(t, b, "container.action", api.ContainerParams{ID: "bad", Action: "start"}); err == nil || !strings.Contains(err.Error(), "cannot bind") {
		t.Fatalf("target binding conflict not reported: %v", err)
	}
}

func TestWindowsCanceledActionPreservesListeners(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 1), inspectStarted: make(chan struct{}, 1)}
	d.set(true, false, "10.11.12.13", label)
	b := windowsTestBackend(t, d.serve)
	if err := b.reconcileWindowsPorts(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := b.forwardedPorts("abc")
	if len(original) != 1 {
		t.Fatalf("initial listener %#v", original)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.reconcileWindowsPorts(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled snapshot: %v", err)
	}
	if !reflect.DeepEqual(original, b.forwardedPorts("abc")) {
		t.Fatal("pre-canceled snapshot closed healthy listener")
	}

	d.mu.Lock()
	d.blockInspect = true
	d.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := api.NewRequest("container.action", "windows", api.ContainerParams{ID: "abc", Action: "start"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := b.Call(ctx, request); done <- err }()
	select {
	case <-d.inspectStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot did not reach inspect")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled action: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled action did not return")
	}
	if !reflect.DeepEqual(original, b.forwardedPorts("abc")) {
		t.Fatal("partially collected canceled snapshot closed healthy listener")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(original[0].Published)))
	if listener, err := net.Listen("tcp", address); err == nil {
		listener.Close()
		t.Fatal("healthy listener was released")
	}
	select {
	case <-b.portReconcile:
	default:
		t.Fatal("canceled action did not schedule service-owned retry")
	}
}

func TestWindowsCanceledActionRetriesWithServiceContext(t *testing.T) {
	label := portLabel(t, api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"})
	d := &fakeWindowsDaemon{streams: make(chan chan string, 1)}
	d.set(true, false, "10.11.12.13", label)
	b := windowsTestBackend(t, d.serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	}()
	select {
	case <-d.streams:
	case <-time.After(5 * time.Second):
		t.Fatal("no event stream")
	}
	waitFor(t, func() bool { return len(b.forwardedPorts("abc")) == 1 })
	d.set(true, false, "10.11.12.14", label)
	canceled, cancelAction := context.WithCancel(context.Background())
	cancelAction()
	if err := b.reconcileActionPorts(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled action reconciliation: %v", err)
	}
	waitFor(t, func() bool {
		b.portMu.Lock()
		defer b.portMu.Unlock()
		forward := b.forwards[forwardKey{container: "abc", index: 0}]
		return forward != nil && forward.target == "10.11.12.14:80"
	})
}
