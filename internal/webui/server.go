// Package webui serves the embedded desktop frontend to a local browser.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skyri/ucd/frontend"
	"github.com/skyri/ucd/internal/api"
)

const maxBodyBytes = 80 << 20

// Run starts a foreground server on the IPv4 loopback interface until ctx ends.
func Run(ctx context.Context, caller api.Caller, port int, output io.Writer) error {
	if port < 0 || port > 65535 {
		return errors.New("port must be between 0 and 65535")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	defer listener.Close()
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return fmt.Errorf("generate browser token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	host := listener.Addr().String()
	handler, err := NewHandler(caller, host, token)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	if _, err := fmt.Fprintf(output, "http://%s/#%s\n", host, token); err != nil {
		return fmt.Errorf("write launch URL: %w", err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve browser UI: %w", err)
}

// NewHandler creates the browser endpoint for an exact host and bearer token.
// The caller supplies the authenticated local service connection.
func NewHandler(caller api.Caller, host, token string) (http.Handler, error) {
	assets, err := fs.Sub(frontend.Assets, "dist")
	if err != nil {
		return nil, fmt.Errorf("open frontend assets: %w", err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read frontend index: %w", err)
	}
	if !strings.Contains(string(index), "</head>") {
		return nil, errors.New("frontend index has no closing head tag")
	}
	index = []byte(strings.Replace(string(index), "</head>", `<meta name="ucd-transport" content="web"></head>`, 1))
	files := http.FileServer(http.FS(assets))
	origin := "http://" + host
	decodes := make(chan struct{}, 4)
	downloads := newDownloads(caller)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		if r.Host != host || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != origin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/call" || r.URL.Path == "/v1/download" {
			h.Set("Content-Type", "application/json")
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, api.Fail("invalid_argument", "POST required"))
				return
			}
			if r.Header.Get("Origin") != origin {
				writeError(w, http.StatusForbidden, api.Fail("permission_denied", "same origin required"))
				return
			}
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(token)) != 1 {
				writeError(w, http.StatusUnauthorized, api.Fail("unauthenticated", "valid bearer token required"))
				return
			}
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, api.Fail("invalid_argument", "application/json required"))
				return
			}
			if r.URL.Path == "/v1/download" {
				downloads.prepare(w, r)
				return
			}
			if r.ContentLength > maxBodyBytes {
				writeError(w, http.StatusRequestEntityTooLarge, api.Fail("request_too_large", "request exceeds 80 MiB"))
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
			defer cancel()
			select {
			case decodes <- struct{}{}:
			case <-ctx.Done():
				return
			}
			var request api.Request
			decodeErr := func() error {
				defer func() { <-decodes }()
				r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
				dec := json.NewDecoder(r.Body)
				dec.DisallowUnknownFields()
				if err := dec.Decode(&request); err != nil {
					return err
				}
				err := dec.Decode(&struct{}{})
				if err == io.EOF {
					return nil
				}
				if err == nil {
					return errors.New("multiple JSON values")
				}
				return err
			}()
			if decodeErr != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(decodeErr, &tooLarge) {
					writeError(w, http.StatusRequestEntityTooLarge, api.Fail("request_too_large", "request exceeds 80 MiB"))
				} else {
					writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "request must contain one valid JSON object"))
				}
				return
			}
			if request.Method == "" || (request.Engine != "" && !api.ValidEngine(request.Engine)) {
				writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "method is required and engine must be linux or windows"))
				return
			}
			result, err := caller.Call(ctx, request)
			if err != nil {
				var serviceError *api.Error
				if errors.As(err, &serviceError) {
					writeError(w, http.StatusOK, serviceError)
					return
				}
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					err = fmt.Errorf("call the Unified Container Desktop service over its local named pipe: %w; verify the service is installed and running", err)
				}
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			body, err := json.Marshal(api.Response{Result: result})
			if err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("encode service response: %w", err))
				return
			}
			if len(body) > maxBodyBytes {
				writeError(w, http.StatusInternalServerError, errors.New("service response exceeds 80 MiB"))
				return
			}
			_, _ = w.Write(body)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/download/") {
			downloads.serve(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			h.Set("Content-Type", "text/html; charset=utf-8")
			if r.Method == http.MethodGet {
				_, _ = w.Write(index)
			}
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}

func writeError(w http.ResponseWriter, status int, err error) {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		apiErr = &api.Error{Code: apiErr.Code, Message: err.Error()}
	} else {
		apiErr = &api.Error{Code: "internal", Message: err.Error()}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Response{Error: apiErr})
}
