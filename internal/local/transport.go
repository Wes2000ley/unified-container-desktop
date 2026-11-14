// Package local provides the authenticated local controller transport.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
)

const maxBodyBytes = 80 << 20

// Client calls the controller over the local Windows named pipe. It is safe
// for concurrent use. Each call uses a new connection and Windows identity.
type Client struct {
	http      *http.Client
	transport *http.Transport
	err       error
}

// DockerRelay represents a short-lived local named pipe used only by the
// bundled Docker CLI. Its HTTP server and pipe are closed when the command ends.
type DockerRelay struct {
	endpoint string
	server   *http.Server
	done     chan error
	once     sync.Once
	err      error
}

// Endpoint is the Docker host URI for the relay pipe.
func (r *DockerRelay) Endpoint() string { return r.endpoint }

// Close shuts down the local pipe server and waits for its listener goroutine.
func (r *DockerRelay) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r.err = r.server.Shutdown(ctx)
		if r.err != nil {
			_ = r.server.Close()
		}
		serveErr := <-r.done
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			r.err = errors.Join(r.err, serveErr)
		}
	})
	return r.err
}

// NewClient creates a client for the controller's named pipe.
func NewClient() *Client { return newClient(api.PipeName) }

// Call invokes a controller method. Successful results are json.RawMessage so
// callers can either decode them or marshal them without double encoding.
func (c *Client) Call(ctx context.Context, request api.Request) (any, error) {
	if c.err != nil {
		return nil, c.err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode local request: %w", err)
	}
	if len(body) > maxBodyBytes {
		return nil, api.Fail("request_too_large", "local request exceeds 80 MiB")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ucd/v1/call", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create local request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call local controller: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read local response: %w", err)
	}
	if len(data) > maxBodyBytes {
		return nil, errors.New("local response exceeds 80 MiB")
	}
	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *api.Error      `json:"error"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode local response (HTTP %d): %w", resp.StatusCode, err)
	}
	if result.Error != nil {
		return nil, result.Error
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("local controller returned HTTP %d without an error", resp.StatusCode)
	}
	if len(result.Result) == 0 {
		result.Result = json.RawMessage("null")
	}
	return result.Result, nil
}

// CallInto invokes a controller method and decodes its JSON result into out.
func (c *Client) CallInto(ctx context.Context, request api.Request, out any) error {
	result, err := c.Call(ctx, request)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(result.(json.RawMessage), out); err != nil {
		return fmt.Errorf("decode local result: %w", err)
	}
	return nil
}

func (c *Client) dockerHandler(engine string) (http.Handler, error) {
	if !api.ValidEngine(engine) {
		return nil, api.Fail("invalid_argument", "engine must be windows or linux")
	}
	if c.err != nil {
		return nil, c.err
	}
	target := &url.URL{Scheme: "http", Host: "ucd", Path: "/docker/" + engine}
	return dockerproxy.New(target, c.transport), nil
}

// Close releases any idle transport connections. Calls already in progress are
// governed by their contexts.
func (c *Client) Close() error {
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return nil
}

type principalFunc func(context.Context) (api.Principal, error)

func handler(caller api.Caller, principal principalFunc) http.Handler {
	return handlerWithDocker(caller, principal, nil)
}

func handlerWithDocker(caller api.Caller, principal principalFunc, docker http.Handler) http.Handler {
	// Import bodies can be large; bound concurrent request decoding. Long-lived
	// stream reads must not hold a slot while other clients need control calls.
	decodes := make(chan struct{}, 4)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/docker/") {
			if docker == nil {
				writeError(w, http.StatusNotFound, api.Fail("not_found", "Docker API proxy is unavailable"))
				return
			}
			p, err := principal(r.Context())
			if err != nil {
				writeError(w, http.StatusUnauthorized, api.Fail("unauthenticated", fmt.Sprintf("could not authenticate Windows pipe client: %v", err)))
				return
			}
			if p.Role != "administrator" && p.Role != "operator" {
				dockerproxy.WriteError(w, http.StatusForbidden, "Container Operators membership is required for Docker API access")
				return
			}
			docker.ServeHTTP(w, r.WithContext(api.WithPrincipal(r.Context(), p)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		if r.URL.Path != "/v1/call" || r.Method != http.MethodPost {
			writeError(w, http.StatusNotFound, api.Fail("not_found", "local endpoint not found"))
			return
		}
		p, err := principal(r.Context())
		if err != nil {
			writeError(w, http.StatusUnauthorized, api.Fail("unauthenticated", fmt.Sprintf("could not authenticate Windows pipe client: %v", err)))
			return
		}
		if r.ContentLength > maxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, api.Fail("request_too_large", "local request exceeds 80 MiB"))
			return
		}
		select {
		case decodes <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		var request api.Request
		decoded := func() bool {
			defer func() { <-decodes }()
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&request); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeError(w, http.StatusRequestEntityTooLarge, api.Fail("request_too_large", "local request exceeds 80 MiB"))
				} else {
					writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "invalid local request JSON"))
				}
				return false
			}
			if err := dec.Decode(&struct{}{}); err != io.EOF {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeError(w, http.StatusRequestEntityTooLarge, api.Fail("request_too_large", "local request exceeds 80 MiB"))
				} else {
					writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "local request must contain one JSON object"))
				}
				return false
			}
			return true
		}()
		if !decoded {
			return
		}
		if request.Method == "" {
			writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "method is required"))
			return
		}
		if err := api.Authorize(p, request.Method); err != nil {
			status := http.StatusForbidden
			var apiErr *api.Error
			if errors.As(err, &apiErr) && apiErr.Code == "unauthenticated" {
				status = http.StatusUnauthorized
			}
			writeError(w, status, err)
			return
		}
		ctx, cancel := context.WithTimeout(api.WithPrincipal(r.Context(), p), 30*time.Minute)
		defer cancel()
		result, err := caller.Call(ctx, request)
		if err != nil {
			status := http.StatusInternalServerError
			var apiErr *api.Error
			if errors.As(err, &apiErr) {
				switch apiErr.Code {
				case "invalid_argument":
					status = http.StatusBadRequest
				case "unauthenticated":
					status = http.StatusUnauthorized
				case "permission_denied":
					status = http.StatusForbidden
				case "not_found":
					status = http.StatusNotFound
				}
			}
			writeError(w, status, err)
			return
		}
		body, err := json.Marshal(api.Response{Result: result})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("encode local result: %w", err))
			return
		}
		if len(body) > maxBodyBytes {
			writeError(w, http.StatusInternalServerError, errors.New("local response exceeds 80 MiB"))
			return
		}
		_, _ = w.Write(body)
	})
}

// NewEngineRouter selects one protected Docker handler by engine. It preserves
// streamed request/response bodies and upgraded exec sessions.
func NewEngineRouter(windowsDocker, linuxDocker http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/docker/windows" && r.URL.Path != "/docker/linux" &&
			!strings.HasPrefix(r.URL.Path, "/docker/windows/") && !strings.HasPrefix(r.URL.Path, "/docker/linux/") {
			dockerproxy.WriteError(w, http.StatusNotFound, "unknown Docker engine")
			return
		}
		engine := strings.TrimPrefix(r.URL.Path, "/docker/")
		if i := strings.IndexByte(engine, '/'); i >= 0 {
			engine = engine[:i]
		}
		target := windowsDocker
		if engine == "linux" {
			target = linuxDocker
		}
		if target == nil {
			dockerproxy.WriteError(w, http.StatusServiceUnavailable, engine+" Docker API is unavailable")
			return
		}
		forwarded := r.Clone(r.Context())
		forwarded.URL.Path = strings.TrimPrefix(r.URL.Path, "/docker/"+engine)
		if forwarded.URL.Path == "" {
			forwarded.URL.Path = "/"
		}
		if forwarded.URL.RawPath != "" {
			forwarded.URL.RawPath = strings.TrimPrefix(forwarded.URL.RawPath, "/docker/"+engine)
		}
		target.ServeHTTP(w, forwarded)
	})
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
