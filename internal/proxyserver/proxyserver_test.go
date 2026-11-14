package proxyserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

func TestHTTPProxyUsesSOCKS5AndPreservesStreamingRequest(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/registry/v2/blobs/layer" {
			http.Error(writer, "unexpected request path: "+request.URL.Path, http.StatusBadRequest)
			return
		}
		if request.Header.Get("Proxy-Authorization") != "" {
			http.Error(writer, "proxy authorization leaked", http.StatusBadRequest)
			return
		}
		_, _ = io.Copy(writer, request.Body)
	}))
	defer target.Close()
	socks := newSOCKSFixture(t, strings.TrimPrefix(target.URL, "http://"))
	proxy := startProxy(t, api.ProxySettings{SOCKS5: socks.URL()})

	request, err := http.NewRequest(http.MethodPut, "http://198.51.100.10/registry/v2/blobs/layer", strings.NewReader("streamed image layer"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := proxyClient(proxy).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "streamed image layer" {
		t.Fatalf("proxy response = %q", body)
	}
	if got := <-socks.requests; got != "198.51.100.10:80" {
		t.Fatalf("SOCKS5 CONNECT target = %q", got)
	}
}

func TestHTTPSConnectUsesSOCKS5(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "secure registry response")
	}))
	defer target.Close()
	targetAddress := strings.TrimPrefix(target.URL, "https://")
	socks := newSOCKSFixture(t, targetAddress)
	proxy := startProxy(t, api.ProxySettings{SOCKS5: socks.URL()})

	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxy.URL),
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "example.com"},
	}}
	response, err := client.Get("https://198.51.100.10/v2/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "secure registry response" {
		t.Fatalf("HTTPS response = %d, %q", response.StatusCode, body)
	}
	if got := <-socks.requests; got != "198.51.100.10:443" {
		t.Fatalf("SOCKS5 CONNECT target = %q", got)
	}
}

func TestNoProxyBypassesSOCKS5AndBadUpdateKeepsCurrentRoute(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "direct")
	}))
	defer target.Close()
	socks := newSOCKSFixture(t, strings.TrimPrefix(target.URL, "http://"))
	proxy := startProxy(t, api.ProxySettings{SOCKS5: socks.URL(), NoProxy: "127.0.0.1"})

	if err := proxy.Server.Update(api.ProxySettings{SOCKS5: "https://not-a-socks-proxy.test:1080"}); err == nil {
		t.Fatal("invalid update unexpectedly succeeded")
	}
	response, err := proxyClient(proxy).Get(target.URL + "/no-proxy")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "direct" {
		t.Fatalf("no-proxy response = %q", body)
	}
	select {
	case request := <-socks.requests:
		t.Fatalf("no-proxy request unexpectedly used SOCKS5: %s", request)
	default:
	}

	response, err = proxyClient(proxy).Get("http://198.51.100.10/through-socks")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := <-socks.requests; got != "198.51.100.10:80" {
		t.Fatalf("retained SOCKS5 CONNECT target = %q", got)
	}
}

func TestUpdateSwitchesAndDisablesSOCKS5Route(t *testing.T) {
	newTarget := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, body)
		}))
	}
	firstTarget := newTarget("first route")
	defer firstTarget.Close()
	secondTarget := newTarget("second route")
	defer secondTarget.Close()
	firstSOCKS := newSOCKSFixture(t, strings.TrimPrefix(firstTarget.URL, "http://"))
	secondSOCKS := newSOCKSFixture(t, strings.TrimPrefix(secondTarget.URL, "http://"))
	proxy := startProxy(t, api.ProxySettings{SOCKS5: firstSOCKS.URL()})
	client := proxyClient(proxy)

	readResponse := func(target string) (string, error) {
		response, err := client.Get(target)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}

	body, err := readResponse("http://198.51.100.20/first")
	if err != nil || body != "first route" {
		t.Fatalf("first route response = %q, %v", body, err)
	}
	if got := <-firstSOCKS.requests; got != "198.51.100.20:80" {
		t.Fatalf("first SOCKS5 target = %q", got)
	}
	if err := proxy.Server.Update(api.ProxySettings{SOCKS5: secondSOCKS.URL()}); err != nil {
		t.Fatal(err)
	}
	body, err = readResponse("http://198.51.100.21/second")
	if err != nil || body != "second route" {
		t.Fatalf("updated route response = %q, %v", body, err)
	}
	if got := <-secondSOCKS.requests; got != "198.51.100.21:80" {
		t.Fatalf("updated SOCKS5 target = %q", got)
	}

	if err := proxy.Server.Update(api.ProxySettings{}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get("http://198.51.100.22/disabled")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("disabled route status = %d", response.StatusCode)
	}
}

