package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
)

var dockerVersionPath = regexp.MustCompile(`^/v[0-9]+\.[0-9]+/`)

// DockerPolicyHandler applies the same machine defaults to native Docker,
// Compose and Buildx HTTP calls as to the GUI's engine operations. Streaming
// request and response bodies pass through unchanged except small create JSON.
func (b *Backend) DockerPolicyHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.id == "windows" || b.id == "linux" {
			if kind := dockerDisplayKindFor(r.Method, r.URL.Path); kind != dockerDisplayNone {
				b.serveDockerDisplay(w, r, next, kind)
				return
			}
		}
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		path := dockerVersionPath.ReplaceAllString(r.URL.Path, "/")
		configuration := b.RuntimeConfiguration()
		var err error
		switch path {
		case "/containers/create":
			err = b.applyDockerContainerDefaults(r, configuration)
		case "/networks/create":
			if b.id == "linux" {
				err = applyDockerNetworkDefaults(r, configuration.Network)
			}
		case "/build":
			var args map[string]*string
			query := r.URL.Query()
			if raw := query.Get("buildargs"); raw != "" {
				err = json.Unmarshal([]byte(raw), &args)
			}
			if err == nil {
				args = ApplyProxyBuildArgs(args, configuration.ContainersProxy)
				if len(args) != 0 {
					var encoded []byte
					encoded, err = json.Marshal(args)
					if err == nil {
						query.Set("buildargs", string(encoded))
						r.URL.RawQuery = query.Encode()
					}
				}
			}
		}
		if err != nil {
			dockerproxy.WriteError(w, http.StatusBadRequest, "apply UCD defaults: "+err.Error())
			return
		}
		if b.id == "windows" && path == "/containers/create" {
			b.serveDockerCreateWithAliases(w, r, next)
			return
		}
		if b.id == "linux" {
			if id, ok := dockerContainerStartID(r.URL.Path); ok {
				b.serveDockerContainerStart(w, r, next, id)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func dockerContainerStartID(path string) (string, bool) {
	path = dockerVersionPath.ReplaceAllString(path, "/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 3 || parts[0] != "containers" || parts[1] == "" || (parts[2] != "start" && parts[2] != "restart") {
		return "", false
	}
	return parts[1], true
}

func (b *Backend) serveDockerContainerStart(w http.ResponseWriter, r *http.Request, next http.Handler, id string) {
	response := newDockerDisplayWriter(1 << 20)
	next.ServeHTTP(response, r)
	if response.tooLarge {
		dockerproxy.WriteError(w, http.StatusBadGateway, "Docker container start response exceeds the UCD buffering limit")
		return
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	if (status >= http.StatusOK && status < http.StatusMultipleChoices) || status == http.StatusNotModified {
		if err := b.waitLinuxContainerPorts(r.Context(), id); err != nil {
			dockerproxy.WriteError(w, http.StatusInternalServerError, "container started but UCD published port forwarding failed: "+err.Error())
			return
		}
	}
	for key, values := range response.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.WriteHeader(status)
	_, _ = w.Write(response.body.Bytes())
}

func readDockerCreate(r *http.Request) (map[string]json.RawMessage, []byte, error) {
	const limit = 8 << 20
	if r.Body == nil {
		return nil, nil, fmt.Errorf("create request body is required")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	closeErr := r.Body.Close()
	if err != nil {
		return nil, nil, err
	}
	if closeErr != nil {
		return nil, nil, closeErr
	}
	if len(data) > limit {
		return nil, nil, fmt.Errorf("create configuration exceeds 8 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, nil, fmt.Errorf("invalid create configuration: %w", err)
	}
	if fields == nil {
		return nil, nil, fmt.Errorf("create configuration must be a JSON object")
	}
	if err := normalizeDockerFields(fields, "HostConfig", "Env", "Labels", "ExposedPorts", "EnableIPv4", "EnableIPv6", "Options"); err != nil {
		return nil, nil, err
	}
	return fields, data, nil
}

func normalizeDockerFields(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		found := ""
		for key := range fields {
			if strings.EqualFold(key, name) {
				if found != "" {
					return fmt.Errorf("ambiguous duplicate field %s", name)
				}
				found = key
			}
		}
		if found != "" && found != name {
			fields[name] = fields[found]
			delete(fields, found)
		}
	}
	return nil
}

func replaceDockerCreate(r *http.Request, fields map[string]json.RawMessage) error {
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.TransferEncoding = nil
	r.Header.Del("Content-Length")
	return nil
}

func (b *Backend) applyDockerContainerDefaults(r *http.Request, settings api.EngineConfiguration) error {
	fields, data, err := readDockerCreate(r)
	if err != nil {
		return err
	}
	p := api.ContainerCreateParams{Config: &container.Config{}}
	if err := json.Unmarshal(data, p.Config); err != nil {
		return fmt.Errorf("decode container configuration: %w", err)
	}
	if raw := fields["HostConfig"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &p.HostConfig); err != nil {
			return fmt.Errorf("decode container host configuration: %w", err)
		}
	}
	if p.HostConfig != nil {
		if err := ValidateContainerNetworkMode(b.id, string(p.HostConfig.NetworkMode), settings.Network); err != nil {
			return err
		}
	}
	p.Config.Env = ApplyContainerProxyEnvironment(p.Config.Env, settings.ContainersProxy)
	p.HostConfig = ApplyContainerDNSDefaults(b.id, p.HostConfig)
	if err := b.prepareContainerPorts(r.Context(), &p); err != nil {
		return err
	}
	for name, value := range map[string]any{"Env": p.Config.Env, "Labels": p.Config.Labels, "ExposedPorts": p.Config.ExposedPorts} {
		if _, existed := fields[name]; !existed {
			switch value := value.(type) {
			case []string:
				if len(value) == 0 {
					continue
				}
			case map[string]string:
				if len(value) == 0 {
					continue
				}
			case network.PortSet:
				if len(value) == 0 {
					continue
				}
			}
		}
		fields[name], err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	if p.HostConfig != nil {
		// Retain Docker fields introduced after our SDK while replacing only
		// values that UCD's port preparation can change.
		host := make(map[string]json.RawMessage)
		if raw := fields["HostConfig"]; len(raw) != 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &host); err != nil {
				return err
			}
		}
		if err := normalizeDockerFields(host, "PortBindings", "PublishAllPorts", "Dns"); err != nil {
			return err
		}
		host["PortBindings"], _ = json.Marshal(p.HostConfig.PortBindings)
		host["PublishAllPorts"], _ = json.Marshal(p.HostConfig.PublishAllPorts)
		host["Dns"], _ = json.Marshal(p.HostConfig.DNS)
		fields["HostConfig"], err = json.Marshal(host)
		if err != nil {
			return err
		}
	}
	return replaceDockerCreate(r, fields)
}

func applyDockerNetworkDefaults(r *http.Request, settings api.NetworkSettings) error {
	fields, data, err := readDockerCreate(r)
	if err != nil {
		return err
	}
	var request network.CreateRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	if err := ApplyNetworkDefaults(&request, settings); err != nil {
		return err
	}
	for name, value := range map[string]any{"EnableIPv4": request.EnableIPv4, "EnableIPv6": request.EnableIPv6, "Options": request.Options, "Labels": request.Labels} {
		fields[name], err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	return replaceDockerCreate(r, fields)
}
