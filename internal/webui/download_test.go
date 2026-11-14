package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

type downloadCaller struct {
	mu        sync.Mutex
	remaining int
	reads     int
	closes    int
	failAt    int
	blockAt   int
	started   chan struct{}
	canceled  chan struct{}
	closed    chan struct{}
}

func (c *downloadCaller) Call(ctx context.Context, request api.Request) (any, error) {
	if request.Method == "engine.list" {
		return "available", nil
	}
	params, err := api.Decode[api.TransferParams](request)
	if err != nil || request.Engine != "linux" || params.SessionID != "download-session" {
		return nil, errors.New("incorrect transfer request")
	}
	c.mu.Lock()
	switch request.Method {
	case "transfer.close":
		c.closes++
		if c.closed != nil && c.closes == 1 {
			close(c.closed)
		}
		c.mu.Unlock()
		return nil, nil
	case "transfer.read":
		if params.MaxBytes != 256<<10 {
			c.mu.Unlock()
			return nil, errors.New("unexpected chunk size")
		}
		c.reads++
		if c.failAt == c.reads {
			c.mu.Unlock()
			return nil, errors.New("source stream failed")
		}
		if c.blockAt == c.reads {
			close(c.started)
			c.mu.Unlock()
			<-ctx.Done()
			close(c.canceled)
			return nil, ctx.Err()
		}
		n := min(params.MaxBytes, c.remaining)
		c.remaining -= n
		eof := c.remaining == 0
		c.mu.Unlock()
		return api.TransferReadResult{Data: bytes.Repeat([]byte("x"), n), EOF: eof}, nil
	default:
		c.mu.Unlock()
		return nil, errors.New("unexpected method")
	}
}

func (c *downloadCaller) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.closes
}

func testDownloadServer(t *testing.T, caller api.Caller) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHandler(caller, server.Listener.Addr().String(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func prepareTestDownload(t *testing.T, server *httptest.Server) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/download", strings.NewReader(`{"engine":"linux","sessionID":"download-session","name":"../unsafe\r\nbad.tar"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", server.URL)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(result.URL, "/v1/download/") {
		t.Fatalf("prepare = %d %+v, %v", resp.StatusCode, result, err)
	}
	return result.URL
}

