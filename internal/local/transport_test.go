package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

type callerFunc func(context.Context, api.Request) (any, error)

func (f callerFunc) Call(ctx context.Context, req api.Request) (any, error) {
	return f(ctx, req)
}

func TestHandlerAuthorizesEachMethod(t *testing.T) {
	tests := []struct {
		role   string
		method string
		status int
	}{
		{"viewer", "engine.list", http.StatusOK},
		{"viewer", "project.import", http.StatusForbidden},
		{"operator", "project.import", http.StatusOK},
		{"operator", "engine.start", http.StatusForbidden},
		{"administrator", "engine.start", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.role+"/"+tt.method, func(t *testing.T) {
			principal := api.Principal{SID: "S-1-5-21-test", Name: "tester", Role: tt.role}
			called := false
			caller := callerFunc(func(ctx context.Context, req api.Request) (any, error) {
				called = true
				if got := api.CurrentPrincipal(ctx); got != principal {
					t.Errorf("caller principal = %+v, want %+v", got, principal)
				}
				return map[string]string{"method": req.Method}, nil
			})
			h := handler(caller, func(context.Context) (api.Principal, error) { return principal, nil })
			r := httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(fmt.Sprintf(`{"method":%q}`, tt.method)))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if called != (tt.status == http.StatusOK) {
				t.Errorf("caller invoked = %v", called)
			}
			if tt.status != http.StatusOK {
				var response struct {
					Error *api.Error `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error == nil || response.Error.Code != "permission_denied" {
					t.Fatalf("expected permission_denied response, got %s (%v)", w.Body.String(), err)
				}
			}
		})
	}
}

func TestHandlerRejectsUntrustedIdentityAndInvalidBodies(t *testing.T) {
	called := false
	h := handler(callerFunc(func(context.Context, api.Request) (any, error) {
		called = true
		return nil, nil
	}), func(context.Context) (api.Principal, error) {
		return api.Principal{SID: "S-1-5-21-test", Role: "viewer"}, nil
	})
	tests := []struct {
		body string
		want string
	}{
		{`{"method":"engine.start","role":"administrator"}`, "invalid_argument"},
		{`{"method":"engine.start","principal":{"sid":"S-1-5-18","role":"administrator"}}`, "invalid_argument"},
		{`{"method":"engine.list"} {"method":"engine.start"}`, "invalid_argument"},
		{`{"params":{}}`, "invalid_argument"},
		{`{`, "invalid_argument"},
	}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(tt.body)))
		var response struct {
			Error *api.Error `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusBadRequest || response.Error == nil || response.Error.Code != tt.want {
			t.Errorf("body %s: status %d, response %s, parse error %v", tt.body, w.Code, w.Body.String(), err)
		}
	}
	if called {
		t.Fatal("caller invoked for rejected request")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(`{}`))
	req.ContentLength = maxBodyBytes + 1
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d", w.Code)
	}
}

func TestHandlerRejectsPrincipalWithoutSID(t *testing.T) {
	h := handler(callerFunc(func(context.Context, api.Request) (any, error) {
		t.Fatal("unauthenticated request reached the caller")
		return nil, nil
	}), func(context.Context) (api.Principal, error) {
		return api.Principal{Role: "administrator"}, nil
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(`{"method":"engine.list"}`)))
	var response struct {
		Error *api.Error `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusUnauthorized || response.Error == nil || response.Error.Code != "unauthenticated" {
		t.Fatalf("status %d, response %s, parse error %v", w.Code, w.Body.String(), err)
	}
}

func TestHandlerRequiresOperatorForEveryDockerRequest(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "administrator"} {
		t.Run(role, func(t *testing.T) {
			called := false
			docker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if p := api.CurrentPrincipal(r.Context()); p.Role != role {
					t.Errorf("Docker principal role = %q, want %q", p.Role, role)
				}
				_, _ = io.WriteString(w, "docker response")
			})
			principal := api.Principal{SID: "S-1-5-21-test", Role: role}
			h := handlerWithDocker(callerFunc(func(context.Context, api.Request) (any, error) {
				t.Fatal("Docker request reached the JSON controller")
				return nil, nil
			}), func(context.Context) (api.Principal, error) { return principal, nil }, docker)
			request := httptest.NewRequest(http.MethodGet, "/docker/windows/v1.45/version", nil)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if role == "viewer" {
				if called || response.Code != http.StatusForbidden {
					t.Fatalf("viewer Docker access called=%v status=%d", called, response.Code)
				}
				return
			}
			if !called || response.Code != http.StatusOK || response.Body.String() != "docker response" {
				t.Fatalf("Docker request called=%v status=%d body=%q", called, response.Code, response.Body.String())
			}
		})
	}
}

func TestEngineRouterPreservesStreamsAndRequestURI(t *testing.T) {
	for _, engine := range []string{"windows", "linux"} {
		t.Run(engine, func(t *testing.T) {
			got := make(chan string, 1)
			target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read Docker request body: %v", err)
				}
				got <- r.Method + " " + r.URL.RequestURI() + " " + string(body)
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, "response stream")
			})
			var windowsDocker, linuxDocker http.Handler
			if engine == "windows" {
				windowsDocker = target
			} else {
				linuxDocker = target
			}
			router := NewEngineRouter(windowsDocker, linuxDocker)
			request := httptest.NewRequest(http.MethodPost, "/docker/"+engine+"/v1.45/images/create?fromImage=alpine%3Alatest", strings.NewReader("streamed request"))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := "POST /v1.45/images/create?fromImage=alpine%3Alatest streamed request"
			if observed := <-got; observed != want {
				t.Fatalf("forwarded request = %q, want %q", observed, want)
			}
			if response.Code != http.StatusAccepted || response.Header().Get("Content-Type") != "application/octet-stream" || response.Body.String() != "response stream" {
				t.Fatalf("Docker response = %d %q %q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
			}
		})
	}
}

func TestEngineRouterRejectsUnknownEngine(t *testing.T) {
	router := NewEngineRouter(http.NotFoundHandler(), http.NotFoundHandler())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/docker/other/version", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown engine status = %d", response.Code)
	}
}

func TestHandlerReturnsCallerAndEncodingErrors(t *testing.T) {
	principal := func(context.Context) (api.Principal, error) {
		return api.Principal{SID: "S-1-5-21-test", Role: "administrator"}, nil
	}
	for _, test := range []struct {
		name   string
		caller callerFunc
		status int
		code   string
	}{
		{"api error", func(context.Context, api.Request) (any, error) { return nil, api.Fail("not_found", "missing") }, http.StatusNotFound, "not_found"},
		{"internal error", func(context.Context, api.Request) (any, error) { return nil, errors.New("failed") }, http.StatusInternalServerError, "internal"},
		{"encoding error", func(context.Context, api.Request) (any, error) { return make(chan int), nil }, http.StatusInternalServerError, "internal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler(test.caller, principal).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(`{"method":"engine.list"}`)))
			var response struct {
				Error *api.Error `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != test.status || response.Error == nil || response.Error.Code != test.code {
				t.Fatalf("status %d, response %s, parse error %v", w.Code, w.Body.String(), err)
			}
		})
	}
}

func TestLongRunningCallsDoNotBlockControlRequests(t *testing.T) {
	started := make(chan string, 4)
	release := make(chan struct{})
	defer close(release)
	caller := callerFunc(func(ctx context.Context, req api.Request) (any, error) {
		switch req.Method {
		case "transfer.read", "transfer.finish", "container.logs.read", "project.logs.read":
			started <- req.Method
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
			return "ok", nil
		}
	})
	h := handler(caller, func(context.Context) (api.Principal, error) {
		return api.Principal{SID: "S-1-5-21-test", Role: "operator"}, nil
	})
	for _, method := range []string{"transfer.read", "transfer.finish", "container.logs.read", "project.logs.read"} {
		go func(method string) {
			request := httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(fmt.Sprintf(`{"method":%q}`, method)))
			h.ServeHTTP(httptest.NewRecorder(), request)
		}(method)
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
		go func() {
			request := httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader(fmt.Sprintf(`{"method":%q}`, method)))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			result <- response.Code
		}()
		select {
		case status := <-result:
			if status != http.StatusOK {
				t.Fatalf("%s returned %d", method, status)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked by long-running calls", method)
		}
	}
}
