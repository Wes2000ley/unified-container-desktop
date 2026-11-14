package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

type callerFunc func(context.Context, api.Request) (any, error)

func (f callerFunc) Call(ctx context.Context, r api.Request) (any, error) { return f(ctx, r) }

func TestEndpointSecurityAndAssets(t *testing.T) {
	called := 0
	h, err := NewHandler(callerFunc(func(_ context.Context, r api.Request) (any, error) {
		called++
		return map[string]string{"method": r.Method}, nil
	}), "127.0.0.1:1234", "secret")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, host, origin, auth, contentType, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:1234"+path, strings.NewReader(body))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	host, origin := "127.0.0.1:1234", "http://127.0.0.1:1234"
	for _, tc := range []struct {
		name, host, origin, auth, ct string
		status                       int
	}{
		{"host", "localhost:1234", origin, "Bearer secret", "application/json", 403},
		{"origin", host, "http://evil.test", "Bearer secret", "application/json", 403},
		{"missing origin", host, "", "Bearer secret", "application/json", 403},
		{"token", host, origin, "Bearer incorrect", "application/json", 401},
		{"content type", host, origin, "Bearer secret", "text/plain", 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request("POST", "/v1/call", tc.host, tc.origin, tc.auth, tc.ct, `{"method":"engine.list"}`)
			if w.Code != tc.status {
				t.Fatalf("got %d", w.Code)
			}
		})
	}
	if called != 0 {
		t.Fatal("unauthorized request reached caller")
	}
	w := request("POST", "/v1/call", host, origin, "Bearer secret", "application/json", `{"method":"engine.list"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"method":"engine.list"`) {
		t.Fatalf("response %d %s", w.Code, w.Body.String())
	}
	w = request("GET", "/", host, "", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<meta name="ucd-transport" content="web">`) {
		t.Fatalf("index %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing security headers")
	}
	w = request("GET", "/assets/does-not-exist.js", host, "", "", "", "")
	if w.Code != 404 {
		t.Fatalf("missing asset status %d", w.Code)
	}
	w = request("POST", "/v1/call", host, origin, "Bearer secret", "application/json", `{"method":"x"} {}`)
	if w.Code != 400 {
		t.Fatalf("multiple JSON status %d", w.Code)
	}
	r := httptest.NewRequest("POST", origin+"/v1/call", nil)
	r.Host = host
	r.Header.Set("Origin", origin)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	r.ContentLength = maxBodyBytes + 1
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("oversized status %d", w.Code)
	}
}

func TestErrorsAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		call               callerFunc
		wantCode, contains string
		status             int
	}{
		{"typed", func(context.Context, api.Request) (any, error) { return nil, api.Fail("permission_denied", "denied") }, "permission_denied", "denied", 200},
		{"connection", func(context.Context, api.Request) (any, error) { return nil, errors.New("pipe unavailable") }, "internal", "verify the service is installed and running", 500},
		{"encoding", func(context.Context, api.Request) (any, error) { return make(chan int), nil }, "internal", "encode service response", 500},
		{"oversized response", func(context.Context, api.Request) (any, error) { return strings.Repeat("x", maxBodyBytes), nil }, "internal", "response exceeds", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHandler(tc.call, "127.0.0.1:1234", "secret")
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "http://127.0.0.1:1234/v1/call", strings.NewReader(`{"method":"x"}`))
			r.Header.Set("Origin", "http://127.0.0.1:1234")
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var response api.Response
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != tc.status || response.Error == nil || response.Error.Code != tc.wantCode || !strings.Contains(response.Error.Message, tc.contains) {
				t.Fatalf("%d %s %v", w.Code, w.Body.String(), err)
			}
		})
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	h, _ := NewHandler(callerFunc(func(ctx context.Context, _ api.Request) (any, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}), "127.0.0.1:1234", "secret")
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234/v1/call", strings.NewReader(`{"method":"x"}`)).WithContext(ctx)
	r.Header.Set("Origin", "http://127.0.0.1:1234")
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not cancel")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("caller did not receive cancellation")
	}
}

func TestRunLaunchAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	// Run output is observed only after shutdown to avoid concurrent buffer reads.
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, callerFunc(func(context.Context, api.Request) (any, error) { return nil, nil }), 0, &output)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "http://127.0.0.1:") || !strings.Contains(output.String(), "/#") {
		t.Fatalf("invalid URL %q", output.String())
	}
	if err := Run(context.Background(), nil, -1, io.Discard); err == nil {
		t.Fatal("negative port accepted")
	}
}

func TestLongRunningCallsDoNotBlockControlRequests(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	defer close(release)
	h, err := NewHandler(callerFunc(func(ctx context.Context, req api.Request) (any, error) {
		switch req.Method {
		case "transfer.read", "transfer.finish", "container.logs.read", "project.logs.read":
			started <- struct{}{}
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
			return "ok", nil
		}
	}), "127.0.0.1:1234", "secret")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:1234/v1/call", strings.NewReader(fmt.Sprintf(`{"method":%q}`, method)))
		r.Header.Set("Origin", "http://127.0.0.1:1234")
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		return response.Code
	}
	for _, method := range []string{"transfer.read", "transfer.finish", "container.logs.read", "project.logs.read"} {
		go request(method)
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("long-running requests did not start")
		}
	}
	for _, method := range []string{"transfer.close", "engine.list"} {
		result := make(chan int, 1)
		go func() { result <- request(method) }()
		select {
		case status := <-result:
			if status != 200 {
				t.Fatalf("%s returned %d", method, status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked by long-running calls", method)
		}
	}
}