func TestBrowserDownloadStreamsLargeFileOnce(t *testing.T) {
	const size = 9<<20 + 123
	caller := &downloadCaller{remaining: size}
	server := testDownloadServer(t, caller)
	url := prepareTestDownload(t, server)
	resp, err := http.Get(server.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil || mediaType != "attachment" || params["filename"] != "unsafe__bad.tar" {
		t.Fatalf("content disposition = %q, %v", resp.Header.Get("Content-Disposition"), err)
	}
	hash := sha256.New()
	n, err := io.Copy(hash, resp.Body)
	resp.Body.Close()
	want := sha256.Sum256(bytes.Repeat([]byte("x"), size))
	if err != nil || resp.StatusCode != http.StatusOK || n != size || !bytes.Equal(hash.Sum(nil), want[:]) {
		t.Fatalf("download = %d bytes, HTTP %d, %v", n, resp.StatusCode, err)
	}
	resp, err = http.Get(server.URL + url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	reads, closes := caller.counts()
	if resp.StatusCode != http.StatusNotFound || closes != 1 || reads < 2 {
		t.Fatalf("reuse = %d, closes %d, reads %d", resp.StatusCode, closes, reads)
	}
}

func TestBrowserDownloadPreparationSecurity(t *testing.T) {
	caller := &downloadCaller{}
	handler, err := NewHandler(caller, "127.0.0.1:1234", "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, host, origin, auth, contentType, body string
		status                                      int
	}{
		{"host", "localhost:1234", "http://127.0.0.1:1234", "Bearer secret", "application/json", `{"engine":"linux","sessionID":"x"}`, 403},
		{"missing origin", "127.0.0.1:1234", "", "Bearer secret", "application/json", `{"engine":"linux","sessionID":"x"}`, 403},
		{"other origin", "127.0.0.1:1234", "http://evil.test", "Bearer secret", "application/json", `{"engine":"linux","sessionID":"x"}`, 403},
		{"bearer", "127.0.0.1:1234", "http://127.0.0.1:1234", "Bearer wrong", "application/json", `{"engine":"linux","sessionID":"x"}`, 401},
		{"content type", "127.0.0.1:1234", "http://127.0.0.1:1234", "Bearer secret", "text/plain", `{"engine":"linux","sessionID":"x"}`, 415},
		{"invalid engine", "127.0.0.1:1234", "http://127.0.0.1:1234", "Bearer secret", "application/json", `{"engine":"elsewhere","sessionID":"x"}`, 400},
		{"unknown field", "127.0.0.1:1234", "http://127.0.0.1:1234", "Bearer secret", "application/json", `{"engine":"linux","sessionID":"x","method":"anything"}`, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234/v1/download", strings.NewReader(test.body))
			req.Host = test.host
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Authorization", test.auth)
			req.Header.Set("Content-Type", test.contentType)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != test.status {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}
	if reads, closes := caller.counts(); reads != 0 || closes != 0 {
		t.Fatal("rejected request reached the backend")
	}
}

func TestBrowserDownloadRejectsCrossOriginWithoutConsumingTicket(t *testing.T) {
	caller := &downloadCaller{remaining: 3}
	handler, err := NewHandler(caller, "127.0.0.1:1234", "secret")
	if err != nil {
		t.Fatal(err)
	}
	prepare := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1234/v1/download", strings.NewReader(`{"engine":"linux","sessionID":"download-session","name":"test.tar"}`))
	prepare.Header.Set("Origin", "http://127.0.0.1:1234")
	prepare.Header.Set("Authorization", "Bearer secret")
	prepare.Header.Set("Content-Type", "application/json")
	prepared := httptest.NewRecorder()
	handler.ServeHTTP(prepared, prepare)
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(prepared.Body.Bytes(), &result); err != nil || prepared.Code != http.StatusOK {
		t.Fatalf("prepare = %d %s, %v", prepared.Code, prepared.Body, err)
	}
	for _, test := range []struct {
		name, host, origin, site string
	}{
		{"host", "localhost:1234", "", ""},
		{"origin", "127.0.0.1:1234", "http://evil.test", ""},
		{"fetch site", "127.0.0.1:1234", "", "cross-site"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234"+result.URL, nil)
			req.Host = test.host
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Sec-Fetch-Site", test.site)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1234"+result.URL, nil))
	if response.Code != http.StatusOK || response.Body.String() != "xxx" {
		t.Fatalf("valid request = %d %q", response.Code, response.Body.String())
	}
	if reads, closes := caller.counts(); reads != 1 || closes != 1 {
		t.Fatalf("reads = %d, closes = %d", reads, closes)
	}
}

func TestBrowserDownloadExpiresAndClosesSession(t *testing.T) {
	caller := &downloadCaller{}
	downloads := newDownloads(caller)
	req := httptest.NewRequest(http.MethodPost, "/v1/download", strings.NewReader(`{"engine":"linux","sessionID":"download-session","name":"test.tar"}`))
	resp := httptest.NewRecorder()
	downloads.prepare(resp, req)
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil || resp.Code != http.StatusOK {
		t.Fatalf("prepare = %d %s, %v", resp.Code, resp.Body, err)
	}
	token := strings.TrimPrefix(result.URL, "/v1/download/")
	downloads.mu.Lock()
	ticket := downloads.pending[token]
	downloads.mu.Unlock()
	downloads.expire(token, ticket)
	_, closes := caller.counts()
	if closes != 1 || downloads.take(token) != nil {
		t.Fatalf("expired session closes = %d", closes)
	}
}

func TestBrowserDownloadCancelAndErrors(t *testing.T) {
	t.Run("source failure before first byte preserves error", func(t *testing.T) {
		caller := &downloadCaller{remaining: 1 << 20, failAt: 1}
		server := testDownloadServer(t, caller)
		url := prepareTestDownload(t, server)
		resp, err := http.Get(server.URL + url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		_, closes := caller.counts()
		if err != nil || resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "source stream failed") || closes != 1 {
			t.Fatalf("response = HTTP %d %s, %v, closes %d", resp.StatusCode, body, err, closes)
		}
	})
	t.Run("disconnect cancels read and closes session", func(t *testing.T) {
		caller := &downloadCaller{remaining: 1 << 20, blockAt: 2, started: make(chan struct{}), canceled: make(chan struct{}), closed: make(chan struct{})}
		server := testDownloadServer(t, caller)
		url := prepareTestDownload(t, server)
		resp, err := http.Get(server.URL + url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if _, err := io.CopyN(io.Discard, resp.Body, 256<<10); err != nil {
			t.Fatal(err)
		}
		select {
		case <-caller.started:
		case <-time.After(5 * time.Second):
			t.Fatal("stream did not begin second read")
		}
		// The long download must not occupy the ordinary API semaphore.
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/call", strings.NewReader(`{"method":"engine.list"}`))
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		apiResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil || apiResp.StatusCode != http.StatusOK {
			t.Fatalf("ordinary API call during download: %v", err)
		}
		apiResp.Body.Close()
		resp.Body.Close()
		select {
		case <-caller.canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("disconnect did not cancel read")
		}
		select {
		case <-caller.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("disconnect did not close session")
		}
	})
	t.Run("source failure aborts partial response", func(t *testing.T) {
		caller := &downloadCaller{remaining: 1 << 20, failAt: 2}
		server := testDownloadServer(t, caller)
		url := prepareTestDownload(t, server)
		resp, err := http.Get(server.URL + url)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		_, closes := caller.counts()
		if err == nil || len(data) != 256<<10 || closes != 1 {
			t.Fatalf("partial download = %d bytes, %v, closes %d", len(data), err, closes)
		}
	})
}
