package webui

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/transferclient"
)

const (
	downloadLifetime    = 2 * time.Minute
	maxPendingDownloads = 128
	maxActiveDownloads  = 4
)

type downloadTicket struct {
	engine    string
	sessionID string
	name      string
	expires   time.Time
	timer     *time.Timer
}

type downloads struct {
	caller  api.Caller
	mu      sync.Mutex
	pending map[string]*downloadTicket
	active  chan struct{}
}

func newDownloads(caller api.Caller) *downloads {
	return &downloads{caller: caller, pending: make(map[string]*downloadTicket), active: make(chan struct{}, maxActiveDownloads)}
}

func (d *downloads) prepare(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request struct {
		Engine    string `json:"engine"`
		SessionID string `json:"sessionID"`
		Name      string `json:"name"`
	}
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "invalid download request"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", "download request must contain one JSON object"))
		return
	}
	if err := transferclient.Validate(request.Engine, request.SessionID); err != nil {
		writeError(w, http.StatusBadRequest, api.Fail("invalid_argument", err.Error()))
		return
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("generate download token: %w", err))
		return
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	d.mu.Lock()
	if len(d.pending) >= maxPendingDownloads {
		d.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, errors.New("too many pending downloads"))
		return
	}
	for _, pending := range d.pending {
		if pending.engine == request.Engine && pending.sessionID == request.SessionID {
			d.mu.Unlock()
			writeError(w, http.StatusConflict, errors.New("download is already prepared"))
			return
		}
	}
	ticket := &downloadTicket{engine: request.Engine, sessionID: request.SessionID, name: transferclient.Filename(request.Name), expires: time.Now().Add(downloadLifetime)}
	d.pending[token] = ticket
	ticket.timer = time.AfterFunc(downloadLifetime, func() { d.expire(token, ticket) })
	d.mu.Unlock()
	_ = json.NewEncoder(w).Encode(struct {
		URL string `json:"url"`
	}{URL: "/v1/download/" + token})
}

func (d *downloads) expire(token string, ticket *downloadTicket) {
	d.mu.Lock()
	if d.pending[token] != ticket {
		d.mu.Unlock()
		return
	}
	delete(d.pending, token)
	ticket.timer.Stop()
	d.mu.Unlock()
	if err := transferclient.Close(d.caller, ticket.engine, ticket.sessionID); err != nil {
		log.Printf("close expired download: %v", err)
	}
}

func (d *downloads) take(token string) *downloadTicket {
	d.mu.Lock()
	ticket := d.pending[token]
	if ticket != nil {
		delete(d.pending, token)
		ticket.timer.Stop()
	}
	d.mu.Unlock()
	if ticket != nil && !time.Now().Before(ticket.expires) {
		if err := transferclient.Close(d.caller, ticket.engine, ticket.sessionID); err != nil {
			log.Printf("close expired download: %v", err)
		}
		return nil
	}
	return ticket
}

func (d *downloads) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "same origin required", http.StatusForbidden)
		return
	}
	select {
	case d.active <- struct{}{}:
		defer func() { <-d.active }()
	default:
		http.Error(w, "too many active downloads", http.StatusTooManyRequests)
		return
	}
	ticket := d.take(strings.TrimPrefix(r.URL.Path, "/v1/download/"))
	if ticket == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": ticket.name}))
	written, readErr := transferclient.Copy(r.Context(), d.caller, ticket.engine, ticket.sessionID, flushWriter{w})
	closeErr := transferclient.Close(d.caller, ticket.engine, ticket.sessionID)
	if err := errors.Join(readErr, closeErr); err != nil {
		if r.Context().Err() != nil {
			if closeErr != nil {
				log.Printf("close canceled download: %v", closeErr)
			}
			return
		}
		if written != 0 {
			// A truncated chunked response must be reported as a failed download.
			log.Printf("download failed: %v", err)
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Content-Disposition")
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusInternalServerError
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == "permission_denied" {
			status = http.StatusForbidden
		}
		writeError(w, status, err)
		return
	}
	if written == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err == nil {
		if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return n, err
}
