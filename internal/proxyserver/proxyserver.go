// Package proxyserver provides the loopback HTTP bridge required by Docker
// Engine when its upstream proxy is SOCKS5.
package proxyserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skyri/ucd/internal/api"
	"golang.org/x/net/http/httpproxy"
	xproxy "golang.org/x/net/proxy"
)

const maxHeaderBytes = 1 << 20

// Server owns a loopback HTTP proxy listener and all of its active tunnels.
// Close stops the listener, waits for regular HTTP requests, and closes tunnels.
type Server struct {
	listener net.Listener
	http     *http.Server
	current  atomic.Pointer[route]
	mu       sync.Mutex
	tunnels  map[*tunnel]struct{}
	closing  bool
	close    sync.Once
	closeErr error
	done     chan struct{}
	serveErr error
	stopCtx  func() bool
}

type route struct {
	enabled   bool
	dialer    xproxy.ContextDialer
	proxyFunc func(*url.URL) (*url.URL, error)
	transport *http.Transport
	forward   *httputil.ReverseProxy
}

type tunnel struct {
	client net.Conn
	remote net.Conn
}

// Start binds a local HTTP proxy at address and applies the SOCKS5 settings.
// address must be a loopback IP and port. The server remains bound but returns
// 503 when SOCKS5 is disabled, allowing the owner to update settings without
// racing daemon restarts. The caller owns ctx and must call Close during exit.
func Start(ctx context.Context, address string, settings api.ProxySettings) (*Server, error) {
	if ctx == nil {
		return nil, errors.New("proxy server context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateListenAddress(address); err != nil {
		return nil, err
	}
	route, err := newRoute(settings)
	if err != nil {
		return nil, fmt.Errorf("configure SOCKS5 HTTP bridge: %w", err)
	}
	if !route.enabled {
		return nil, errors.New("SOCKS5 proxy settings are required to start the HTTP bridge")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		route.close()
		return nil, fmt.Errorf("listen for Docker SOCKS5 proxy traffic on %s: %w", address, err)
	}
	server := &Server{listener: listener, tunnels: make(map[*tunnel]struct{}), done: make(chan struct{})}
	server.current.Store(route)
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	go func() {
		err := server.http.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			server.serveErr = err
		}
		close(server.done)
	}()
	server.stopCtx = context.AfterFunc(ctx, func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Close(closeCtx)
	})
	return server, nil
}

// Update replaces the SOCKS5 route without interrupting in-flight requests.
// When SOCKS5 is cleared the listener responds with 503 until it is configured
// again; the owner can first restart Docker with the direct proxy settings.
func (s *Server) Update(settings api.ProxySettings) error {
	if s == nil {
		return errors.New("proxy server is nil")
	}
	route, err := newRoute(settings)
	if err != nil {
		return fmt.Errorf("configure SOCKS5 HTTP bridge: %w", err)
	}
	old := s.current.Load()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		route.close()
		return net.ErrClosed
	}
	s.current.Store(route)
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	return nil
}

// Addr reports the listener address, including the selected port when Start
// was given port zero.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Done closes when the HTTP listener stops. Err reports an unexpected serve
// failure after Done closes.
func (s *Server) Done() <-chan struct{} { return s.done }

// Err returns the listener error after Done closes.
func (s *Server) Err() error {
	select {
	case <-s.done:
		return s.serveErr
	default:
		return nil
	}
}

// Close gracefully stops HTTP requests and closes every upgraded tunnel.
func (s *Server) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("proxy server close context is required")
	}
	if s.stopCtx != nil {
		s.stopCtx()
	}
	s.close.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()

		if err := s.http.Shutdown(ctx); err != nil {
			s.closeErr = errors.Join(err, s.http.Close())
		}
		s.closeTunnels()
		<-s.done
		if route := s.current.Load(); route != nil {
			route.close()
		}
		s.closeErr = errors.Join(s.closeErr, s.serveErr)
	})
	return s.closeErr
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid proxy listen address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return errors.New("proxy listen address must use a loopback IP")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("proxy listen port must be between 0 and 65535")
	}
	return nil
}