func TestRequestCancellationStopsProxyUpstream(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(cancelled)
	}))
	defer target.Close()
	socks := newSOCKSFixture(t, strings.TrimPrefix(target.URL, "http://"))
	proxy := startProxy(t, api.ProxySettings{SOCKS5: socks.URL()})
	request, err := http.NewRequest(http.MethodGet, "http://198.51.100.30/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	done := make(chan error, 1)
	go func() {
		response, err := proxyClient(proxy).Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the upstream")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled proxy request unexpectedly succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy request did not stop after cancellation")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe request cancellation")
	}
}

func TestStartContextStopsListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Start(ctx, "127.0.0.1:0", api.ProxySettings{SOCKS5: "socks5://127.0.0.1:1080"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-server.Done():
		if err := server.Err(); err != nil {
			t.Fatalf("listener stopped with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy listener did not stop after context cancellation")
	}
}

func TestRunReconcilesProxyLifecycle(t *testing.T) {
	target := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, body)
		}))
	}
	firstTarget, secondTarget := target("first route"), target("second route")
	defer firstTarget.Close()
	defer secondTarget.Close()
	firstSOCKS := newSOCKSFixture(t, strings.TrimPrefix(firstTarget.URL, "http://"))
	secondSOCKS := newSOCKSFixture(t, strings.TrimPrefix(secondTarget.URL, "http://"))
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}

	var mu sync.RWMutex
	settings := api.ProxySettings{}
	source := func() api.ProxySettings {
		mu.RLock()
		defer mu.RUnlock()
		return settings
	}
	set := func(value api.ProxySettings) {
		mu.Lock()
		settings = value
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, address, source, nil) }()

	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("empty initial settings unexpectedly opened a listener")
	}
	proxyURL := &url.URL{Scheme: "http", Host: address}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	waitResponse := func(targetURL string, status int, body string) {
		t.Helper()
		deadline := time.NewTimer(4 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			requestCtx, requestCancel := context.WithTimeout(ctx, 250*time.Millisecond)
			request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, targetURL, nil)
			if err == nil {
				response, requestErr := client.Do(request)
				if requestErr == nil {
					responseBody, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					requestCancel()
					if readErr == nil && response.StatusCode == status && string(responseBody) == body {
						return
					}
				} else {
					requestCancel()
				}
			} else {
				requestCancel()
			}
			select {
			case <-ctx.Done():
				t.Fatal("proxy lifecycle canceled before the requested route became active")
			case <-deadline.C:
				t.Fatalf("proxy route did not return HTTP %d with body %q", status, body)
			case <-ticker.C:
			}
		}
	}

	set(api.ProxySettings{SOCKS5: firstSOCKS.URL()})
	waitResponse("http://198.51.100.40/first", http.StatusOK, "first route")
	if got := <-firstSOCKS.requests; got != "198.51.100.40:80" {
		t.Fatalf("initial SOCKS5 target = %q", got)
	}
	set(api.ProxySettings{SOCKS5: secondSOCKS.URL()})
	waitResponse("http://198.51.100.41/second", http.StatusOK, "second route")
	if got := <-secondSOCKS.requests; got != "198.51.100.41:80" {
		t.Fatalf("updated SOCKS5 target = %q", got)
	}
	set(api.ProxySettings{})
	waitResponse("http://198.51.100.42/disabled", http.StatusServiceUnavailable, "SOCKS5 proxy is not configured\n")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run stopped with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not join its listener during shutdown")
	}
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("Run left its listener open after cancellation")
	}
}

