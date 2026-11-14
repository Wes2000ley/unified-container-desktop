package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/dockerproxy"
)

const (
	maxDockerDisplayResponse = 80 << 20
	maxWindowsPortMetadata   = 1 << 20
)

type dockerDisplayKind uint8

const (
	dockerDisplayNone dockerDisplayKind = iota
	dockerDisplayList
	dockerDisplayInspect
)

var errDockerDisplayResponseTooLarge = errors.New("Docker response exceeds the UCD port-display limit")

// dockerDisplayKindFor selects only finite JSON endpoints whose output omits
// UCD's Windows port forwards. Other Docker requests stay on the stream path.
func dockerDisplayKindFor(method, path string) dockerDisplayKind {
	if method != http.MethodGet {
		return dockerDisplayNone
	}
	path = dockerVersionPath.ReplaceAllString(path, "/")
	if path == "/containers/json" {
		return dockerDisplayList
	}
	if strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json") {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
		if name != "" && !strings.Contains(name, "/") {
			return dockerDisplayInspect
		}
	}
	return dockerDisplayNone
}

func (b *Backend) serveDockerDisplay(w http.ResponseWriter, r *http.Request, next http.Handler, kind dockerDisplayKind) {
	response := newDockerDisplayWriter(maxDockerDisplayResponse)
	next.ServeHTTP(response, r)
	if response.tooLarge {
		dockerproxy.WriteError(w, http.StatusBadGateway, "container port metadata could not be added because the Docker response exceeded 80 MiB; narrow the container list or inspect one container")
		return
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	body := response.body.Bytes()
	changed := false
	if status >= http.StatusOK && status < http.StatusMultipleChoices && isJSONResponse(response.header) {
		var err error
		body, changed, err = b.decorateDockerDisplayBody(kind, response.header, body)
		if err != nil {
			dockerproxy.WriteError(w, http.StatusBadGateway, "could not add Windows container port metadata: "+err.Error())
			return
		}
	}
	for key, values := range response.header {
		w.Header()[key] = append([]string(nil), values...)
	}
	if changed {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (b *Backend) decorateDockerDisplayBody(kind dockerDisplayKind, header http.Header, body []byte) ([]byte, bool, error) {
	original := body
	encoded := strings.EqualFold(strings.TrimSpace(header.Get("Content-Encoding")), "gzip")
	if encoded {
		decoded, err := gunzipDockerDisplayBody(body)
		if err != nil {
			return nil, false, err
		}
		body = decoded
	} else if contentEncoding := strings.TrimSpace(header.Get("Content-Encoding")); contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		return nil, false, fmt.Errorf("unsupported response encoding %q", contentEncoding)
	}
	var (
		updated []byte
		changed bool
		err     error
	)
	switch kind {
	case dockerDisplayList:
		updated, changed, err = b.decorateDockerContainerList(body)
	case dockerDisplayInspect:
		updated, changed, err = b.decorateDockerContainerInspect(body)
	default:
		return original, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return original, false, nil
	}
	if !encoded {
		return updated, changed, err
	}
	compressed, err := gzipDockerDisplayBody(updated)
	if err != nil {
		return nil, false, err
	}
	return compressed, true, nil
}

func isJSONResponse(header http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func (b *Backend) decorateDockerContainerList(body []byte) ([]byte, bool, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil || entries == nil {
		return body, false, nil
	}
	changed := false
	for index, raw := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, false, fmt.Errorf("decode Docker container list item %d", index)
		}
		var id, state string
		if err := json.Unmarshal(fields["State"], &state); err != nil || !strings.EqualFold(state, "running") {
			continue
		}
		labels, err := rawObject(fields["Labels"], "container labels")
		if err != nil {
			return nil, false, err
		}
		requested, found, err := rawWindowsPortMetadata(labels)
		if err != nil || !found {
			if err != nil {
				return nil, false, err
			}
			continue
		}
		if err := json.Unmarshal(fields["Id"], &id); err != nil || id == "" {
			return nil, false, errors.New("Docker container list item has UCD port metadata but no ID")
		}
		active := matchActiveDockerPorts(requested, b.forwardedPorts(id))
		if len(active) == 0 {
			continue
		}
		var ports []json.RawMessage
		if rawPorts := fields["Ports"]; len(rawPorts) != 0 && string(rawPorts) != "null" {
			if err := json.Unmarshal(rawPorts, &ports); err != nil {
				return nil, false, fmt.Errorf("decode Docker container ports: %w", err)
			}
		}
		for _, port := range active {
			if dockerListHasPort(ports, port) {
				continue
			}
			encoded, err := json.Marshal(dockerPortSummary{
				IP:          port.HostIP,
				PrivatePort: port.Target,
				PublicPort:  port.Published,
				Type:        port.Protocol,
			})
			if err != nil {
				return nil, false, fmt.Errorf("encode Docker port summary: %w", err)
			}
			ports = append(ports, encoded)
		}
		fields["Ports"], err = json.Marshal(ports)
		if err != nil {
			return nil, false, fmt.Errorf("encode Docker container ports: %w", err)
		}
		entries[index], err = json.Marshal(fields)
		if err != nil {
			return nil, false, fmt.Errorf("encode Docker container list item: %w", err)
		}
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	updated, err := json.Marshal(entries)
	if err != nil {
		return nil, false, fmt.Errorf("encode Docker container list: %w", err)
	}
	return updated, true, nil
}

type dockerPortSummary struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

func dockerListHasPort(ports []json.RawMessage, want api.Port) bool {
	for _, raw := range ports {
		var item struct {
			IP          string `json:"IP"`
			PrivatePort uint16 `json:"PrivatePort"`
			PublicPort  uint16 `json:"PublicPort"`
			Type        string `json:"Type"`
		}
		if json.Unmarshal(raw, &item) == nil && item.IP == want.HostIP && item.PrivatePort == want.Target && item.PublicPort == want.Published && strings.EqualFold(item.Type, want.Protocol) {
			return true
		}
	}
	return false
}

func (b *Backend) decorateDockerContainerInspect(body []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return body, false, nil
	}
	config, err := rawObject(fields["Config"], "container config")
	if err != nil {
		return nil, false, err
	}
	labels, err := rawObject(config["Labels"], "container labels")
	if err != nil {
		return nil, false, err
	}
	requested, found, err := rawWindowsPortMetadata(labels)
	if err != nil || !found {
		if err != nil {
			return nil, false, err
		}
		return body, false, nil
	}
	var id string
	if err := json.Unmarshal(fields["Id"], &id); err != nil || id == "" {
		return nil, false, errors.New("Docker inspect response has UCD port metadata but no ID")
	}
	bindings, managedKeys, err := dockerPortBindings(requested)
	if err != nil {
		return nil, false, err
	}
	hostConfig, err := rawObject(fields["HostConfig"], "container host config")
	if err != nil {
		return nil, false, err
	}
	existingBindings, err := rawObject(hostConfig["PortBindings"], "container host port bindings")
	if err != nil {
		return nil, false, err
	}
	for key, value := range bindings {
		existingBindings[key] = value
	}
	hostConfig["PortBindings"], err = json.Marshal(existingBindings)
	if err != nil {
		return nil, false, fmt.Errorf("encode Docker host port bindings: %w", err)
	}
	fields["HostConfig"], err = json.Marshal(hostConfig)
	if err != nil {
		return nil, false, fmt.Errorf("encode Docker host config: %w", err)
	}
	state, err := rawObject(fields["State"], "container state")
	if err != nil {
		return nil, false, err
	}
	var running bool
	_ = json.Unmarshal(state["Running"], &running)
	networkSettings, err := rawObject(fields["NetworkSettings"], "container network settings")
	if err != nil {
		return nil, false, err
	}
	existingPorts, err := rawObject(networkSettings["Ports"], "container network ports")
	if err != nil {
		return nil, false, err
	}
	for key := range managedKeys {
		delete(existingPorts, key)
	}
	if running {
		active := matchActiveDockerPorts(requested, b.forwardedPorts(id))
		activeBindings, _, err := dockerPortBindings(active)
		if err != nil {
			return nil, false, err
		}
		for key, value := range activeBindings {
			existingPorts[key] = value
		}
	}
	networkSettings["Ports"], err = json.Marshal(existingPorts)
	if err != nil {
		return nil, false, fmt.Errorf("encode Docker network ports: %w", err)
	}
	fields["NetworkSettings"], err = json.Marshal(networkSettings)
	if err != nil {
		return nil, false, fmt.Errorf("encode Docker network settings: %w", err)
	}
	updated, err := json.Marshal(fields)
	if err != nil {
		return nil, false, fmt.Errorf("encode Docker inspect response: %w", err)
	}
	return updated, true, nil
}

func rawWindowsPortMetadata(labels map[string]json.RawMessage) ([]api.Port, bool, error) {
	raw, found := labels[windowsPortsLabel]
	if !found {
		return nil, false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, fmt.Errorf("decode %s label: %w", windowsPortsLabel, err)
	}
	ports, err := parseWindowsPortMetadata(value)
	return ports, true, err
}

func parseWindowsPortMetadata(value string) ([]api.Port, error) {
	if len(value) > maxWindowsPortMetadata {
		return nil, fmt.Errorf("%s label exceeds 1 MiB", windowsPortsLabel)
	}
	var ports []api.Port
	if err := json.Unmarshal([]byte(value), &ports); err != nil || len(ports) == 0 || len(ports) > 1024 {
		return nil, fmt.Errorf("container has invalid %s label", windowsPortsLabel)
	}
	for _, port := range ports {
		if err := validateForwardPort(port); err != nil {
			return nil, err
		}
	}
	return ports, nil
}

func rawObject(raw json.RawMessage, field string) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return make(map[string]json.RawMessage), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("decode %s object", field)
	}
	return object, nil
}

