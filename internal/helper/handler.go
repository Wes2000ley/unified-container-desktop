package helper

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skyri/ucd/internal/api"
)

const (
	controllerName   = "ucd-controller"
	maxRequestBytes  = 80 << 20
	maxResponseBytes = 80 << 20
	maxYAMLBytes     = 4 << 20
	maxFileBytes     = 32 << 20
	maxTransferBytes = 56 << 20
	maxFiles         = 512
	maxFilePathBytes = 1024
)

// NewHandler returns the Linux engine API. Its HTTP server must require and
// verify client certificates using the private controller CA.
func NewHandler(backend api.Caller, events *api.EventBus, dataRoot string) http.Handler {
	return newHandler(backend, events, dataRoot, nil)
}

func newHandler(backend api.Caller, events *api.EventBus, dataRoot string, docker http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verifyController(r.TLS); err != nil {
			w.Header().Set("Content-Type", "application/json")
			writeError(w, http.StatusUnauthorized, api.Fail("unauthenticated", err.Error()))
			return
		}
		if docker != nil && (r.URL.Path == "/docker" || strings.HasPrefix(r.URL.Path, "/docker/")) {
			http.StripPrefix("/docker", docker).ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("set helper request deadline: %w", err))
			return
		}
		if r.URL.Path != "/v1/call" {
			writeError(w, http.StatusNotFound, api.Fail("not_found", "helper endpoint not found"))
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, api.Fail("invalid_argument", "helper calls require POST"))
			return
		}
		if r.ContentLength > maxRequestBytes {
			writeError(w, http.StatusRequestEntityTooLarge, api.Fail("invalid_argument", "helper request exceeds 80 MiB"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request api.Request
		if err := decoder.Decode(&request); err != nil {
			writeDecodeError(w, err)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				err = errors.New("only one JSON value is permitted")
			}
			writeDecodeError(w, err)
			return
		}
		if !allowedMethod(request.Method) {
			writeError(w, http.StatusForbidden, api.Fail("permission_denied", "method is not available through the Linux helper"))
			return
		}
		if request.Engine != "" && request.Engine != "linux" {
			writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "Linux helper accepts only the linux engine"))
			return
		}
		request.Engine = "linux"
		if strings.HasPrefix(request.Method, "compose.") && request.Method != "compose.logs.read" && request.Method != "compose.logs.close" {
			if err := prepareCompose(&request, dataRoot); err != nil {
				writeError(w, http.StatusBadRequest, asAPIError(err))
				return
			}
		}
		if request.Method == "system.events" {
			params, err := api.Decode[api.EventsParams](request)
			if err != nil {
				writeError(w, http.StatusBadRequest, asAPIError(err))
				return
			}
			if events == nil {
				writeError(w, http.StatusInternalServerError, api.Fail("internal", "helper event bus is unavailable"))
				return
			}
			writeResult(w, events.Batch(params.After, params.Epoch))
			return
		}
		if backend == nil {
			writeError(w, http.StatusInternalServerError, api.Fail("internal", "Linux engine is unavailable"))
			return
		}
		result, err := backend.Call(r.Context(), request)
		if err != nil {
			apiErr := asAPIError(err)
			status := http.StatusInternalServerError
			switch apiErr.Code {
			case "invalid_argument":
				status = http.StatusBadRequest
			case "not_found":
				status = http.StatusNotFound
			case "permission_denied":
				status = http.StatusForbidden
			}
			writeError(w, status, apiErr)
			return
		}
		writeResult(w, result)
	})
}

func allowedMethod(method string) bool {
	if api.IsEngineMethod(method) {
		return true
	}
	switch method {
	case "project.discover", "system.events", "compose.validate", "compose.import", "compose.read", "compose.write", "compose.action", "compose.ports",
		"compose.logs.open", "compose.logs.read", "compose.logs.close":
		return true
	default:
		return false
	}
}

func verifyController(state *tls.ConnectionState) error {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return errors.New("a verified controller certificate is required")
	}
	if state.PeerCertificates[0].Subject.CommonName != controllerName {
		return errors.New("unexpected controller certificate subject")
	}
	return nil
}

func prepareCompose(request *api.Request, dataRoot string) error {
	params, err := api.Decode[api.ComposeParams](*request)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dataRoot) {
		return api.Fail("invalid_argument", "helper data root must be an absolute path")
	}
	if !api.ValidName(params.Project.Name) {
		return api.Fail("invalid_argument", "project name must start with a lowercase letter and contain only lowercase letters, digits, or hyphens (maximum 63 characters)")
	}
	if params.Project.Engine != "" && params.Project.Engine != "linux" {
		return api.Fail("invalid_argument", "compose project must use the linux engine")
	}
	directory := filepath.Join(filepath.Clean(dataRoot), "projects", params.Project.Name)
	composeFile := filepath.Join(directory, "compose.yaml")
	if params.Project.Directory != "" && params.Project.Directory != directory {
		return api.Fail("invalid_argument", "project is outside the managed directory; import it to manage it here")
	}
	if params.Project.ComposeFile != "" && params.Project.ComposeFile != composeFile {
		return api.Fail("invalid_argument", "compose file is outside the managed project; import the project to manage it here")
	}
	if len(params.YAML) > maxYAMLBytes {
		return api.Fail("invalid_argument", "compose YAML exceeds 4 MiB")
	}
	if len(params.Files) > maxFiles {
		return api.Fail("invalid_argument", "project transfer exceeds 512 files")
	}
	paths := map[string]bool{"compose.yaml": true, ".ucd-import.json": true}
	total := 0
	for _, file := range params.Files {
		key := strings.ToLower(file.Path)
		if !validTransferPath(file.Path) || paths[key] {
			return api.Fail("invalid_argument", "project file path is invalid, duplicated, or reserved")
		}
		paths[key] = true
		if len(file.Content) > maxFileBytes {
			return api.Fail("invalid_argument", "project file exceeds 32 MiB")
		}
		total += len(file.Content)
		if total > maxTransferBytes {
			return api.Fail("invalid_argument", "project files exceed 56 MiB")
		}
		if err := rejectSymlinks(filepath.Join(directory, filepath.FromSlash(file.Path))); err != nil {
			return err
		}
	}
	if err := rejectSymlinks(composeFile); err != nil {
		return err
	}
	params.Project.Engine = "linux"
	params.Project.Directory = directory
	params.Project.ComposeFile = composeFile
	request.Params, err = json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode compose request: %w", err)
	}
	return nil
}

func validTransferPath(name string) bool {
	return len(name) <= maxFilePathBytes && name != "." && fs.ValidPath(name) &&
		!strings.ContainsAny(name, "\\:\x00")
}

// Reject links at every existing component, including the data root's parents.
// The backend must still use rooted file operations to avoid races while writing.
func rejectSymlinks(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return api.Fail("invalid_argument", fmt.Sprintf("inspect managed project path: %v", err))
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return api.Fail("invalid_argument", "managed project paths must not contain symbolic links")
		}
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	return nil
}

func asAPIError(err error) *api.Error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return &api.Error{Code: "engine_error", Message: err.Error()}
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, api.Fail("invalid_argument", "helper request exceeds 80 MiB"))
		return
	}
	writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", fmt.Sprintf("invalid helper request: %v", err)))
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Response{Error: asAPIError(err)})
}

func writeResult(w http.ResponseWriter, value any) {
	_ = json.NewEncoder(w).Encode(api.Response{Result: value})
}
