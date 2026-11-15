package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

const linuxPortTestID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPrepareLinuxContainerPortsKeepsRequestsAndUsesOnePrivateRelay(t *testing.T) {
	p := api.ContainerCreateParams{
		Config: &container.Config{
			Image:        "example",
			Labels:       map[string]string{"custom": "kept"},
			ExposedPorts: network.PortSet{network.MustParsePort("9000/tcp"): {}},
		},
		HostConfig: &container.HostConfig{
			DNS:        []netip.Addr{netip.MustParseAddr("192.0.2.53")},
			ExtraHosts: []string{"host.docker.internal:192.0.2.10"},
			PortBindings: network.PortMap{
				network.MustParsePort("80/tcp"): {
					{HostPort: ""},
					{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"},
				},
				network.MustParsePort("443/tcp"): {{HostIP: netip.MustParseAddr("::1"), HostPort: "8443"}},
				network.MustParsePort("53/udp"):  {{HostPort: ""}},
			},
		},
	}
	b := &Backend{id: "linux", runtimeConfig: api.EngineConfiguration{Network: api.DefaultNetworkSettings()}}
	if err := b.prepareContainerPorts(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	requested, err := parseLinuxPortMetadata(p.Config.Labels[linuxPortsLabel])
	if err != nil {
		t.Fatal(err)
	}
	want := []api.Port{
		{HostIP: "127.0.0.1", Target: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", Published: 8080, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"},
		{HostIP: "::1", Published: 8443, Target: 443, Protocol: "tcp"},
	}
	if !reflect.DeepEqual(requested, want) {
		t.Fatalf("requested metadata = %#v, want %#v", requested, want)
	}
	if len(p.HostConfig.PortBindings) != 3 || len(p.Config.ExposedPorts) != 4 || p.Config.Labels["custom"] != "kept" {
		t.Fatalf("container create was not preserved: %#v %#v", p.Config, p.HostConfig.PortBindings)
	}
	for _, target := range []string{"53/udp", "80/tcp", "443/tcp"} {
		bindings := p.HostConfig.PortBindings[network.MustParsePort(target)]
		if len(bindings) != 2 || bindings[0].HostIP != linuxRelayIP || bindings[0].HostPort != "0" ||
			bindings[1].HostIP != linuxRelayIPv6 || bindings[1].HostPort != "0" {
			t.Errorf("private binding %s = %#v", target, bindings)
		}
	}
	if !reflect.DeepEqual(p.HostConfig.DNS, []netip.Addr{netip.MustParseAddr("192.0.2.53")}) ||
		!reflect.DeepEqual(p.HostConfig.ExtraHosts, []string{"host.docker.internal:192.0.2.10"}) {
		t.Fatalf("explicit DNS or extra hosts changed: %#v", p.HostConfig)
	}
}

func TestPrepareLinuxContainerPortsLeavesNativeNetworkModesAlone(t *testing.T) {
	for _, mode := range []string{"host", "none", "container:peer"} {
		t.Run(mode, func(t *testing.T) {
			p := api.ContainerCreateParams{
				Config: &container.Config{Image: "example"},
				HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(mode), PortBindings: network.PortMap{
					network.MustParsePort("80/tcp"): {{HostIP: linuxRelayIP, HostPort: "8080"}},
				}},
			}
			b := &Backend{id: "linux"}
			if err := b.prepareContainerPorts(context.Background(), &p); err != nil {
				t.Fatal(err)
			}
			if p.HostConfig.PortBindings[network.MustParsePort("80/tcp")][0].HostPort != "8080" ||
				p.Config.Labels[linuxPortsLabel] != "" {
				t.Fatalf("native network mode changed: %#v %#v", p.HostConfig, p.Config.Labels)
			}
		})
	}
}

func TestComposeContainerCreateRemapsLinuxPublishedPorts(t *testing.T) {
	want := []api.Port{
		{HostIP: "127.0.0.1", Target: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"},
	}
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/containers/create":
			var body struct {
				Labels     map[string]string
				HostConfig struct {
					PortBindings map[string][]struct{ HostIP, HostPort string }
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			requested, err := parseLinuxPortMetadata(body.Labels[linuxPortsLabel])
			if err != nil || !reflect.DeepEqual(requested, want) {
				t.Errorf("Compose request metadata = %#v (%v), want %#v", requested, err, want)
			}
			for _, key := range []string{"80/tcp", "53/udp"} {
				bindings := body.HostConfig.PortBindings[key]
				if len(bindings) != 2 || bindings[0].HostIP != "127.0.0.1" || bindings[0].HostPort != "0" ||
					bindings[1].HostIP != "::1" || bindings[1].HostPort != "0" {
					t.Errorf("Compose private binding %s = %#v", key, bindings)
				}
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"compose-container"}`)
		default:
			t.Errorf("unexpected Docker request %s", r.URL)
			http.NotFound(w, r)
		}
	})
	composeClient := &composeDefaultsClient{APIClient: b.docker, backend: b}
	result, err := composeClient.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Config: &container.Config{Image: "example"},
		HostConfig: &container.HostConfig{PortBindings: network.PortMap{
			network.MustParsePort("80/tcp"): {
				{HostPort: ""},
				{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: ""},
			},
			network.MustParsePort("53/udp"): {{HostPort: ""}},
		}},
	})
	if err != nil || result.ID != "compose-container" {
		t.Fatalf("Compose container create = %#v, %v", result, err)
	}
}

func TestLinuxPortBindingsAndProjectionPreserveDynamicRequests(t *testing.T) {
	requested := []api.Port{
		{HostIP: "127.0.0.1", Published: 5353, Target: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"},
	}
	label := linuxPortLabelForTest(t, requested)
	list := map[string]any{
		"Id": linuxPortTestID, "Names": []string{"/web"}, "State": "paused",
		"Status": "Up 2 minutes (Paused)", "Labels": map[string]string{linuxPortsLabel: label},
		"Ports": []map[string]any{
			{"IP": "127.0.0.1", "PrivatePort": 53, "PublicPort": 49153, "Type": "udp"},
			{"IP": "127.0.0.1", "PrivatePort": 80, "PublicPort": 49152, "Type": "tcp"},
			{"IP": "::1", "PrivatePort": 80, "PublicPort": 49154, "Type": "tcp"},
			{"IP": "127.0.0.1", "PrivatePort": 9999, "PublicPort": 9999, "Type": "tcp"},
		},
		"future": "keep",
	}
	inspect := map[string]any{
		"Id": linuxPortTestID, "State": map[string]any{"Running": true, "Paused": true},
		"Config": map[string]any{"Labels": map[string]string{linuxPortsLabel: label}},
		"HostConfig": map[string]any{"PortBindings": map[string]any{
			"53/udp":   []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49153"}},
			"80/tcp":   []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49152"}},
			"9999/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "9999"}},
		}, "futureHost": "keep"},
		"NetworkSettings": map[string]any{"Ports": map[string]any{
			"53/udp":   []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49153"}},
			"80/tcp":   []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49152"}, {"HostIp": "::1", "HostPort": "49154"}},
			"9999/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "9999"}},
		}, "futureNetwork": "keep"},
		"futureTop": map[string]string{"field": "keep"},
	}
	listBody, _ := json.Marshal([]any{list})
	inspectBody, _ := json.Marshal(inspect)
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/containers/json":
			_, _ = w.Write(listBody)
		case "/containers/" + linuxPortTestID + "/json":
			_, _ = w.Write(inspectBody)
		default:
			t.Errorf("unexpected Docker request %s", r.URL)
			http.NotFound(w, r)
		}
	})

	result, err := call(t, b, "engine.internal.portbindings", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := result.(api.LinuxPortBindings)
	if len(snapshot.Bindings) != 3 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	for index, binding := range snapshot.Bindings {
		if binding.Container != linuxPortTestID || binding.Index != index || binding.Port != requested[index] {
			t.Fatalf("binding %d = %#v", index, binding)
		}
		wantRelay := uint16(49152)
		if binding.Port.Target == 53 {
			wantRelay = 49153
		}
		if binding.RelayPort != wantRelay {
			t.Fatalf("binding %d relay = %d, want %d", index, binding.RelayPort, wantRelay)
		}
	}

	active := append([]api.LinuxPortBinding(nil), snapshot.Bindings...)
	active[0].Port.Published = 5353
	active[1].Port.Published = 45000
	active[2].Port.Published = 45001
	if _, err := call(t, b, "engine.internal.portstate", api.LinuxPortBindings{Bindings: active}); err != nil {
		t.Fatal(err)
	}
	bad := append([]api.LinuxPortBinding(nil), active...)
	bad[1].RelayPort++
	if _, err := call(t, b, "engine.internal.portstate", api.LinuxPortBindings{Bindings: bad}); err == nil {
		t.Fatal("accepted changed relay port")
	}
	bad = append([]api.LinuxPortBinding(nil), active...)
	bad[0].Port.Published++
	if _, err := call(t, b, "engine.internal.portstate", api.LinuxPortBindings{Bindings: bad}); err == nil {
		t.Fatal("accepted a public port that differs from the fixed request")
	}
	result, err = call(t, b, "engine.internal.portbindings", nil)
	if err != nil || !reflect.DeepEqual(result.(api.LinuxPortBindings).Bindings, snapshot.Bindings) {
		t.Fatalf("requested snapshot changed by active state: %#v, %v", result, err)
	}

	rows, err := call(t, b, "container.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPublic := []api.Port{
		{HostIP: "127.0.0.1", Published: 5353, Target: 53, Protocol: "udp"},
		{HostIP: "0.0.0.0", Published: 45000, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Published: 45001, Target: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", Published: 9999, Target: 9999, Protocol: "tcp"},
	}
	gotPublic := rows.([]api.Container)[0].Ports
	sort.Slice(gotPublic, func(i, j int) bool { return linuxPortLess(gotPublic[i], gotPublic[j]) })
	sort.Slice(wantPublic, func(i, j int) bool { return linuxPortLess(wantPublic[i], wantPublic[j]) })
	if !reflect.DeepEqual(gotPublic, wantPublic) {
		t.Fatalf("container list leaked relay or lost public mappings: %#v", gotPublic)
	}

	result, err = call(t, b, "container.inspect", api.ContainerParams{ID: linuxPortTestID})
	if err != nil {
		t.Fatal(err)
	}
	inspection := result.(container.InspectResponse)
	if got := inspection.HostConfig.PortBindings[network.MustParsePort("80/tcp")]; len(got) != 2 ||
		got[0].HostPort != "" || got[1].HostPort != "" {
		t.Fatalf("inspect requested TCP bindings = %#v", got)
	}
	if got := inspection.NetworkSettings.Ports[network.MustParsePort("80/tcp")]; len(got) != 2 ||
		got[0].HostPort != "45000" || got[1].HostPort != "45001" {
		t.Fatalf("inspect active TCP bindings = %#v", got)
	}
	if got := inspection.NetworkSettings.Ports[network.MustParsePort("53/udp")]; len(got) != 1 || got[0].HostPort != "5353" {
		t.Fatalf("inspect active UDP binding = %#v", got)
	}
	if got := inspection.NetworkSettings.Ports[network.MustParsePort("9999/tcp")]; len(got) != 1 || got[0].HostPort != "9999" {
		t.Fatalf("inspect unrelated mapping changed: %#v", got)
	}

	display := b.DockerPolicyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/containers/json" {
			_, _ = w.Write(listBody)
			return
		}
		w.Header().Set("X-Docker-Header", "keep")
		_, _ = w.Write(inspectBody)
	}))
	for _, path := range []string{"/containers/json", "/containers/web/json"} {
		response := httptest.NewRecorder()
		display.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("raw Docker display %s = %d %v", path, response.Code, response.Header())
		}
		if path == "/containers/json" {
			var entries []struct {
				Ports []struct {
					IP                      string
					PrivatePort, PublicPort uint16
					Type                    string
				}
				Future string
			}
			if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Future != "keep" || len(entries[0].Ports) != 4 {
				t.Fatalf("raw Docker list projection = %s", response.Body.String())
			}
			for _, port := range entries[0].Ports {
				if port.PublicPort == 49152 || port.PublicPort == 49153 {
					t.Fatalf("raw list leaked private relay: %#v", port)
				}
			}
		} else {
			if response.Header().Get("X-Docker-Header") != "keep" {
				t.Fatal("raw Docker inspect response headers were not preserved")
			}
			var got struct {
				HostConfig struct {
					PortBindings map[string][]struct{ HostIP, HostPort string }
				}
				NetworkSettings struct {
					Ports         map[string][]struct{ HostIP, HostPort string }
					FutureNetwork string
				}
				FutureTop map[string]string
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.HostConfig.PortBindings["80/tcp"]) != 2 ||
				got.HostConfig.PortBindings["80/tcp"][0].HostPort != "" ||
				got.HostConfig.PortBindings["80/tcp"][1].HostPort != "" {
				t.Fatalf("raw Docker requested bindings = %#v", got.HostConfig.PortBindings)
			}
			if got.NetworkSettings.Ports["80/tcp"][0].HostPort != "45000" ||
				got.NetworkSettings.Ports["80/tcp"][1].HostPort != "45001" ||
				got.NetworkSettings.FutureNetwork != "keep" || got.FutureTop["field"] != "keep" {
				t.Fatalf("raw Docker active projection or unknown fields = %#v", got)
			}
		}
	}

	result, err = call(t, b, "engine.internal.portbindings", nil)
	if err != nil || len(result.(api.LinuxPortBindings).Bindings) != 3 {
		t.Fatalf("paused container bindings were dropped: %#v, %v", result, err)
	}

	list["Ports"] = []map[string]any{
		{"IP": "127.0.0.1", "PrivatePort": 53, "PublicPort": 49253, "Type": "udp"},
		{"IP": "127.0.0.1", "PrivatePort": 80, "PublicPort": 49252, "Type": "tcp"},
	}
	listBody, _ = json.Marshal([]any{list})
	result, err = call(t, b, "engine.internal.portbindings", nil)
	if err != nil || len(result.(api.LinuxPortBindings).Bindings) != 3 {
		t.Fatalf("restart snapshot failed: %#v, %v", result, err)
	}
	b.linuxPortsMu.RLock()
	activeCount := len(b.linuxPortState)
	b.linuxPortsMu.RUnlock()
	if activeCount != 0 {
		t.Fatalf("old relay state survived a changed private relay port: %d", activeCount)
	}
	list["State"] = "exited"
	listBody, _ = json.Marshal([]any{list})
	result, err = call(t, b, "engine.internal.portbindings", nil)
	if err != nil || len(result.(api.LinuxPortBindings).Bindings) != 0 {
		t.Fatalf("stopped container retained port bindings: %#v, %v", result, err)
	}
}

func linuxPortLabelForTest(t *testing.T, ports []api.Port) string {
	t.Helper()
	data, err := json.Marshal(ports)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWaitLinuxPortStateReturnsAfterValidatedStateUpdate(t *testing.T) {
	want := api.LinuxPortBinding{
		Container: linuxPortTestID,
		Index:     0,
		Port:      api.Port{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"},
		RelayPort: 49152,
	}
	b := &Backend{}
	done := make(chan error, 1)
	go func() { done <- b.waitLinuxPortState(context.Background(), []api.LinuxPortBinding{want}) }()
	b.replaceLinuxPortState(map[forwardKey]api.LinuxPortBinding{
		{container: want.Container, index: want.Index}: {
			Container: want.Container, Index: want.Index,
			Port:      api.Port{HostIP: want.Port.HostIP, Published: 45000, Target: want.Port.Target, Protocol: want.Port.Protocol},
			RelayPort: want.RelayPort,
		},
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWaitLinuxContainerPortsAllowsQuickExitAndAutoRemove(t *testing.T) {
	label := linuxPortLabelForTest(t, []api.Port{{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}})
	exited, err := json.Marshal(map[string]any{
		"Id": "one-shot", "State": map[string]any{"Running": false},
		"Config": map[string]any{"Labels": map[string]string{linuxPortsLabel: label}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name: "one shot process exited",
			body: string(exited),
		},
		{
			name:   "auto removed after start",
			status: http.StatusNotFound,
			body:   `{"message":"No such container: one-shot"}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if dockerTestPath(w, r) == "" {
					return
				}
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = io.WriteString(w, test.body)
			})
			if err := b.waitLinuxContainerPorts(context.Background(), "one-shot"); err != nil {
				t.Fatalf("quick exit should preserve Docker start success: %v", err)
			}
		})
	}
}

