package helper

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skyri/ucd/internal/api"
)

type callerFunc func(context.Context, api.Request) (any, error)

func (f callerFunc) Call(ctx context.Context, request api.Request) (any, error) {
	return f(ctx, request)
}

type testAuthority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func makeAuthority(t *testing.T) testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testAuthority{certificate: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca testAuthority) issue(t *testing.T, name string, usage x509.ExtKeyUsage) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM, keyPEM
}

type testServer struct {
	server     *httptest.Server
	ca         testAuthority
	controller tls.Certificate
	settings   api.Settings
	dataRoot   string
}

func startTestServer(t *testing.T, backend api.Caller, events *api.EventBus, docker ...http.Handler) testServer {
	t.Helper()
	ca := makeAuthority(t)
	serverCert, _, _ := ca.issue(t, "ucd-helper", x509.ExtKeyUsageServerAuth)
	controller, controllerPEM, keyPEM := ca.issue(t, controllerName, x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca.certificate)
	dataRoot := t.TempDir()
	var handler http.Handler = NewHandler(backend, events, dataRoot)
	if len(docker) != 0 {
		handler = newHandler(backend, events, dataRoot, docker[0])
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyController(&state)
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	certDir := t.TempDir()
	settings := api.Settings{
		HelperURL:  server.URL,
		CACert:     filepath.Join(certDir, "ca.pem"),
		ClientCert: filepath.Join(certDir, "client.pem"),
		ClientKey:  filepath.Join(certDir, "client-key.pem"),
	}
	for path, data := range map[string][]byte{settings.CACert: ca.pem, settings.ClientCert: controllerPEM, settings.ClientKey: keyPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return testServer{server: server, ca: ca, controller: controller, settings: settings, dataRoot: dataRoot}
}

func (s testServer) httpClient(cert *tls.Certificate) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.certificate)
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	if cert != nil {
		config.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 10 * time.Second}
}

func TestMutualTLSAndClientValidation(t *testing.T) {
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		return request.Engine, nil
	})
	s := startTestServer(t, backend, &api.EventBus{})
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.Call(context.Background(), api.Request{Method: "engine.status"})
	if err != nil || string(result.(json.RawMessage)) != `"linux"` {
		t.Fatalf("valid authenticated call = %s, %v", result, err)
	}
	wrongName, _, _ := s.ca.issue(t, "someone-else", x509.ExtKeyUsageClientAuth)
	wrongUsage, _, _ := s.ca.issue(t, controllerName, x509.ExtKeyUsageServerAuth)
	otherCA := makeAuthority(t)
	untrusted, _, _ := otherCA.issue(t, controllerName, x509.ExtKeyUsageClientAuth)
	for name, cert := range map[string]*tls.Certificate{
		"missing certificate": nil, "unexpected subject": &wrongName,
		"wrong usage": &wrongUsage, "untrusted issuer": &untrusted,
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := s.httpClient(cert).Post(s.server.URL+"/v1/call", "application/json", strings.NewReader(`{"method":"engine.status"}`))
			if err == nil {
				resp.Body.Close()
				t.Fatal("unauthorized TLS client was accepted")
			}
		})
	}
	badURL := s.settings
	badURL.HelperURL = "http://localhost:7443"
	if _, err := NewClient(badURL); err == nil {
		t.Fatal("plaintext URL was accepted")
	}
	badCA := s.settings
	badCA.CACert = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := NewClient(badCA); err == nil {
		t.Fatal("missing CA was accepted")
	}
}

func TestClientWaitReadyUsesAuthenticatedHelper(t *testing.T) {
	s := startTestServer(t, nil, &api.EventBus{})
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.WaitReady(context.Background()); err != nil {
		t.Fatalf("authenticated helper readiness probe failed: %v", err)
	}
}

func TestWaitForReadyRetriesAndPreservesTimeoutCause(t *testing.T) {
	t.Run("retry until ready", func(t *testing.T) {
		calls := 0
		err := waitForReady(context.Background(), time.Second, func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("helper is still starting")
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("wait result = %v after %d calls", err, calls)
		}
	})

	t.Run("timeout includes last connection error", func(t *testing.T) {
		failure := errors.New("connection refused")
		err := waitForReady(context.Background(), 20*time.Millisecond, func(context.Context) error { return failure })
		if !errors.Is(err, failure) || !strings.Contains(err.Error(), "did not become ready") {
			t.Fatalf("wait error %v does not include timeout and probe cause", err)
		}
	})
}

func TestWaitForReadyReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := waitForReady(ctx, time.Second, func(context.Context) error {
		calls++
		cancel()
		return errors.New("connection refused")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("wait result = %v after %d calls, want cancellation after one probe", err, calls)
	}
}

func TestComposeTransferAndMethodAuthorization(t *testing.T) {
	var forwarded api.Request
	backend := callerFunc(func(_ context.Context, request api.Request) (any, error) {
		forwarded = request
		return "ok", nil
	})
	events := &api.EventBus{}
	events.Publish(api.Event{Type: "first"})
	events.Publish(api.Event{Type: "second"})
	s := startTestServer(t, backend, events)
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	content := []byte{0, 1, 2, 127, 128, 255}
	request, err := api.NewRequest("compose.import", "linux", api.ComposeParams{
		Project: api.ProjectPart{Name: "demo"}, YAML: "services: {}", Files: []api.File{{Path: "build/input.bin", Content: content, Mode: 0755}}, Replace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	params, err := api.Decode[api.ComposeParams](forwarded)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(s.dataRoot, "projects", "demo")
	if params.Project.Directory != wantDir || params.Project.ComposeFile != filepath.Join(wantDir, "compose.yaml") || params.Project.Engine != "linux" {
		t.Fatalf("unexpected managed paths: %+v", params.Project)
	}
	if !params.Replace || len(params.Files) != 1 || params.Files[0].Mode != 0755 || !bytes.Equal(params.Files[0].Content, content) {
		t.Fatalf("binary file changed during transfer: %+v", params.Files)
	}
	forwarded = api.Request{}
	_, err = client.Call(context.Background(), api.Request{Method: "system.configure"})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "permission_denied" || forwarded.Method != "" {
		t.Fatalf("disallowed method = %v; forwarded = %+v", err, forwarded)
	}
	if _, err := client.Call(context.Background(), api.Request{Method: "engine.configure"}); err != nil || forwarded.Method != "engine.configure" {
		t.Fatalf("engine configuration call = %v; forwarded = %+v", err, forwarded)
	}
	eventRequest, _ := api.NewRequest("system.events", "linux", api.EventsParams{After: 1, Epoch: events.Batch(0, "").Epoch})
	result, err := client.Call(context.Background(), eventRequest)
	if err != nil {
		t.Fatal(err)
	}
	var gotEvents api.EventBatch
	if err := json.Unmarshal(result.(json.RawMessage), &gotEvents); err != nil || len(gotEvents.Events) != 1 || gotEvents.Events[0].Type != "second" {
		t.Fatalf("events = %s, %v", result, err)
	}
	staleRequest, _ := api.NewRequest("system.events", "linux", api.EventsParams{After: 1000, Epoch: "previous-helper"})
	result, err = client.Call(context.Background(), staleRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.(json.RawMessage), &gotEvents); err != nil || len(gotEvents.Events) != 2 || gotEvents.Epoch == "previous-helper" {
		t.Fatalf("events after helper restart = %s, %v", result, err)
	}
}

func TestComposeLogSessionRequests(t *testing.T) {
	received := make(chan api.Request, 3)
	s := startTestServer(t, callerFunc(func(_ context.Context, request api.Request) (any, error) {
		received <- request
		return "ok", nil
	}), &api.EventBus{})
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	open, err := api.NewRequest("compose.logs.open", "linux", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Services: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	params, err := api.Decode[api.ComposeParams](<-received)
	if err != nil || params.Project.Directory != filepath.Join(s.dataRoot, "projects", "demo") || len(params.Services) != 1 || params.Services[0] != "web" {
		t.Fatalf("open parameters = %+v, %v", params, err)
	}
	for _, method := range []string{"compose.logs.read", "compose.logs.close"} {
		request, err := api.NewRequest(method, "linux", api.ExecParams{SessionID: "session-123"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Call(context.Background(), request); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		got, err := api.Decode[api.ExecParams](<-received)
		if err != nil || got.SessionID != "session-123" {
			t.Fatalf("%s parameters = %+v, %v", method, got, err)
		}
	}
}

func TestProjectPathRestrictions(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name   string
		params api.ComposeParams
	}{
		{"invalid name", api.ComposeParams{Project: api.ProjectPart{Name: "../demo"}}},
		{"outside directory", api.ComposeParams{Project: api.ProjectPart{Name: "demo", Directory: filepath.Join(root, "elsewhere")}}},
		{"outside compose file", api.ComposeParams{Project: api.ProjectPart{Name: "demo", ComposeFile: filepath.Join(root, "other.yaml")}}},
		{"parent traversal", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: "a/../../outside"}}}},
		{"absolute path", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: "/tmp/outside"}}}},
		{"windows path", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: `C:\outside`}}}},
		{"reserved file", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: "compose.yaml"}}}},
		{"reserved import manifest", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: ".UCD-IMPORT.JSON"}}}},
		{"case-insensitive duplicate", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: "a"}, {Path: "A"}}}},
		{"duplicate file", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}, Files: []api.File{{Path: "a"}, {Path: "a"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := api.NewRequest("compose.import", "linux", tc.params)
			if err != nil {
				t.Fatal(err)
			}
			if err := prepareCompose(&request, root); err == nil {
				t.Fatal("invalid project path was accepted")
			}
		})
	}
	t.Run("symbolic link", func(t *testing.T) {
		projects := filepath.Join(root, "projects")
		if err := os.Mkdir(projects, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(projects, "demo")); err != nil {
			t.Skipf("symbolic links unavailable: %v", err)
		}
		request, _ := api.NewRequest("compose.import", "linux", api.ComposeParams{Project: api.ProjectPart{Name: "demo"}})
		if err := prepareCompose(&request, root); err == nil {
			t.Fatal("symlinked project directory was accepted")
		}
	})
}