func dockerPortBindings(ports []api.Port) (map[string]json.RawMessage, map[string]struct{}, error) {
	type binding struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	values := make(map[string][]binding)
	keys := make(map[string]struct{})
	for _, port := range ports {
		if err := validateForwardPort(port); err != nil {
			return nil, nil, err
		}
		key := strconv.Itoa(int(port.Target)) + "/" + port.Protocol
		hostPort := ""
		if port.Published != 0 {
			hostPort = strconv.Itoa(int(port.Published))
		}
		values[key] = append(values[key], binding{HostIP: port.HostIP, HostPort: hostPort})
		keys[key] = struct{}{}
	}
	encoded := make(map[string]json.RawMessage, len(values))
	for key, bindings := range values {
		value, err := json.Marshal(bindings)
		if err != nil {
			return nil, nil, fmt.Errorf("encode Docker port binding %s: %w", key, err)
		}
		encoded[key] = value
	}
	return encoded, keys, nil
}

func matchActiveDockerPorts(requested, active []api.Port) []api.Port {
	remaining := make(map[string][]uint16, len(requested))
	for _, port := range requested {
		key := dockerPortIdentity(port)
		remaining[key] = append(remaining[key], port.Published)
	}
	matched := make([]api.Port, 0, len(active))
	for _, port := range active {
		key := dockerPortIdentity(port)
		requests := remaining[key]
		if len(requests) == 0 {
			continue
		}
		match := -1
		for i, published := range requests {
			if published == 0 || published == port.Published {
				match = i
				break
			}
		}
		if match < 0 {
			continue
		}
		remaining[key] = append(requests[:match], requests[match+1:]...)
		matched = append(matched, port)
	}
	return matched
}