func TestLinuxIPv6OnlyRelayForwardsTCPAndUDPToIPv4Loopback(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			targetPort, exchange := startIPv6Echo(t, protocol)
			backend := &Backend{}
			defer backend.closeLinuxPortRelays()
			requested := []api.Port{{HostIP: "127.0.0.1", Target: 8080, Protocol: protocol}}
			docker := newLinuxDockerRelayPorts(1)
			docker.ipv6[linuxPortKey(8080, protocol)] = targetPort
			relay, err := backend.hostRelayPorts("ipv6-only", requested, docker)
			if err != nil {
				t.Fatal(err)
			}
			relayPort := relay[linuxPortKey(8080, protocol)]
			if relayPort == 0 || relayPort == targetPort {
				t.Fatalf("IPv4 shim port = %d, Docker IPv6 port = %d", relayPort, targetPort)
			}

			reused, err := backend.hostRelayPorts("ipv6-only", requested, docker)
			if err != nil || reused[linuxPortKey(8080, protocol)] != relayPort {
				t.Fatalf("relay was not reused: %#v, %v", reused, err)
			}
			if err := exchange(relayPort); err != nil {
				t.Fatal(err)
			}

			backend.closeLinuxPortRelays()
			if protocol == "tcp" {
				listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(relayPort))))
				if err != nil {
					t.Fatalf("TCP relay listener was not released: %v", err)
				}
				_ = listener.Close()
			} else {
				listener, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(relayPort))))
				if err != nil {
					t.Fatalf("UDP relay listener was not released: %v", err)
				}
				_ = listener.Close()
			}
		})
	}
}