func TestRequestLimitsAndStructuredErrors(t *testing.T) {
	s := startTestServer(t, callerFunc(func(context.Context, api.Request) (any, error) { return nil, nil }), &api.EventBus{})
	client := s.httpClient(&s.controller)
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"invalid JSON", "{", http.StatusBadRequest},
		{"trailing JSON", `{"method":"engine.status"}{}`, http.StatusBadRequest},
		{"unknown field", `{"method":"engine.status","extra":true}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, s.server.URL+"/v1/call", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body api.Response
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != tc.status || body.Error == nil {
				t.Fatalf("response: status %d, body %+v, error %v", resp.StatusCode, body, err)
			}
		})
	}
	controller, err := x509.ParseCertificate(s.controller.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{controller}, VerifiedChains: [][]*x509.Certificate{{controller, s.ca.certificate}}}
	t.Run("oversized declared body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/call", strings.NewReader("{}"))
		req.TLS = state
		req.ContentLength = maxRequestBytes + 1
		rec := httptest.NewRecorder()
		NewHandler(nil, nil, s.dataRoot).ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
	})
	t.Run("oversized streamed body", func(t *testing.T) {
		body := io.MultiReader(strings.NewReader(`{"method":"`), io.LimitReader(repeatByte('x'), maxRequestBytes), strings.NewReader(`"}`))
		req := httptest.NewRequest(http.MethodPost, "/v1/call", body)
		req.TLS = state
		rec := httptest.NewRecorder()
		NewHandler(nil, nil, s.dataRoot).ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
	})
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func TestCancellationReachesBackend(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	s := startTestServer(t, callerFunc(func(ctx context.Context, _ api.Request) (any, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}), &api.EventBus{})
	client, err := NewClient(s.settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, api.Request{Method: "engine.status"})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call returned %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not receive cancellation")
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "helper.json")
	t.Run("Linux defaults", func(t *testing.T) {
		if filepath.Separator == '\\' {
			t.Skip("Linux default paths are not absolute on Windows")
		}
		if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		config, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if config.Listen != "127.0.0.1:7443" || config.DataRoot != "/var/lib/ucd" {
			t.Fatalf("unexpected defaults: %+v", config)
		}
	})
	t.Run("listener must be loopback", func(t *testing.T) {
		config := Config{Listen: "0.0.0.0:7443", DataRoot: root, CACert: path, ServerCert: path, ServerKey: path}
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("non-loopback listener accepted")
		}
	})
	t.Run("oversized configuration", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("{}"+strings.Repeat(" ", 1<<20)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("oversized configuration accepted")
		}
	})
}
