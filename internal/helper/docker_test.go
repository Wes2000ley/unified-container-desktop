package helper

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
)

func TestDockerProxyStreamsThroughAuthenticatedHelper(t *testing.T) {
	continueUpload := make(chan struct{})
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1.45/images/my%2Frepo/json" || r.URL.RawQuery != "a=x%3By&a=b;v=1" {
			t.Errorf("unexpected Docker request: %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, buf); err != nil || string(buf) != "first" {
			t.Errorf("first upload = %q, %v", buf, err)
			return
		}
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("enable Docker full duplex: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "one")
		w.(http.Flusher).Flush()
		rest, err := io.ReadAll(r.Body)
		if err != nil || string(rest) != "second" {
			t.Errorf("remaining upload = %q, %v", rest, err)
			return
		}
		_, _ = io.WriteString(w, "two")
	}))
	defer docker.Close()
	target, err := url.Parse(docker.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := startTestServer(t, nil, &api.EventBus{}, dockerproxy.New(target, nil))
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	frontend := httptest.NewServer(client.DockerHandler())
	defer frontend.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, frontend.URL+"/v1.45/images/my%2Frepo/json?a=x%3By&a=b;v=1", reader)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		response *http.Response
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		results <- result{response, err}
	}()
	go func() {
		_, _ = io.WriteString(writer, "first")
		select {
		case <-continueUpload:
			_, _ = io.WriteString(writer, "second")
		case <-ctx.Done():
		}
		_ = writer.Close()
	}()
	var response *http.Response
	select {
	case got := <-results:
		if got.err != nil {
			t.Fatal(got.err)
		}
		response = got.response
	case <-ctx.Done():
		t.Fatal("streaming upload did not reach Docker")
	}
	defer response.Body.Close()
	first := make([]byte, 3)
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "one" {
		t.Fatalf("first response chunk = %q, %v", first, err)
	}
	close(continueUpload)
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != "two" || response.StatusCode != http.StatusOK {
		t.Fatalf("remaining response = %q, status %d, %v", rest, response.StatusCode, err)
	}
}

func TestDockerProxyUpgradesAndCancellation(t *testing.T) {
	for _, test := range []struct{ protocol, path string }{
		{"tcp", "/v1.45/exec/id/start"},
		{"h2c", "/v1.45/session"},
	} {
		t.Run(test.protocol, func(t *testing.T) { testDockerUpgrade(t, test.protocol, test.path) })
	}
}

func testDockerUpgrade(t *testing.T, protocol, path string) {
	t.Helper()
	closed := make(chan struct{})
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		defer close(closed)
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", protocol)
		_ = rw.Flush()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(rw, buf); err != nil || string(buf) != "hello" {
			t.Errorf("upgraded input = %q, %v", buf, err)
			return
		}
		_, _ = rw.WriteString("world")
		_ = rw.Flush()
		_, _ = io.Copy(io.Discard, rw)
	}))
	defer docker.Close()
	target, err := url.Parse(docker.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := startTestServer(t, nil, &api.EventBus{}, dockerproxy.New(target, nil))
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	frontend := httptest.NewUnstartedServer(client.DockerHandler())
	frontend.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	frontend.Start()
	defer frontend.Close()
	address := strings.TrimPrefix(frontend.URL, "http://")
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", path, protocol)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response = %v, %v", response, err)
	}
	_, _ = io.WriteString(conn, "hello")
	buf := make([]byte, 5)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "world" {
		t.Fatalf("upgraded output = %q, %v", buf, err)
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not close the upgraded Docker stream")
	}
}

func TestDockerProxyUpgradeHalfClose(t *testing.T) {
	backendEOF := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBackend := func() { releaseOnce.Do(func() { close(release) }) }
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		input, err := io.ReadAll(rw)
		if err != nil || string(input) != "input" {
			t.Errorf("upgraded input = %q, %v", input, err)
			return
		}
		close(backendEOF)
		<-release
		_, _ = rw.WriteString("after EOF")
		_ = rw.Flush()
	}))
	defer docker.Close()
	target, err := url.Parse(docker.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := startTestServer(t, nil, &api.EventBus{}, dockerproxy.New(target, nil))
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	proxyDone := make(chan struct{})
	proxy := client.DockerHandler()
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
		close(proxyDone)
	}))
	defer frontend.Close()
	defer releaseBackend()
	address, err := net.ResolveTCPAddr("tcp", strings.TrimPrefix(frontend.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTCP("tcp", nil, address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprint(conn, "POST /v1.45/exec/id/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response = %v, %v", response, err)
	}
	_, _ = io.WriteString(conn, "input")
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-backendEOF:
	case <-time.After(5 * time.Second):
		t.Fatal("Docker did not receive stdin EOF")
	}
	select {
	case <-proxyDone:
		t.Fatal("proxy closed before Docker could finish writing")
	case <-time.After(100 * time.Millisecond):
	}
	releaseBackend()
	output, err := io.ReadAll(reader)
	if err != nil || string(output) != "after EOF" {
		t.Fatalf("output after stdin close = %q, %v", output, err)
	}
}

func TestDockerRouteRequiresControllerCertificate(t *testing.T) {
	var called atomic.Int32
	s := startTestServer(t, nil, &api.EventBus{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		_, _ = io.WriteString(w, "OK")
	}))
	wrongName, _, _ := s.ca.issue(t, "not-the-controller", x509.ExtKeyUsageClientAuth)
	for _, cert := range []*tls.Certificate{nil, &wrongName} {
		response, err := s.httpClient(cert).Get(s.server.URL + "/docker/_ping")
		if err == nil {
			response.Body.Close()
			t.Fatal("Docker API accepted a client without the controller certificate")
		}
	}
	response, err := s.httpClient(&s.controller).Get(s.server.URL + "/docker/_ping")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || called.Load() != 1 {
		t.Fatalf("authenticated Docker route = %d, called %d times", response.StatusCode, called.Load())
	}
	response, err = s.httpClient(&s.controller).Get(s.server.URL + "/docker-other/_ping")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound || called.Load() != 1 {
		t.Fatalf("wrong Docker route = %d, called %d times", response.StatusCode, called.Load())
	}
}

func TestDockerHelperRejectsUnauthenticatedRequestsAndReturnsErrors(t *testing.T) {
	called := false
	handler := newHandler(nil, nil, t.TempDir(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/docker/_ping", nil))
	if recorder.Code != http.StatusUnauthorized || called {
		t.Fatalf("unauthenticated Docker route = %d, called %v", recorder.Code, called)
	}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("socket unavailable") })
	proxy := dockerproxy.New(&url.URL{Scheme: "http", Host: "docker"}, transport)
	recorder = httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), `"message":"Docker engine connection failed: socket unavailable"`) {
		t.Fatalf("proxy error = %d, %s", recorder.Code, recorder.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