func TestLinuxFamilyRelayPrefersIPv4AndReplacesOrCleansIPv6Shim(t *testing.T) {
	backend := &Backend{}
	defer backend.closeLinuxPortRelays()
	requested := []api.Port{{HostIP: "127.0.0.1", Target: 8080, Protocol: "tcp"}}
	key := linuxPortKey(8080, "tcp")
	docker := newLinuxDockerRelayPorts(1)
	docker.ipv6[key] = 50001

	first, err := backend.hostRelayPorts("container-a", requested, docker)
	if err != nil {
		t.Fatal(err)
	}
	firstPort := first[key]
	reused, err := backend.hostRelayPorts("container-a", requested, docker)
	if err != nil || reused[key] != firstPort {
		t.Fatalf("unchanged IPv6 shim was not reused: %#v, %v", reused, err)
	}

	docker.ipv6[key] = 50002
	replaced, err := backend.hostRelayPorts("container-a", requested, docker)
	if err != nil {
		t.Fatal(err)
	}
	if replaced[key] == firstPort {
		t.Fatalf("changed Docker relay kept old IPv4 shim port %d", firstPort)
	}
	if listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(firstPort)))); err != nil {
		t.Fatalf("replaced IPv4 shim listener was not released: %v", err)
	} else {
		_ = listener.Close()
	}

	newPort := replaced[key]
	docker.ipv4[key] = 50003
	preferred, err := backend.hostRelayPorts("container-a", requested, docker)
	if err != nil || preferred[key] != 50003 {
		t.Fatalf("IPv4 Docker relay was not preferred: %#v, %v", preferred, err)
	}
	if listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(newPort)))); err != nil {
		t.Fatalf("IPv4 shim remained after direct IPv4 relay became available: %v", err)
	} else {
		_ = listener.Close()
	}

	docker.ipv4 = nil
	_, err = backend.hostRelayPorts("container-a", requested, docker)
	if err != nil {
		t.Fatal(err)
	}
	backend.pruneLinuxFamilyRelays(nil)
	backend.linuxPortsMu.RLock()
	remaining := len(backend.linuxRelayPorts)
	backend.linuxPortsMu.RUnlock()
	if remaining != 0 {
		t.Fatalf("stopped-container relay cleanup left %d shims", remaining)
	}
}

