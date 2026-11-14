//go:build linux

package helper

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dnsfilter"
	"github.com/skyri/ucd/internal/dockerproxy"
	"github.com/skyri/ucd/internal/engine"
	"github.com/skyri/ucd/internal/proxyserver"
)

// Serve runs the Linux Docker helper until ctx is canceled. It closes its
// backend and gracefully stops accepting requests before returning.
func Serve(ctx context.Context, config Config) error {
	if err := config.validate(); err != nil {
		return err
	}
	if err := engine.ConfigureServiceDockerEnvironment(config.DataRoot); err != nil {
		return err
	}
	roots, err := loadCA(config.CACert)
	if err != nil {
		return fmt.Errorf("load controller CA: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(config.ServerCert, config.ServerKey)
	if err != nil {
		return fmt.Errorf("load helper server certificate and key: %w", err)
	}
	if err := verifyServerCertificate(cert, roots); err != nil {
		return fmt.Errorf("verify helper server certificate: %w", err)
	}
	events := &api.EventBus{}
	backend, err := engine.New("linux", "unix:///var/run/docker.sock", config.DataRoot, events)
	if err != nil {
		return fmt.Errorf("initialize Linux engine: %w", err)
	}
	defer backend.Close()
	dnsCtx, stopDNS := context.WithCancel(ctx)
	dnsResult := make(chan error, 1)
	dnsReady := make(chan struct{}, 1)
	dnsPath := filepath.Join(config.DataRoot, "config", "dnsfilter.json")
	go func() { dnsResult <- dnsfilter.Start(dnsCtx, dnsPath, dnsReady) }()
	dnsJoined := false
	defer func() {
		stopDNS()
		if !dnsJoined {
			<-dnsResult
		}
	}()
	select {
	case <-dnsReady:
	case err := <-dnsResult:
		dnsJoined = true
		if err != nil {
			return fmt.Errorf("serve Linux container DNS: %w", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("Linux container DNS stopped before its listeners were ready")
	case <-ctx.Done():
		return nil
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("listen for Linux helper calls: %w", err)
	}
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	watchResult := make(chan error, 1)
	go func() { watchResult <- backend.Run(watchCtx) }()
	watchJoined := false
	defer func() {
		stopWatching()
		if !watchJoined {
			<-watchResult
		}
	}()
	proxyCtx, stopProxy := context.WithCancel(ctx)
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- proxyserver.Run(proxyCtx, "127.0.0.1:7445", func() api.ProxySettings {
			return backend.RuntimeConfiguration().DockerProxy
		}, func(err error) {
			if err != nil {
				events.Publish(api.Event{Type: "proxy.failed", Engine: "linux", Message: err.Error()})
				return
			}
			events.Publish(api.Event{Type: "proxy.restored", Engine: "linux", Message: "Docker proxy configuration is active"})
		})
	}()
	proxyJoined := false
	defer func() {
		stopProxy()
		if !proxyJoined {
			<-proxyResult
		}
	}()
	dockerTransport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}
	defer dockerTransport.CloseIdleConnections()
	server := &http.Server{
		Handler:           newHandler(backend, events, config.DataRoot, backend.DockerPolicyHandler(dockerproxy.New(&url.URL{Scheme: "http", Host: "docker"}, dockerTransport))),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return watchCtx },
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    roots,
			VerifyConnection: func(state tls.ConnectionState) error {
				return verifyController(&state)
			},
		},
	}
	result := make(chan error, 1)
	go func() { result <- server.ServeTLS(listener, "", "") }()
	shutdown := func() error {
		stopWatching()
		stopDNS()
		stopProxy()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			_ = server.Close()
		}
		serveErr := <-result
		if shutdownErr != nil {
			return fmt.Errorf("stop Linux helper: %w", shutdownErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("serve Linux helper: %w", serveErr)
		}
		return nil
	}
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve Linux helper: %w", err)
	case err := <-watchResult:
		watchJoined = true
		if err == nil && ctx.Err() != nil {
			return shutdown()
		}
		stopWatching()
		_ = server.Close()
		<-result
		if err != nil {
			return fmt.Errorf("watch Linux engine events: %w", err)
		}
		return errors.New("Linux engine event watcher stopped unexpectedly")
	case err := <-dnsResult:
		dnsJoined = true
		if err == nil && ctx.Err() != nil {
			return shutdown()
		}
		stopWatching()
		_ = server.Close()
		<-result
		if err != nil {
			return fmt.Errorf("serve Linux container DNS: %w", err)
		}
		return errors.New("Linux container DNS service stopped unexpectedly")
	case err := <-proxyResult:
		proxyJoined = true
		if err == nil && ctx.Err() != nil {
			return shutdown()
		}
		stopWatching()
		stopDNS()
		_ = server.Close()
		<-result
		if err != nil {
			return fmt.Errorf("serve Linux Docker proxy: %w", err)
		}
		return errors.New("Linux Docker proxy service stopped unexpectedly")
	case <-ctx.Done():
		return shutdown()
	}
}

func verifyServerCertificate(cert tls.Certificate, roots *x509.CertPool) error {
	if len(cert.Certificate) == 0 {
		return errors.New("server certificate is missing")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		ca, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		intermediates.AddCert(ca)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: "localhost", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}