func newRoute(settings api.ProxySettings) (*route, error) {
	if err := api.ValidateProxySettings("SOCKS5 bridge", settings); err != nil {
		return nil, err
	}
	if settings.SOCKS5 == "" {
		return &route{}, nil
	}
	proxyURL, err := url.Parse(settings.SOCKS5)
	if err != nil {
		return nil, fmt.Errorf("parse SOCKS5 proxy URL: %w", err)
	}
	dialer, err := xproxy.FromURL(proxyURL, xproxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 dialer does not support request cancellation")
	}
	config := &httpproxy.Config{HTTPProxy: settings.SOCKS5, HTTPSProxy: settings.SOCKS5, NoProxy: settings.NoProxy}
	proxyFunc := config.ProxyFunc()
	transport := &http.Transport{
		Proxy: func(request *http.Request) (*url.URL, error) {
			return proxyFunc(request.URL)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	forward := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(request *httputil.ProxyRequest) {
			upstream := *request.In.URL
			upstream.Path = ""
			upstream.RawPath = ""
			upstream.RawQuery = ""
			upstream.Fragment = ""
			upstream.ForceQuery = false
			request.SetURL(&upstream)
			request.Out.Header.Del("Proxy-Authorization")
			request.Out.Header.Del("Proxy-Connection")
		},
	}
	return &route{enabled: true, dialer: contextDialer, proxyFunc: proxyFunc, transport: transport, forward: forward}, nil
}

func (r *route) close() {
	if r != nil && r.transport != nil {
		r.transport.CloseIdleConnections()
	}
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	route := s.current.Load()
	if route == nil || !route.enabled {
		http.Error(writer, "SOCKS5 proxy is not configured", http.StatusServiceUnavailable)
		return
	}
	if request.Method == http.MethodConnect {
		s.serveConnect(writer, request, route)
		return
	}
	if request.URL == nil || !strings.EqualFold(request.URL.Scheme, "http") || request.URL.Hostname() == "" || request.URL.User != nil {
		http.Error(writer, "absolute HTTP proxy URL required", http.StatusBadRequest)
		return
	}
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	request.RequestURI = ""
	route.forward.ServeHTTP(writer, request)
}

func (s *Server) serveConnect(writer http.ResponseWriter, request *http.Request, route *route) {
	target, err := connectAddress(request.Host)
	if err != nil {
		http.Error(writer, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	proxy, err := route.proxyFunc(&url.URL{Scheme: "https", Host: target})
	if err != nil {
		http.Error(writer, "SOCKS5 proxy route failed", http.StatusBadGateway)
		return
	}
	var remote net.Conn
	if proxy == nil {
		remote, err = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(request.Context(), "tcp", target)
	} else {
		remote, err = route.dialer.DialContext(request.Context(), "tcp", target)
	}
	if err != nil {
		http.Error(writer, "proxy tunnel could not connect to target", http.StatusBadGateway)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		remote.Close()
		http.Error(writer, "HTTP connection tunneling is unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		remote.Close()
		return
	}
	tunnel := &tunnel{client: client, remote: remote}
	if !s.addTunnel(tunnel) {
		return
	}
	defer s.removeTunnel(tunnel)
	defer tunnel.close()
	if _, err := io.WriteString(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	bridge(tunnel, buffered.Reader)
}

func connectAddress(authority string) (string, error) {
	host, portText, err := net.SplitHostPort(authority)
	if err != nil || host == "" || strings.ContainsAny(host, "\r\n\x00") {
		return "", errors.New("invalid proxy CONNECT authority")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("invalid proxy CONNECT port")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (s *Server) addTunnel(value *tunnel) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		value.close()
		return false
	}
	s.tunnels[value] = struct{}{}
	return true
}

func (s *Server) removeTunnel(value *tunnel) {
	s.mu.Lock()
	delete(s.tunnels, value)
	s.mu.Unlock()
}

func (s *Server) closeTunnels() {
	s.mu.Lock()
	tunnels := make([]*tunnel, 0, len(s.tunnels))
	for value := range s.tunnels {
		tunnels = append(tunnels, value)
	}
	s.mu.Unlock()
	for _, value := range tunnels {
		value.close()
	}
}

func (t *tunnel) close() {
	_ = t.client.Close()
	_ = t.remote.Close()
}

func bridge(value *tunnel, buffered io.Reader) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(value.remote, buffered)
		closeWrite(value.remote)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(value.client, value.remote)
		closeWrite(value.client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(conn net.Conn) {
	if value, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = value.CloseWrite()
	}
}
