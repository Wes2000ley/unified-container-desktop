//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

func TestLinuxHostPortsForwardBothProtocolsAndRemoveListeners(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "linux-container") }))
	defer server.Close()
	relay := uint16(server.Listener.Addr().(*net.TCPAddr).Port)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 1024)
		for {
			n, remote, err := udp.ReadFromUDP(b)
			if err != nil {
				return
			}
			_, _ = udp.WriteToUDP(b[:n], remote)
		}
	}()
	defer func() { udp.Close(); <-done }()
	desired := api.LinuxPortBindings{Bindings: []api.LinuxPortBinding{
		{Container: "fixture", Index: 0, Port: api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, RelayPort: relay},
		{Container: "fixture", Index: 1, Port: api.Port{HostIP: "127.0.0.1", Target: 53, Protocol: "udp"}, RelayPort: uint16(udp.LocalAddr().(*net.UDPAddr).Port)},
	}}
	var published api.LinuxPortBindings
	ports := linuxHostPorts{helper: aliasCaller(func(_ context.Context, r api.Request) (any, error) {
		if r.Method == "engine.internal.portbindings" {
			data, _ := json.Marshal(desired)
			return json.RawMessage(data), nil
		}
		var err error
		published, err = api.Decode[api.LinuxPortBindings](r)
		return nil, err
	})}
	defer ports.close()
	if err := ports.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(published.Bindings) != 2 {
		t.Fatalf("public state: %+v", published)
	}
	first := published.Bindings[0].Port.Published
	if first == 0 || published.Bindings[1].Port.Published == 0 {
		t.Fatal("dynamic public ports not reported")
	}
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(int(first)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "linux-container" {
		t.Fatalf("TCP relay=%q %v", body, err)
	}
	connection, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(published.Bindings[1].Port.Published)})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	connection.Write([]byte("udp-proof"))
	buffer := make([]byte, 1024)
	n, err := connection.Read(buffer)
	if err != nil || string(buffer[:n]) != "udp-proof" {
		t.Fatalf("UDP relay=%q %v", buffer[:n], err)
	}
	if err := ports.reconcile(context.Background()); err != nil || published.Bindings[0].Port.Published != first {
		t.Fatalf("stable snapshot changed listener: %+v %v", published, err)
	}
	desired.Bindings = nil
	if err := ports.reconcile(context.Background()); err != nil || len(published.Bindings) != 0 || len(ports.forwards) != 0 {
		t.Fatalf("removed endpoints: %+v %v", published, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(first))))
	if err != nil {
		t.Fatalf("removed public listener still bound: %v", err)
	}
	listener.Close()
}

func TestLinuxHostPortConflictIsVisibleAndUnverifiedEndpointsClose(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	desired := api.LinuxPortBindings{Bindings: []api.LinuxPortBinding{{Container: "conflict", Index: 0, Port: api.Port{HostIP: "127.0.0.1", Published: uint16(occupied.Addr().(*net.TCPAddr).Port), Target: 80, Protocol: "tcp"}, RelayPort: 7443}, {Container: "healthy", Index: 0, Port: api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, RelayPort: 7443}}}
	var published api.LinuxPortBindings
	var unavailable bool
	ports := linuxHostPorts{helper: aliasCaller(func(_ context.Context, r api.Request) (any, error) {
		if r.Method == "engine.internal.portbindings" {
			if unavailable {
				return nil, errors.New("helper disconnected")
			}
			return desired, nil
		}
		var err error
		published, err = api.Decode[api.LinuxPortBindings](r)
		return nil, err
	})}
	defer ports.close()
	err = ports.reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "container conflict cannot bind") || len(published.Bindings) != 1 || published.Bindings[0].Container != "healthy" {
		t.Fatalf("conflict hidden or healthy binding lost: %+v %v", published, err)
	}
	public := published.Bindings[0].Port.Published
	unavailable = true
	if err := ports.reconcile(context.Background()); err == nil || len(ports.forwards) != 0 || len(published.Bindings) != 0 {
		t.Fatalf("unverified listeners or display state retained: %+v %v", published, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(public))))
	if err != nil {
		t.Fatalf("unverified listener still bound: %v", err)
	}
	listener.Close()
}

