package dnsfilter

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestFilteredAnswer(t *testing.T) {
	tests := []struct {
		name     string
		config   Config
		kind     dnsmessage.Type
		filtered bool
	}{
		{"none A", Config{Inhibition: "none", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeA, false},
		{"none AAAA", Config{Inhibition: "none", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeAAAA, false},
		{"IPv4 A", Config{Inhibition: "ipv4", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeA, true},
		{"IPv4 AAAA", Config{Inhibition: "ipv4", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeAAAA, false},
		{"IPv6 AAAA", Config{Inhibition: "ipv6", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeAAAA, true},
		{"IPv6 A", Config{Inhibition: "ipv6", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeA, false},
		{"auto IPv4-only AAAA", Config{Inhibition: "auto", DefaultNetworkingMode: "ipv4only"}, dnsmessage.TypeAAAA, true},
		{"auto IPv4-only A", Config{Inhibition: "auto", DefaultNetworkingMode: "ipv4only"}, dnsmessage.TypeA, false},
		{"auto IPv6-only A", Config{Inhibition: "auto", DefaultNetworkingMode: "ipv6only"}, dnsmessage.TypeA, true},
		{"auto IPv6-only AAAA", Config{Inhibition: "auto", DefaultNetworkingMode: "ipv6only"}, dnsmessage.TypeAAAA, false},
		{"auto dual-stack A", Config{Inhibition: "auto", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeA, false},
		{"auto dual-stack AAAA", Config{Inhibition: "auto", DefaultNetworkingMode: "dual-stack"}, dnsmessage.TypeAAAA, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			answer, filtered, err := filteredAnswer(testDNSQuery(test.kind), test.config)
			if err != nil {
				t.Fatal(err)
			}
			if filtered != test.filtered {
				t.Fatalf("filtered = %t, want %t", filtered, test.filtered)
			}
			if !filtered {
				if len(answer) != 0 {
					t.Fatalf("unfiltered query returned %d bytes", len(answer))
				}
				return
			}
			var response dnsmessage.Message
			if err := response.Unpack(answer); err != nil {
				t.Fatal(err)
			}
			if !response.Header.Response || response.Header.RCode != dnsmessage.RCodeSuccess || len(response.Answers) != 0 {
				t.Fatalf("filtered response is not NOERROR/NODATA: %#v", response)
			}
			if len(response.Questions) != 1 || response.Questions[0].Type != test.kind {
				t.Fatalf("filtered response lost the question: %#v", response.Questions)
			}
		})
	}
}

func TestFilteredAnswerRejectsUnsupportedRequestShape(t *testing.T) {
	if _, _, err := filteredAnswer([]byte{1, 2, 3}, Config{Inhibition: "ipv4", DefaultNetworkingMode: "ipv4only"}); err == nil {
		t.Fatal("malformed DNS request was accepted")
	}
	query := testDNSQuery(dnsmessage.TypeA)
	var message dnsmessage.Message
	if err := message.Unpack(query); err != nil {
		t.Fatal(err)
	}
	message.Questions = append(message.Questions, message.Questions[0])
	query, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := filteredAnswer(query, Config{Inhibition: "ipv4", DefaultNetworkingMode: "ipv4only"}); err == nil {
		t.Fatal("multi-question DNS request was accepted")
	}
}

func TestSettingsFileDefaultsAndWriteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "dnsfilter.json")
	settings := &settingsFile{path: path}
	got, err := settings.load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Inhibition != "auto" || got.DefaultNetworkingMode != "ipv4only" {
		t.Fatalf("startup defaults = %#v", got)
	}

	want := Config{Inhibition: "ipv6", DefaultNetworkingMode: "dual-stack", Upstreams: []string{"192.0.2.53"}}
	if err := WriteConfig(path, want); err != nil {
		t.Fatal(err)
	}
	got, err = (&settingsFile{path: path}).load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded config = %#v, want %#v", got, want)
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := WriteConfig(invalidPath, Config{Inhibition: "bad", DefaultNetworkingMode: "ipv4only"}); err == nil {
		t.Fatal("invalid DNS policy was written")
	}
	if err := WriteConfig(invalidPath, Config{Inhibition: "auto", DefaultNetworkingMode: "ipv4only", Upstreams: []string{IPv4Address, IPv6Address}}); err == nil {
		t.Fatal("UCD DNS listeners were accepted as the only upstreams")
	}
	if _, err := os.Stat(invalidPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid policy file stat error = %v", err)
	}
	if err := WriteConfig("relative.json", want); err == nil {
		t.Fatal("relative configuration path was accepted")
	}
}

func TestResolverFileUsesSystemAndConfiguredUpstreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	contents := "# system resolver\nnameserver 192.0.2.53\nsearch example.test\nnameserver 2001:db8::53\ninvalid 203.0.113.1\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	resolver := &resolverFile{path: path}
	servers, err := resolver.servers(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"192.0.2.53:53", "[2001:db8::53]:53"}
	if !reflect.DeepEqual(servers, want) {
		t.Fatalf("system resolvers = %v, want %v", servers, want)
	}
	explicit := []string{IPv4Address, "203.0.113.53", IPv6Address, "2001:db8::53"}
	servers, err = resolver.servers(explicit)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"203.0.113.53:53", "[2001:db8::53]:53"}
	if !reflect.DeepEqual(servers, want) {
		t.Fatalf("configured resolvers = %v, want %v", servers, want)
	}
}

func TestResolverFileRejectsOnlySelfReferentialNameservers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	contents := "nameserver " + IPv4Address + "\nnameserver " + IPv6Address + "\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&resolverFile{path: path}).servers(nil); err == nil || !strings.Contains(err.Error(), "forwarding loop") {
		t.Fatalf("self-referential system resolver error = %v", err)
	}
}