func TestRunReportsOnceAndRetriesListenerFailure(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "recovered")
	}))
	defer target.Close()
	socks := newSOCKSFixture(t, strings.TrimPrefix(target.URL, "http://"))
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := blocker.Addr().String()
	reports := make(chan error, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, address, func() api.ProxySettings {
			return api.ProxySettings{SOCKS5: socks.URL()}
		}, func(err error) {
			reports <- err
		})
	}()

	select {
	case err := <-reports:
		if err == nil {
			t.Fatal("listener conflict was reported as recovery")
		}
	case <-time.After(time.Second):
		t.Fatal("listener conflict was not reported")
	}
	quiet := time.NewTimer(1100 * time.Millisecond)
	select {
	case err := <-reports:
		quiet.Stop()
		t.Fatalf("unchanged listener error was reported again: %v", err)
	case <-quiet.C:
	}
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reports:
		if err != nil {
			t.Fatalf("listener recovery was reported as failure: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("listener retry did not report recovery")
	}

	proxyURL := &url.URL{Scheme: "http", Host: address}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	response, err := client.Get("http://198.51.100.50/recovered")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "recovered" {
		t.Fatalf("recovered listener response = %d, %q, %v", response.StatusCode, body, err)
	}
	select {
	case got := <-socks.requests:
		if got != "198.51.100.50:80" {
			t.Fatalf("recovered SOCKS5 target = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("recovered request did not use SOCKS5")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run stopped with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestCloseCancelsActiveCONNECTTunnel(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	socks := newSOCKSFixture(t, target.Addr().String())
	proxy := startProxy(t, api.ProxySettings{SOCKS5: socks.URL()})

	client, err := net.DialTimeout("tcp", proxy.Server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "CONNECT 198.51.100.10:443 HTTP/1.1\r\nHost: 198.51.100.10:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200 Connection Established") {
		t.Fatalf("CONNECT response = %q, %v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read CONNECT headers: %v", readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	remote := <-accepted
	defer remote.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.Server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("client tunnel remained open after proxy close")
	}
}

func TestStartRequiresLoopbackAndValidProxy(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name     string
		address  string
		settings api.ProxySettings
	}{
		{name: "non-loopback listener", address: "0.0.0.0:7444", settings: api.ProxySettings{SOCKS5: "socks5://127.0.0.1:1080"}},
		{name: "missing SOCKS5", address: "127.0.0.1:0"},
		{name: "SOCKS5 credentials", address: "127.0.0.1:0", settings: api.ProxySettings{SOCKS5: "socks5://user:secret@127.0.0.1:1080"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := Start(ctx, test.address, test.settings)
			if err == nil {
				_ = server.Close(context.Background())
				t.Fatal("Start unexpectedly accepted invalid settings")
			}
		})
	}
}

type runningProxy struct {
	Server *Server
	URL    *url.URL
}

func startProxy(t *testing.T, settings api.ProxySettings) runningProxy {
	t.Helper()
	server, err := Start(context.Background(), "127.0.0.1:0", settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close test proxy: %v", err)
		}
	})
	return runningProxy{Server: server, URL: &url.URL{Scheme: "http", Host: server.Addr().String()}}
}

func proxyClient(proxy runningProxy) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy.URL)}}
}

type socksFixture struct {
	listener    net.Listener
	destination string
	requests    chan string
	mu          sync.Mutex
	connections map[net.Conn]struct{}
}

func newSOCKSFixture(t *testing.T, destination string) *socksFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &socksFixture{listener: listener, destination: destination, requests: make(chan string, 16), connections: make(map[net.Conn]struct{})}
	t.Cleanup(fixture.close)
	go fixture.serve()
	return fixture
}

func (s *socksFixture) URL() string { return "socks5://" + s.listener.Addr().String() }

func (s *socksFixture) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connections[conn] = struct{}{}
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *socksFixture) handle(client net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.connections, client)
		s.mu.Unlock()
		_ = client.Close()
	}()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(client)
	var greeting [2]byte
	if _, err := io.ReadFull(reader, greeting[:]); err != nil || greeting[0] != 5 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	if !containsByte(methods, 0) {
		_, _ = client.Write([]byte{5, 0xff})
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil || header[0] != 5 || header[1] != 1 {
		return
	}
	host, err := readSOCKSHost(reader, header[3])
	if err != nil {
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(reader, portBytes[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes[:])
	s.requests <- net.JoinHostPort(host, strconv.Itoa(int(port)))
	remote, err := net.DialTimeout("tcp", s.destination, time.Second)
	if err != nil {
		_, _ = client.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer remote.Close()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})
	bridge(&tunnel{client: client, remote: remote}, reader)
}

func readSOCKSHost(reader *bufio.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		address := make([]byte, 4)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	case 3:
		var length byte
		if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
			return "", err
		}
		address := make([]byte, int(length))
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return string(address), nil
	case 4:
		address := make([]byte, 16)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	default:
		return "", fmt.Errorf("unexpected SOCKS address type %d", addressType)
	}
}

func containsByte(values []byte, target byte) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *socksFixture) close() {
	_ = s.listener.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.connections {
		_ = conn.Close()
	}
}