func TestLinuxDuplicateDynamicBindingsHaveIndependentPublicPorts(t *testing.T) {
	desired := api.LinuxPortBindings{Bindings: []api.LinuxPortBinding{
		{Container: "dual", Index: 0, Port: api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, RelayPort: 7443},
		{Container: "dual", Index: 1, Port: api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, RelayPort: 7443},
	}}
	var state api.LinuxPortBindings
	ports := linuxHostPorts{helper: aliasCaller(func(_ context.Context, r api.Request) (any, error) {
		if r.Method == "engine.internal.portbindings" {
			return desired, nil
		}
		var err error
		state, err = api.Decode[api.LinuxPortBindings](r)
		return nil, err
	})}
	defer ports.close()
	if err := ports.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(state.Bindings) != 2 || state.Bindings[0].Port.Published == 0 || state.Bindings[1].Port.Published == 0 || state.Bindings[0].Port.Published == state.Bindings[1].Port.Published {
		t.Fatalf("duplicate dynamic bindings lost their individual public ports: %+v", state)
	}
}

func TestLinuxHostPortsEventsAndShutdown(t *testing.T) {
	streamEvents := make(chan string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			t.Errorf("unexpected Docker request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-streamEvents:
				if event == "" {
					return
				}
				io.WriteString(w, event+"\n")
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer server.Close()
	docker, err := client.New(client.WithHost("tcp"+strings.TrimPrefix(server.URL, "http")), client.WithAPIVersion("1.54"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	var mu sync.Mutex
	desired := api.LinuxPortBindings{Bindings: []api.LinuxPortBinding{{Container: "events", Port: api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, RelayPort: 7443}}}
	states := make(chan api.LinuxPortBindings, 10)
	helper := aliasCaller(func(_ context.Context, r api.Request) (any, error) {
		if r.Method == "engine.internal.portbindings" {
			mu.Lock()
			defer mu.Unlock()
			return desired, nil
		}
		state, err := api.Decode[api.LinuxPortBindings](r)
		if err == nil {
			states <- state
		}
		return nil, err
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runLinuxHostPorts(ctx, helper, docker, &api.EventBus{}) }()
	defer func() { cancel(); <-done }()
	next := func(count int) api.LinuxPortBindings {
		t.Helper()
		select {
		case state := <-states:
			if len(state.Bindings) != count {
				t.Fatalf("published bindings=%+v, want %d", state, count)
			}
			return state
		case <-time.After(5 * time.Second):
			t.Fatal("port state did not follow Docker event")
			return api.LinuxPortBindings{}
		}
	}
	public := next(1).Bindings[0].Port.Published
	mu.Lock()
	desired.Bindings = nil
	mu.Unlock()
	select {
	case streamEvents <- `{"Type":"container","Action":"die"}`:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream was not subscribed")
	}
	next(0)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(public))))
	if err != nil {
		t.Fatalf("container exit left a public listener: %v", err)
	}
	listener.Close()
	mu.Lock()
	desired.Bindings = []api.LinuxPortBinding{{Container: "events", Port: api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}, RelayPort: 7443}}
	mu.Unlock()
	select {
	case streamEvents <- `{"Type":"container","Action":"start"}`:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream stopped")
	}
	public = next(1).Bindings[0].Port.Published
	select {
	case streamEvents <- "":
	case <-time.After(5 * time.Second):
		t.Fatal("could not disconnect Docker event stream")
	}
	next(0)
	public = next(1).Bindings[0].Port.Published
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream did not stop on controller shutdown")
	}
	next(0)
	listener, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(public))))
	if err != nil {
		t.Fatalf("controller shutdown left a public listener: %v", err)
	}
	listener.Close()
}

func TestLinuxHostPortsFilterUnrelatedDockerEvents(t *testing.T) {
	for _, event := range [][2]string{{"container", "start"}, {"container", "die"}, {"container", "pause"}, {"network", "connect"}} {
		if !linuxPortsEvent(event[0], event[1]) {
			t.Fatalf("missed port event %v", event)
		}
	}
	for _, event := range [][2]string{{"container", "exec_start: /bin/sh"}, {"container", "exec_die"}, {"image", "pull"}, {"volume", "create"}} {
		if linuxPortsEvent(event[0], event[1]) {
			t.Fatalf("unnecessary port snapshot for %v", event)
		}
	}
}