func TestSelectPolicyUsesMostSpecificNetworkAndRejectsConflict(t *testing.T) {
	wide := networkPolicy{subnet: netip.MustParsePrefix("172.16.0.0/12"), mode: "none", ipv4: true}
	specific := networkPolicy{subnet: netip.MustParsePrefix("172.18.0.0/16"), mode: "ipv4", ipv4: true}
	selected, found, err := selectPolicy([]networkPolicy{specific, wide}, netip.MustParseAddr("172.18.1.9"))
	if err != nil || !found || selected.mode != "ipv4" {
		t.Fatalf("specific policy = %#v, found=%t, err=%v", selected, found, err)
	}
	if _, found, err := selectPolicy([]networkPolicy{wide}, netip.MustParseAddr("192.0.2.1")); err != nil || found {
		t.Fatalf("unmatched policy found=%t, err=%v", found, err)
	}
	conflict := wide
	conflict.mode = "ipv6"
	if _, _, err := selectPolicy([]networkPolicy{wide, conflict}, netip.MustParseAddr("172.18.1.9")); err == nil {
		t.Fatal("overlapping conflicting networks were accepted")
	}
}

func TestApplyNetworkPolicyAutoUsesEnabledFamilies(t *testing.T) {
	tests := []struct {
		name       string
		ipv4       bool
		ipv6       bool
		mode       string
		filterA    bool
		filterAAAA bool
	}{
		{name: "IPv4-only", ipv4: true, mode: "ipv4only", filterAAAA: true},
		{name: "IPv6-only", ipv6: true, mode: "ipv6only", filterA: true},
		{name: "dual-stack", ipv4: true, ipv6: true, mode: "dual-stack"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := applyNetworkPolicy(Config{Inhibition: "auto", DefaultNetworkingMode: "dual-stack"}, networkPolicy{mode: "auto", ipv4: test.ipv4, ipv6: test.ipv6})
			if err != nil {
				t.Fatal(err)
			}
			if config.DefaultNetworkingMode != test.mode {
				t.Fatalf("network mode = %q, want %q", config.DefaultNetworkingMode, test.mode)
			}
			if got := filteredType(config, dnsmessage.TypeA); got != test.filterA {
				t.Fatalf("filter A = %t, want %t", got, test.filterA)
			}
			if got := filteredType(config, dnsmessage.TypeAAAA); got != test.filterAAAA {
				t.Fatalf("filter AAAA = %t, want %t", got, test.filterAAAA)
			}
		})
	}
	if _, err := applyNetworkPolicy(Config{Inhibition: "auto", DefaultNetworkingMode: "dual-stack"}, networkPolicy{mode: "auto"}); err == nil {
		t.Fatal("network with both families disabled was accepted")
	}
}

func TestResolveFiltersAndForwardsByPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dnsfilter.json")
	if err := WriteConfig(path, Config{Inhibition: "auto", DefaultNetworkingMode: "ipv4only"}); err != nil {
		t.Fatal(err)
	}
	resolvPath := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolvPath, []byte("nameserver 192.0.2.53\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := &settingsFile{path: path}
	resolvers := &resolverFile{path: resolvPath}
	forwarded := 0
	forward := func(_ context.Context, query []byte, tcp bool, upstreams []string) ([]byte, error) {
		forwarded++
		if tcp {
			t.Fatal("unexpected TCP forwarding")
		}
		if !reflect.DeepEqual(upstreams, []string{"192.0.2.53:53"}) {
			t.Fatalf("upstreams = %v", upstreams)
		}
		return testDNSResponse(query), nil
	}
	response, err := resolve(context.Background(), testDNSQuery(dnsmessage.TypeAAAA), &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 12345}, false, settings, resolvers, nil, forward)
	if err != nil {
		t.Fatal(err)
	}
	var filtered dnsmessage.Message
	if err := filtered.Unpack(response); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Answers) != 0 || forwarded != 0 {
		t.Fatalf("AAAA was not locally inhibited: answers=%d forwarded=%d", len(filtered.Answers), forwarded)
	}
	if _, err := resolve(context.Background(), testDNSQuery(dnsmessage.TypeA), &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 12345}, false, settings, resolvers, nil, forward); err != nil {
		t.Fatal(err)
	}
	if forwarded != 1 {
		t.Fatalf("forwarded queries = %d, want 1", forwarded)
	}
}

func TestUpstreamAnswerValidation(t *testing.T) {
	query := testDNSQuery(dnsmessage.TypeA)
	answer := testDNSResponse(query)
	if _, err := validateUpstreamAnswer(query, answer); err != nil {
		t.Fatal(err)
	}

	var response dnsmessage.Message
	if err := response.Unpack(answer); err != nil {
		t.Fatal(err)
	}
	response.Header.ID++
	badID, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpstreamAnswer(query, badID); err == nil {
		t.Fatal("mismatched upstream identifier was accepted")
	}
	response.Header.ID--
	response.Questions[0].Name = dnsmessage.MustNewName("other.example.")
	badQuestion, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateUpstreamAnswer(query, badQuestion); err == nil {
		t.Fatal("mismatched upstream question was accepted")
	}
}

func testDNSQuery(kind dnsmessage.Type) []byte {
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 123, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: kind, Class: dnsmessage.ClassINET}},
	}
	data, err := message.Pack()
	if err != nil {
		panic(err)
	}
	return data
}

func testDNSResponse(query []byte) []byte {
	var request dnsmessage.Message
	if err := request.Unpack(query); err != nil {
		panic(err)
	}
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: request.ID, Response: true, RecursionAvailable: true},
		Questions: request.Questions,
	}
	data, err := response.Pack()
	if err != nil {
		panic(err)
	}
	return data
}

func TestWriteAllHandlesShortAndZeroWrites(t *testing.T) {
	var output strings.Builder
	if err := writeAll(shortWriter{writer: &output}, []byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if output.String() != "abcdef" {
		t.Fatalf("short-write output = %q", output.String())
	}
	if err := writeAll(zeroWriter{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero write error = %v", err)
	}
}

type shortWriter struct{ writer *strings.Builder }

func (w shortWriter) Write(data []byte) (int, error) {
	if len(data) > 2 {
		data = data[:2]
	}
	return w.writer.Write(data)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