func TestLinuxIPv6OnlyPortSnapshotReadinessAndProjectionUseIPv4Shim(t *testing.T) {
	requested := []api.Port{{HostIP: "127.0.0.1", Target: 80, Protocol: "tcp"}}
	label := linuxPortLabelForTest(t, requested)
	list := map[string]any{
		"Id":     linuxPortTestID,
		"Names":  []string{"/v6-web"},
		"State":  "running",
		"Labels": map[string]string{linuxPortsLabel: label},
		"Ports":  []map[string]any{{"IP": "::1", "PrivatePort": 80, "PublicPort": 49152, "Type": "tcp"}},
	}
	inspect := map[string]any{
		"Id":     linuxPortTestID,
		"State":  map[string]any{"Running": true},
		"Config": map[string]any{"Labels": map[string]string{linuxPortsLabel: label}},
		"HostConfig": map[string]any{"PortBindings": map[string]any{
			"80/tcp": []map[string]string{{"HostIp": "::1", "HostPort": "49152"}},
		}},
		"NetworkSettings": map[string]any{"Ports": map[string]any{
			"80/tcp": []map[string]string{{"HostIp": "::1", "HostPort": "49152"}},
		}},
	}
	listBody, _ := json.Marshal([]any{list})
	inspectBody, _ := json.Marshal(inspect)
	backend := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		switch dockerTestPath(w, r) {
		case "":
		case "/containers/json":
			_, _ = w.Write(listBody)
		case "/containers/" + linuxPortTestID + "/json":
			_, _ = w.Write(inspectBody)
		default:
			t.Errorf("unexpected Docker request %s", r.URL)
			http.NotFound(w, r)
		}
	})
	defer backend.closeLinuxPortRelays()

	result, err := call(t, backend, "engine.internal.portbindings", nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := result.(api.LinuxPortBindings)
	if len(snapshot.Bindings) != 1 || snapshot.Bindings[0].RelayPort == 0 || snapshot.Bindings[0].RelayPort == 49152 {
		t.Fatalf("controller snapshot did not expose the IPv4 shim: %#v", snapshot.Bindings)
	}
	active := snapshot.Bindings
	active[0].Port.Published = 44000
	if _, err := call(t, backend, "engine.internal.portstate", api.LinuxPortBindings{Bindings: active}); err != nil {
		t.Fatal(err)
	}
	if err := backend.waitLinuxContainerPorts(context.Background(), linuxPortTestID); err != nil {
		t.Fatalf("start readiness did not use the IPv4 shim: %v", err)
	}

	result, err = call(t, backend, "container.inspect", api.ContainerParams{ID: linuxPortTestID})
	if err != nil {
		t.Fatal(err)
	}
	inspection := result.(container.InspectResponse)
	publicPort := inspection.NetworkSettings.Ports[network.MustParsePort("80/tcp")]
	if len(publicPort) != 1 || publicPort[0].HostIP != linuxRelayIP || publicPort[0].HostPort != "44000" {
		t.Fatalf("typed inspect leaked the Docker IPv6 relay: %#v", publicPort)
	}
	rows, err := call(t, backend, "container.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	ports := rows.([]api.Container)[0].Ports
	if len(ports) != 1 || ports[0].HostIP != linuxRelayIP.String() || ports[0].Published != 44000 {
		t.Fatalf("container list leaked the Docker IPv6 relay: %#v", ports)
	}
}

func startIPv6Echo(t *testing.T, protocol string) (uint16, func(uint16) error) {
	t.Helper()
	if protocol == "tcp" {
		listener, err := net.Listen("tcp6", net.JoinHostPort("::1", "0"))
		if err != nil {
			t.Skipf("IPv6 loopback is unavailable: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		server := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				server <- err
				return
			}
			defer conn.Close()
			buffer := make([]byte, 4)
			if _, err := io.ReadFull(conn, buffer); err != nil {
				server <- err
				return
			}
			_, err = conn.Write([]byte("pong"))
			server <- err
		}()
		return port, func(relayPort uint16) error {
			conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(relayPort))), time.Second)
			if err != nil {
				return err
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Write([]byte("ping")); err != nil {
				return err
			}
			buffer := make([]byte, 4)
			if _, err := io.ReadFull(conn, buffer); err != nil {
				return err
			}
			if string(buffer) != "pong" {
				return fmt.Errorf("TCP IPv6 relay response = %q", buffer)
			}
			return <-server
		}
	}
	listener, err := net.ListenPacket("udp6", net.JoinHostPort("::1", "0"))
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := uint16(listener.LocalAddr().(*net.UDPAddr).Port)
	server := make(chan error, 1)
	go func() {
		buffer := make([]byte, 16)
		n, from, err := listener.ReadFrom(buffer)
		if err == nil && string(buffer[:n]) == "ping" {
			_, err = listener.WriteTo([]byte("pong"), from)
		}
		server <- err
	}()
	return port, func(relayPort uint16) error {
		client, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		target := &net.UDPAddr{IP: netip.MustParseAddr("127.0.0.1").AsSlice(), Port: int(relayPort)}
		if _, err := client.WriteTo([]byte("ping"), target); err != nil {
			return err
		}
		buffer := make([]byte, 16)
		n, _, err := client.ReadFrom(buffer)
		if err != nil {
			return err
		}
		if string(buffer[:n]) != "pong" {
			return fmt.Errorf("UDP IPv6 relay response = %q", buffer[:n])
		}
		return <-server
	}
}
