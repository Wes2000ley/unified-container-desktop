package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/moby/moby/api/types/container"
	"github.com/skyri/ucd/internal/dockerproxy"
)

// Only the finite create response is buffered. Attach, exec, build and other
// streaming Docker requests continue through the ordinary proxy untouched.
func (b *Backend) serveDockerCreateWithAliases(w http.ResponseWriter, r *http.Request, next http.Handler) {
	data, err := io.ReadAll(r.Body) // applyDockerContainerDefaults already enforces 8 MiB.
	if err != nil {
		dockerproxy.WriteError(w, http.StatusBadRequest, "read container host aliases: "+err.Error())
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	var request struct{ HostConfig *container.HostConfig }
	if err := json.Unmarshal(data, &request); err != nil {
		dockerproxy.WriteError(w, http.StatusBadRequest, "decode container host aliases: "+err.Error())
		return
	}
	r.Header.Del("Accept-Encoding")
	response := newDockerDisplayWriter(1 << 20)
	next.ServeHTTP(response, r)
	if response.tooLarge {
		dockerproxy.WriteError(w, http.StatusBadGateway, "Docker container create response exceeds 1 MiB")
		return
	}
	if response.status == http.StatusCreated {
		var created struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(response.body.Bytes(), &created); err != nil || created.ID == "" {
			dockerproxy.WriteError(w, http.StatusBadGateway, "Docker created a container but returned an invalid container ID")
			return
		}
		if err := b.prepareWindowsHostAliases(r.Context(), created.ID, request.HostConfig); err != nil {
			dockerproxy.WriteError(w, http.StatusInternalServerError, "container "+created.ID+" was created but Docker host aliases could not be configured: "+err.Error())
			return
		}
	}
	for name, values := range response.header {
		w.Header()[name] = append([]string(nil), values...)
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(response.body.Bytes())
}
