// Package helper implements the mutually authenticated controller-to-Linux transport.
package helper

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
)

// Client sends Linux engine requests over mutually authenticated TLS. It is safe
// for concurrent use. Call Close when the client is replaced or no longer used.
type Client struct {
	url       string
	dockerURL *url.URL
	client    *http.Client
	transport *http.Transport
}

var _ api.Caller = (*Client)(nil)

const (
	helperReadyTimeout     = 30 * time.Second
	helperReadyInterval    = 250 * time.Millisecond
	helperReadyCallTimeout = 2 * time.Second
)

// NewClient validates the helper URL and controller credentials before use.
func NewClient(settings api.Settings) (*Client, error) {
	u, err := url.Parse(settings.HelperURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("helper URL must be an HTTPS origin without a path, credentials, query, or fragment")
	}
	roots, err := loadCA(settings.CACert)
	if err != nil {
		return nil, fmt.Errorf("load helper CA: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(settings.ClientCert, settings.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load controller certificate and key: %w", err)
	}
	if err := verifyClientCertificate(cert, roots); err != nil {
		return nil, fmt.Errorf("verify controller certificate: %w", err)
	}
	t := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}},
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: 0, // Compose operations can take an arbitrary amount of time.
	}
	client := &http.Client{
		Transport: t,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("helper redirects are not permitted")
		},
	}
	origin := strings.TrimSuffix(u.String(), "/")
	dockerURL, err := url.Parse(origin + "/docker")
	if err != nil {
		return nil, fmt.Errorf("create helper Docker URL: %w", err)
	}
	return &Client{url: origin + "/v1/call", dockerURL: dockerURL, client: client, transport: t}, nil
}

// DockerHandler forwards the Docker HTTP API through the authenticated helper.
// Its streams are canceled when the incoming request context ends.
func (c *Client) DockerHandler() http.Handler {
	return dockerproxy.New(c.dockerURL, c.transport)
}

// Call executes one request. The caller controls cancellation and operation
// deadlines through ctx. A successful result is a json.RawMessage.
func (c *Client) Call(ctx context.Context, request api.Request) (any, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode helper request: %w", err)
	}
	if len(body) > maxRequestBytes {
		return nil, api.Fail("invalid_argument", "helper request exceeds 80 MiB")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create helper request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Linux helper: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read helper response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("helper response exceeds 80 MiB")
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *api.Error      `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("decode helper response (HTTP %d): %w", resp.StatusCode, err)
	}
	if envelope.Error != nil {
		return nil, envelope.Error
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("helper returned HTTP %d without an error", resp.StatusCode)
	}
	if len(envelope.Result) == 0 {
		return json.RawMessage("null"), nil
	}
	return envelope.Result, nil
}

// WaitReady waits until the authenticated Linux helper accepts a local event
// request or the caller is canceled. It bounds both the overall wait and each
// individual request.
func (c *Client) WaitReady(ctx context.Context) error {
	return waitForReady(ctx, helperReadyTimeout, func(ctx context.Context) error {
		_, err := c.Call(ctx, api.Request{Method: "system.events"})
		return err
	})
}

func waitForReady(ctx context.Context, timeout time.Duration, probe func(context.Context) error) error {
	if ctx == nil {
		return errors.New("helper readiness requires a context")
	}
	if probe == nil {
		return errors.New("helper readiness requires a probe")
	}
	if timeout <= 0 {
		return errors.New("helper readiness timeout must be positive")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(helperReadyInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		probeCtx, cancelProbe := context.WithTimeout(waitCtx, helperReadyCallTimeout)
		lastErr = probe(probeCtx)
		cancelProbe()
		if err := ctx.Err(); err != nil {
			return err
		}
		if lastErr == nil {
			return nil
		}
		if waitCtx.Err() != nil {
			return fmt.Errorf("Linux helper did not become ready within %s: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("Linux helper did not become ready within %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}

// Close releases idle helper connections.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

func loadCA(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file does not contain a PEM certificate")
	}
	return roots, nil
}

func verifyClientCertificate(cert tls.Certificate, roots *x509.CertPool) error {
	if len(cert.Certificate) == 0 {
		return errors.New("controller certificate is missing")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	if leaf.Subject.CommonName != controllerName {
		return fmt.Errorf("controller certificate common name must be %q", controllerName)
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		ca, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		intermediates.AddCert(ca)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err
}
