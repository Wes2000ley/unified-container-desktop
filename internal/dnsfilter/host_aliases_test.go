package dnsfilter

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func hostQuery(t *testing.T, name string, kind dnsmessage.Type) []byte {
	t.Helper()
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: 321, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: kind, Class: dnsmessage.ClassINET}}}
	data, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDockerHostDNSUsesLocalAddressesAndRefreshes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostaliases.json")
	for _, address := range []string{"172.28.240.1", "172.29.16.1"} {
		if err := os.WriteFile(path, []byte(`{"hostIPv4":"`+address+`"}`), 0600); err != nil {
			t.Fatal(err)
		}
		answer, handled, err := dockerHostAnswer(hostQuery(t, "HOST.DOCKER.INTERNAL.", dnsmessage.TypeA), path, netip.Addr{})
		if err != nil || !handled {
			t.Fatalf("host answer: handled=%t error=%v", handled, err)
		}
		var response dnsmessage.Message
		if err := response.Unpack(answer); err != nil {
			t.Fatal(err)
		}
		if response.ID != 321 || len(response.Answers) != 1 || response.Answers[0].Body.(*dnsmessage.AResource).A != netip.MustParseAddr(address).As4() {
			t.Fatalf("host answer: %#v", response)
		}
	}
	answer, handled, err := dockerHostAnswer(hostQuery(t, "gateway.docker.internal.", dnsmessage.TypeA), "not-read-for-gateway", netip.MustParseAddr("172.19.0.1"))
	if err != nil || !handled {
		t.Fatalf("gateway answer: %v", err)
	}
	var response dnsmessage.Message
	_ = response.Unpack(answer)
	if response.Answers[0].Body.(*dnsmessage.AResource).A != netip.MustParseAddr("172.19.0.1").As4() {
		t.Fatalf("wrong network gateway: %#v", response)
	}
	if _, handled, err := dockerHostAnswer(hostQuery(t, "ordinary.example.", dnsmessage.TypeA), "not-read", netip.Addr{}); err != nil || handled {
		t.Fatalf("intercepted ordinary DNS: %v", err)
	}
}

func TestDockerHostDNSRejectsInvalidControlConfiguration(t *testing.T) {
	for _, data := range []string{`{"hostIPv4":"127.0.0.1"}`, `{"hostIPv4":"::1"}`, `{"hostIPv4":"172.28.240.1","extra":true}`, `{"hostIPv4":"172.28.240.1"} {}`} {
		path := filepath.Join(t.TempDir(), "hostaliases.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, handled, err := dockerHostAnswer(hostQuery(t, "host.docker.internal.", dnsmessage.TypeA), path, netip.Addr{}); !handled || err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestHostAliasesRespectDNSFilteringWithoutChangingUpstreamQueries(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "dnsfilter.json")
	if err := WriteConfig(path, Config{Inhibition: "ipv4", DefaultNetworkingMode: "dual-stack", Upstreams: []string{"192.0.2.53"}}); err != nil {
		t.Fatal(err)
	}
	settings := &settingsFile{path: path}
	forwarded := false
	forward := func(context.Context, []byte, bool, []string) ([]byte, error) {
		forwarded = true
		return []byte("upstream"), nil
	}
	answer, err := resolve(context.Background(), hostQuery(t, "host.docker.internal.", dnsmessage.TypeA), nil, false, settings, &resolverFile{}, nil, forward)
	if err != nil || forwarded {
		t.Fatalf("filtered host query leaked upstream: %v", err)
	}
	var response dnsmessage.Message
	_ = response.Unpack(answer)
	if len(response.Answers) != 0 {
		t.Fatal("IPv4-inhibited host query returned IPv4")
	}
	answer, err = resolve(context.Background(), hostQuery(t, "ordinary.example.", dnsmessage.TypeAAAA), nil, false, settings, &resolverFile{}, nil, forward)
	if err != nil || !forwarded || string(answer) != "upstream" {
		t.Fatalf("ordinary query changed: %q %v", answer, err)
	}
}
