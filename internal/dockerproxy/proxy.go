// Package dockerproxy forwards Docker's HTTP API without buffering its streams.
package dockerproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
)

const (
	// WindowsEndpoint is the native Windows Docker daemon's named pipe.
	WindowsEndpoint = `npipe:////./pipe/UCDDocker`
	// LinuxEndpoint is the controller's authenticated Linux Docker API pipe.
	LinuxEndpoint = `npipe:////./pipe/UCDLinuxDocker`
	// LinuxPipe is the Windows path corresponding to LinuxEndpoint.
	LinuxPipe = `\\.\pipe\UCDLinuxDocker`
)

// New forwards Docker API requests to target using transport. A target path is
// prepended to each incoming path; escaped paths, queries, streaming bodies,
// and HTTP upgrades are retained.
func New(target *url.URL, transport http.RoundTripper) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			// Docker owns interpretation of its API query; do not normalize it.
			p.Out.URL.RawQuery = p.In.URL.RawQuery
		},
		Transport:     transport,
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode != http.StatusSwitchingProtocols {
				return nil
			}
			body, ok := response.Body.(io.ReadWriteCloser)
			if !ok {
				return errors.New("upgraded Docker response is not a read-write stream")
			}
			response.Body = upgradeBody{ReadWriteCloser: body}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			WriteError(w, http.StatusBadGateway, "Docker engine connection failed: "+err.Error())
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Docker can send build progress while the request body is still being
		// uploaded. HTTP/1 servers otherwise drain the body before writing.
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			WriteError(w, http.StatusBadGateway, "enable Docker stream: "+err.Error())
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

type upgradeBody struct {
	io.ReadWriteCloser
}

func (b upgradeBody) CloseWrite() error {
	closer, ok := b.ReadWriteCloser.(interface{ CloseWrite() error })
	if !ok {
		return errors.New("upgraded Docker response does not support half-close")
	}
	if err := closer.CloseWrite(); err != nil && !isClosedStreamError(err) {
		return fmt.Errorf("half-close upgraded Docker stream: %w", err)
	}
	// Exec can finish and close its daemon connection before the client's
	// independent stdin copier propagates EOF. In that case the write half is
	// already gone; let ReverseProxy continue draining the daemon's output half.
	return nil
}

// WriteError returns a Docker-compatible JSON error response.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Message string `json:"message"`
	}{Message: message})
}