func dockerPortIdentity(port api.Port) string {
	return port.HostIP + "/" + strconv.Itoa(int(port.Target)) + "/" + port.Protocol
}

func (b *Backend) decorateManagedInspectPorts(inspect *container.InspectResponse) error {
	if b.id != "windows" || inspect.Config == nil || inspect.Config.Labels == nil {
		return nil
	}
	value, found := inspect.Config.Labels[windowsPortsLabel]
	if !found {
		return nil
	}
	requested, err := parseWindowsPortMetadata(value)
	if err != nil {
		return err
	}
	bindings, managedKeys, err := dockerPortBindings(requested)
	if err != nil {
		return err
	}
	if inspect.HostConfig == nil {
		inspect.HostConfig = &container.HostConfig{}
	}
	if inspect.HostConfig.PortBindings == nil {
		inspect.HostConfig.PortBindings = make(network.PortMap)
	}
	for key, raw := range bindings {
		port, err := network.ParsePort(key)
		if err != nil {
			return fmt.Errorf("parse Docker port binding %s: %w", key, err)
		}
		var hostBindings []struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		}
		if err := json.Unmarshal(raw, &hostBindings); err != nil {
			return fmt.Errorf("decode Docker port binding %s: %w", key, err)
		}
		bindings := make([]network.PortBinding, 0, len(hostBindings))
		for _, binding := range hostBindings {
			hostIP, err := netip.ParseAddr(binding.HostIP)
			if err != nil {
				return fmt.Errorf("parse Docker host IP %q: %w", binding.HostIP, err)
			}
			bindings = append(bindings, network.PortBinding{HostIP: hostIP, HostPort: binding.HostPort})
		}
		inspect.HostConfig.PortBindings[port] = bindings
	}
	if inspect.NetworkSettings == nil {
		inspect.NetworkSettings = &container.NetworkSettings{}
	}
	if inspect.NetworkSettings.Ports == nil {
		inspect.NetworkSettings.Ports = make(network.PortMap)
	}
	for key := range managedKeys {
		port, err := network.ParsePort(key)
		if err != nil {
			return fmt.Errorf("parse Docker port binding %s: %w", key, err)
		}
		delete(inspect.NetworkSettings.Ports, port)
	}
	if inspect.State != nil && inspect.State.Running {
		active := matchActiveDockerPorts(requested, b.forwardedPorts(inspect.ID))
		for _, binding := range active {
			port, err := network.ParsePort(strconv.Itoa(int(binding.Target)) + "/" + binding.Protocol)
			if err != nil {
				return fmt.Errorf("parse Docker port binding: %w", err)
			}
			hostIP, err := netip.ParseAddr(binding.HostIP)
			if err != nil {
				return fmt.Errorf("parse Docker host IP %q: %w", binding.HostIP, err)
			}
			inspect.NetworkSettings.Ports[port] = append(inspect.NetworkSettings.Ports[port], network.PortBinding{HostIP: hostIP, HostPort: strconv.Itoa(int(binding.Published))})
		}
	}
	return nil
}

type dockerDisplayWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	limit    int64
	tooLarge bool
}

func newDockerDisplayWriter(limit int64) *dockerDisplayWriter {
	return &dockerDisplayWriter{header: make(http.Header), limit: limit}
}

func (w *dockerDisplayWriter) Header() http.Header { return w.header }

func (w *dockerDisplayWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *dockerDisplayWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.tooLarge {
		return 0, errDockerDisplayResponseTooLarge
	}
	if int64(len(p)) > w.limit-int64(w.body.Len()) {
		w.tooLarge = true
		return 0, errDockerDisplayResponseTooLarge
	}
	return w.body.Write(p)
}

func (w *dockerDisplayWriter) Flush() {}

func gunzipDockerDisplayBody(body []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("decode compressed Docker response: %w", err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, maxDockerDisplayResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read compressed Docker response: %w", err)
	}
	if len(decoded) > maxDockerDisplayResponse {
		return nil, errDockerDisplayResponseTooLarge
	}
	return decoded, nil
}

func gzipDockerDisplayBody(body []byte) ([]byte, error) {
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(body); err != nil {
		return nil, fmt.Errorf("compress decorated Docker response: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish decorated Docker response: %w", err)
	}
	return encoded.Bytes(), nil
}
