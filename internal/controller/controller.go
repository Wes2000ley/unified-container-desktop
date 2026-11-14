// Package controller coordinates two independent engines without replacing Compose.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skyri/ucd/internal/api"
	"github.com/skyri/ucd/internal/resourcesaver"
	"github.com/skyri/ucd/internal/state"
)

type Controller struct {
	store           *state.Store
	engines         map[string]api.Caller
	platform        api.Caller
	events          *api.EventBus
	mu              sync.Mutex
	locks           map[string]*projectLock
	portsMu         sync.Mutex
	eventsMu        sync.Mutex
	linuxSequence   uint64
	linuxEpoch      string
	linuxEventError string
	settingsMu      sync.Mutex
	saver           *resourcesaver.Gate
}
type projectLock struct {
	mu   sync.Mutex
	refs int
}
type ListError struct {
	Engine  string `json:"engine"`
	Message string `json:"message"`
}
type Inventory struct {
	Items  []json.RawMessage `json:"items"`
	Errors []ListError       `json:"errors"`
}

func New(store *state.Store, windows, linux, platform api.Caller, events *api.EventBus) *Controller {
	return &Controller{store: store, engines: map[string]api.Caller{"windows": windows, "linux": linux}, platform: platform, events: events, locks: map[string]*projectLock{}}
}

// SetResourceSaver attaches the service-owned gate before serving requests.
func (c *Controller) SetResourceSaver(gate *resourcesaver.Gate) { c.saver = gate }

func (c *Controller) Call(ctx context.Context, r api.Request) (any, error) {
	if err := api.Authorize(api.CurrentPrincipal(ctx), r.Method); err != nil {
		return nil, err
	}
	if r.Engine != "" && !api.ValidEngine(r.Engine) {
		return nil, api.Fail("invalid_argument", "engine must be windows or linux")
	}
	switch r.Method {
	case "engine.list":
		return c.engineList(ctx), nil
	case "system.settings":
		return struct {
			Settings  api.Settings  `json:"settings"`
			Principal api.Principal `json:"principal"`
		}{c.settingsSnapshot(), api.CurrentPrincipal(ctx)}, nil
	case "system.events":
		p, err := api.Decode[api.EventsParams](r)
		if err != nil {
			return nil, err
		}
		c.collectLinuxEvents(ctx)
		return c.events.Batch(p.After, p.Epoch), nil
	case "system.diagnostics":
		return c.diagnostics(ctx), nil
	case "system.configure":
		return c.configure(ctx, r)
	case "registry.login":
		return c.registryLogin(ctx, r)
	case "registry.list", "registry.credential.get", "registry.credential.list":
		// Registry credentials belong to the machine, so both engine views use
		// the Windows controller store as their single authority.
		return c.engineCall(ctx, "windows", r)
	case "registry.logout", "registry.credential.store", "registry.credential.erase":
		return c.engineCall(ctx, "windows", r)
	case "project.list":
		return c.projects(ctx), nil
	case "project.import":
		return c.importProject(ctx, r)
	case "project.read", "project.write", "project.action", "project.logs.open":
		return c.projectCall(ctx, r)
	case "project.logs.read", "project.logs.close":
		if r.Engine == "" {
			return nil, api.Fail("invalid_argument", "an engine is required for a project log session")
		}
		r.Method = strings.Replace(r.Method, "project.", "compose.", 1)
		return c.engineCall(ctx, r.Engine, r)
	case "engine.start", "engine.stop", "wsl.status", "wsl.configure", "operator.list", "operator.add", "operator.remove", "share.list", "share.create", "share.remove":
		if r.Method == "wsl.configure" {
			c.settingsMu.Lock()
			defer c.settingsMu.Unlock()
			p, e := api.Decode[api.WSLSettings](r)
			if e != nil {
				return nil, e
			}
			next := c.store.Snapshot()
			next.WSL.CPUs, next.WSL.MemoryMB, next.WSL.DiskGB = p.CPUs, p.MemoryMB, p.DiskGB
			if e = state.Validate(next); e != nil {
				return nil, api.Fail("invalid_argument", e.Error())
			}
		}
		if c.platform == nil {
			return nil, api.Fail("unavailable", "Windows infrastructure manager is unavailable")
		}
		result, err := c.platform.Call(ctx, r)
		if err == nil && r.Engine == "linux" && (r.Method == "engine.start" || r.Method == "engine.stop") && c.saver != nil {
			c.saver.ResetAfterEngineControl()
		}
		if err == nil && r.Method == "wsl.configure" {
			p, e := api.Decode[struct {
				CPUs     int   `json:"cpus"`
				MemoryMB int64 `json:"memoryMB"`
				DiskGB   int64 `json:"diskGB"`
			}](r)
			if e != nil {
				return nil, e
			}
			err = c.store.Update(func(s *api.Settings) error {
				s.WSL.CPUs = p.CPUs
				s.WSL.MemoryMB = p.MemoryMB
				s.WSL.DiskGB = p.DiskGB
				return nil
			})
			if err != nil {
				return result, fmt.Errorf("WSL resource limits were applied, but saving controller metadata failed: %w; restart the controller to reconcile its metadata", err)
			}
		}
		return result, err
	case "container.create", "container.run":
		if r.Engine == "" {
			return nil, api.Fail("invalid_argument", "an engine is required for this operation")
		}
		p, err := api.Decode[api.ContainerCreateParams](r)
		if err != nil {
			return nil, err
		}
		// Check the same effective addresses that the engine will publish.
		if c.store.Snapshot().Network.DefaultPortBinding == api.DefaultPortBindingAll && p.HostConfig != nil {
			for target, bindings := range p.HostConfig.PortBindings {
				for i := range bindings {
					if !bindings[i].HostIP.IsValid() {
						bindings[i].HostIP = netip.IPv4Unspecified()
					}
				}
				p.HostConfig.PortBindings[target] = bindings
			}
		}
		ports, err := api.ContainerPublishedPorts(p)
		if err != nil {
			return nil, err
		}
		c.portsMu.Lock()
		defer c.portsMu.Unlock()
		if _, err = c.checkDesiredPorts(ctx, r.Engine, "", ports); err != nil {
			return nil, err
		}
		return c.engineCall(ctx, r.Engine, r)
	case "container.list", "image.list", "volume.list", "network.list":
		if r.Engine == "" {
			return c.inventory(ctx, r), nil
		}
		return c.engineCall(ctx, r.Engine, r)
	case "engine.status", "container.inspect", "container.logs", "container.stats", "container.action", "image.pull", "image.remove", "volume.inspect", "volume.remove":
		if r.Engine == "" {
			return nil, api.Fail("invalid_argument", "an engine is required for this operation")
		}
		return c.engineCall(ctx, r.Engine, r)
	default:
		if api.IsEngineMethod(r.Method) {
			if r.Engine == "" {
				return nil, api.Fail("invalid_argument", "an engine is required for this operation")
			}
			return c.engineCall(ctx, r.Engine, r)
		}
		return nil, api.Fail("not_found", "unknown controller method: "+r.Method)
	}
}

func (c *Controller) settingsSnapshot() api.Settings {
	s := c.store.Snapshot()
	if source, ok := c.platform.(interface{ WSLSettings() api.WSLSettings }); ok {
		s.WSL = source.WSLSettings()
	}
	return s
}
func (c *Controller) engineCall(ctx context.Context, id string, r api.Request) (any, error) {
	b := c.engines[id]
	if b == nil {
		return nil, api.Fail("unavailable", id+" engine is not configured")
	}
	r.Engine = id
	if id == "linux" && needsRegistryAuth(r) {
		result, err := c.callLinuxWithRegistry(ctx, b, r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
		return result, nil
	}
	result, err := b.Call(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	return result, nil
}

func needsRegistryAuth(r api.Request) bool {
	switch r.Method {
	case "image.pull", "image.push", "image.build":
		return true
	case "transfer.open":
		p, err := api.Decode[api.TransferOpenParams](r)
		return err == nil && p.Operation == "image.build"
	case "compose.action":
		p, err := api.Decode[api.ComposeParams](r)
		return err == nil && (p.Action == "up" || p.Action == "build" || p.Action == "pull")
	default:
		return false
	}
}

func (c *Controller) callLinuxWithRegistry(ctx context.Context, linux api.Caller, operation api.Request) (any, error) {
	windows := c.engines["windows"]
	if windows == nil {
		return nil, api.Fail("unavailable", "machine registry credential store is unavailable")
	}
	snapshot, err := windows.Call(ctx, api.Request{Method: "registry.internal.snapshot", Engine: "windows"})
	if err != nil {
		return nil, fmt.Errorf("read machine registry credentials: %w", err)
	}
	var credentials api.RegistrySnapshot
	if err := convert(snapshot, &credentials); err != nil {
		return nil, fmt.Errorf("decode machine registry credentials: %w", err)
	}
	request, err := api.NewRequest("registry.internal.call", "linux", api.RegistryOperation{Request: operation, Snapshot: credentials})
	if err != nil {
		return nil, fmt.Errorf("encode Linux registry operation: %w", err)
	}
	result, err := linux.Call(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("execute Linux operation with shared registry credentials: %w", err)
	}
	return result, nil
}

func (c *Controller) registryLogin(ctx context.Context, request api.Request) (any, error) {
	p, err := api.Decode[api.RegistryParams](request)
	if err != nil {
		return nil, err
	}
	var auth api.RegistryAuth
	failures := make([]string, 0, 2)
	for _, id := range []string{"windows", "linux"} {
		engine := c.engines[id]
		if engine == nil {
			failures = append(failures, id+" engine is not configured")
			continue
		}
		validate, err := api.NewRequest("registry.internal.validate", id, p)
		if err != nil {
			return nil, fmt.Errorf("encode registry validation request: %w", err)
		}
		value, err := engine.Call(ctx, validate)
		if err == nil {
			if err := convert(value, &auth); err != nil {
				return nil, fmt.Errorf("decode registry validation result from %s engine: %w", id, err)
			}
			break
		}
		failures = append(failures, id+" engine: "+err.Error())
	}
	if auth.Server == "" {
		return nil, fmt.Errorf("registry authentication failed on both engines: %s", strings.Join(failures, "; "))
	}
	windows := c.engines["windows"]
	if windows == nil {
		return nil, api.Fail("unavailable", "machine registry credential store is unavailable")
	}
	store, err := api.NewRequest("registry.internal.store", "windows", api.RegistryAuthParams{Server: auth.Server, Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("encode machine registry credential request: %w", err)
	}
	if _, err := windows.Call(ctx, store); err != nil {
		return nil, fmt.Errorf("save machine registry credentials: %w", err)
	}
	return nil, nil
}
func (c *Controller) engineList(ctx context.Context) []api.EngineStatus {
	out := make([]api.EngineStatus, 2)
	var wg sync.WaitGroup
	for i, id := range []string{"windows", "linux"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			check, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			v, err := c.engineCall(check, id, api.Request{Method: "engine.status"})
			out[i] = api.EngineStatus{ID: id, OS: id}
			if err == nil {
				err = convert(v, &out[i])
			}
			if err != nil {
				out[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()
	return out
}
func (c *Controller) inventory(ctx context.Context, r api.Request) Inventory {
	results := make([]any, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []string{"windows", "linux"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			check, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			results[i], errs[i] = c.engineCall(check, id, r)
		}()
	}
	wg.Wait()
	out := Inventory{Items: []json.RawMessage{}, Errors: []ListError{}}
	for i, id := range []string{"windows", "linux"} {
		if errs[i] != nil {
			out.Errors = append(out.Errors, ListError{id, errs[i].Error()})
			continue
		}
		var rows []json.RawMessage
		if err := convert(results[i], &rows); err != nil {
			out.Errors = append(out.Errors, ListError{id, err.Error()})
			continue
		}
		out.Items = append(out.Items, rows...)
	}
	return out
}
func convert(value, out any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// collectLinuxEvents runs only on an active client event request. Engine streams
// stay local to each host; no background inventory polling is needed.
func (c *Controller) collectLinuxEvents(ctx context.Context) {
	c.eventsMu.Lock()
	defer c.eventsMu.Unlock()
	req, _ := api.NewRequest("system.events", "linux", api.EventsParams{After: c.linuxSequence, Epoch: c.linuxEpoch})
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	value, err := c.engineCall(check, "linux", req)
	if err != nil {
		if err.Error() != c.linuxEventError {
			c.events.Publish(api.Event{Engine: "linux", Type: "engine.events_unavailable", Message: err.Error()})
			c.linuxEventError = err.Error()
		}
		return
	}
	c.linuxEventError = ""
	var batch api.EventBatch
	if err := convert(value, &batch); err != nil {
		return
	}
	if batch.Epoch != c.linuxEpoch {
		c.linuxSequence = 0
		c.linuxEpoch = batch.Epoch
	}
	for _, event := range batch.Events {
		if event.Sequence > c.linuxSequence {
			c.linuxSequence = event.Sequence
		}
		c.events.Publish(api.Event{Engine: "linux", Project: event.Project, Type: event.Type, Message: event.Message})
	}
}

func (c *Controller) projectLock(id string) func() {
	c.mu.Lock()
	l := c.locks[id]
	if l == nil {
		l = &projectLock{}
		c.locks[id] = l
	}
	l.refs++
	c.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		c.mu.Lock()
		defer c.mu.Unlock()
		l.refs--
		if l.refs == 0 {
			delete(c.locks, id)
		}
	}
}
func (c *Controller) projects(ctx context.Context) []api.Project {
	projects := c.store.Snapshot().Projects
	known := map[string]bool{}
	for _, p := range projects {
		for _, part := range p.Parts {
			known[part.Engine+"/"+part.Name] = true
		}
	}
	for _, engine := range []string{"windows", "linux"} {
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		value, err := c.engineCall(check, engine, api.Request{Method: "project.discover"})
		cancel()
		if err != nil {
			continue
		}
		var parts []api.ProjectPart
		if convert(value, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if known[engine+"/"+part.Name] {
				continue
			}
			part.Engine = engine
			projects = append(projects, api.Project{ID: "discovered-" + engine + "-" + part.Name, Name: part.Name, Parts: []api.ProjectPart{part}, Discovered: true})
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects
}
func (c *Controller) findProject(ctx context.Context, id string) (api.Project, error) {
	for _, p := range c.store.Snapshot().Projects {
		if p.ID == id {
			return p, nil
		}
	}
	if strings.HasPrefix(id, "discovered-") {
		for _, p := range c.projects(ctx) {
			if p.ID == id {
				return p, nil
			}
		}
	}
	return api.Project{}, api.Fail("not_found", "project not found: "+id)
}
func projectID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	if len(id) > 48 {
		id = strings.TrimRight(id[:48], "-")
	}
	return id
}
func (c *Controller) importProject(ctx context.Context, r api.Request) (any, error) {
	p, err := api.Decode[api.ProjectImport](r)
	if err != nil {
		return nil, err
	}
	if !api.ValidEngine(p.Engine) {
		p.Engine = r.Engine
	}
	if !api.ValidEngine(p.Engine) {
		return nil, api.Fail("invalid_argument", "project engine must be windows or linux")
	}
	implicitID := p.ID == ""
	if implicitID {
		p.ID = projectID(p.Name)
	}
	if !api.ValidName(p.ID) || len(p.ID) > 55 {
		return nil, api.Fail("invalid_argument", "project ID must start with a lowercase letter and contain at most 55 lowercase letters, digits or hyphens")
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 128 {
		return nil, api.Fail("invalid_argument", "project name must contain 1 to 128 characters")
	}
	if strings.TrimSpace(p.YAML) == "" {
		return nil, api.Fail("invalid_argument", "Compose YAML is required")
	}
	unlock := c.projectLock(p.ID)
	defer unlock()
	logical := api.Project{ID: p.ID, Name: p.Name, Parts: []api.ProjectPart{}, StartupOrder: p.StartupOrder}
	replaceIndex := -1
	for _, existing := range c.store.Snapshot().Projects {
		if existing.ID == p.ID {
			if implicitID && existing.Name != p.Name {
				return nil, api.Fail("already_exists", "another application has the same normalized ID; choose an explicit unique project ID")
			}
			logical = existing
			for i, part := range logical.Parts {
				if part.Engine == p.Engine {
					if !p.Replace {
						return nil, api.Fail("already_exists", "this application already has a "+p.Engine+" Compose project; edit it instead")
					}
					replaceIndex = i
				}
			}
		}
	}
	for _, id := range p.StartupOrder {
		if !api.ValidEngine(id) {
			return nil, api.Fail("invalid_argument", "startup order must contain only windows and linux")
		}
	}
	name := p.ID + "-" + p.Engine
	directory := filepath.Join(c.store.Snapshot().DataRoot, "projects", name)
	composeFile := filepath.Join(directory, "compose.yaml")
	if p.Engine == "linux" {
		directory = "/var/lib/ucd/projects/" + name
		composeFile = directory + "/compose.yaml"
	}
	part := api.ProjectPart{Engine: p.Engine, Name: name, Directory: directory, ComposeFile: composeFile}
	req, _ := api.NewRequest("compose.import", p.Engine, api.ComposeParams{Project: part, YAML: p.YAML, Files: p.Files, Replace: p.Replace})
	value, err := c.engineCall(ctx, p.Engine, req)
	if err != nil {
		return nil, err
	}
	if err = convert(value, &part); err != nil {
		return nil, fmt.Errorf("read imported project: %w", err)
	}
	if replaceIndex >= 0 {
		logical.Parts[replaceIndex] = part
	} else {
		logical.Parts = append(logical.Parts, part)
	}
	if len(p.StartupOrder) > 0 {
		logical.StartupOrder = append([]string{}, p.StartupOrder...)
	}
	err = c.store.Update(func(s *api.Settings) error {
		for i := range s.Projects {
			if s.Projects[i].ID == logical.ID {
				s.Projects[i] = logical
				return nil
			}
		}
		s.Projects = append(s.Projects, logical)
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.events.Publish(api.Event{Type: "project.imported", Project: p.ID, Engine: p.Engine, Message: "Imported " + logical.Name})
	return logical, nil
}
func (c *Controller) projectCall(ctx context.Context, r api.Request) (any, error) {
	p, err := api.Decode[api.ProjectParams](r)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.Fail("invalid_argument", "project ID is required")
	}
	unlock := c.projectLock(p.ID)
	defer unlock()
	logical, err := c.findProject(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if p.Engine == "" {
		p.Engine = r.Engine
	}
	if p.Engine != "" && !api.ValidEngine(p.Engine) {
		return nil, api.Fail("invalid_argument", "invalid project engine")
	}
	parts := orderedParts(logical, p.Engine)
	if len(parts) == 0 {
		return nil, api.Fail("not_found", "project has no part for the requested engine")
	}
	if r.Method == "project.read" {
		out := []api.ProjectFile{}
		for _, part := range parts {
			req, _ := api.NewRequest("compose.read", part.Engine, api.ComposeParams{Project: part})
			value, e := c.engineCall(ctx, part.Engine, req)
			if e != nil {
				return nil, e
			}
			var file api.ProjectFile
			if e = convert(value, &file); e != nil {
				return nil, e
			}
			out = append(out, file)
		}
		return out, nil
	}
	if r.Method == "project.write" {
		if len(parts) != 1 {
			return nil, api.Fail("invalid_argument", "choose an engine when editing a mixed application")
		}
		req, _ := api.NewRequest("compose.write", parts[0].Engine, api.ComposeParams{Project: parts[0], YAML: p.YAML})
		return c.engineCall(ctx, parts[0].Engine, req)
	}
	if r.Method == "project.logs.open" {
		result := api.OperationResult{ProjectID: p.ID, Success: true, Parts: []api.PartResult{}}
		for _, part := range parts {
			req, _ := api.NewRequest("compose.logs.open", part.Engine, api.ComposeParams{Project: part, Services: p.Services})
			value, e := c.engineCall(ctx, part.Engine, req)
			pr := api.PartResult{Engine: part.Engine, Result: value}
			if e != nil {
				pr.Error = e.Error()
				result.Success = false
			}
			result.Parts = append(result.Parts, pr)
		}
		return result, nil
	}
	actions := map[string]bool{"up": true, "start": true, "stop": true, "restart": true, "down": true, "remove": true, "pull": true, "build": true, "logs": true}
	if !actions[p.Action] {
		return nil, api.Fail("invalid_argument", "unsupported project action: "+p.Action)
	}
	if p.RemoveVolumes && p.Action != "down" && p.Action != "remove" {
		return nil, api.Fail("invalid_argument", "volume removal requires down or remove")
	}
	if len(p.Services) > 0 && (p.Action == "remove" || p.Action == "down") {
		return nil, api.Fail("invalid_argument", "down and project removal apply to the whole project; use stop for selected services")
	}
	if p.Action == "up" || p.Action == "start" || p.Action == "restart" {
		c.portsMu.Lock()
		defer c.portsMu.Unlock()
	}
	if p.Action == "stop" || p.Action == "down" || p.Action == "remove" {
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
	}
	// Inspect all selected parts before starting any of them. A port claimed by
	// two parts must prevent both deployments, even if neither is running yet.
	portErrors := make([]error, len(parts))
	if p.Action == "up" || p.Action == "start" || p.Action == "restart" {
		portLists := make([][]api.Port, len(parts))
		for i, part := range parts {
			portLists[i], portErrors[i] = c.checkPorts(ctx, part, p.Services)
		}
		for i := range parts {
			for j := i + 1; j < len(parts); j++ {
				for _, a := range portLists[i] {
					for _, b := range portLists[j] {
						if portsOverlap(a, b) {
							conflict := api.Fail("port_conflict", fmt.Sprintf("host port %d is requested by both %s and %s project parts", a.Published, parts[i].Engine, parts[j].Engine))
							for _, index := range []int{i, j} {
								if portErrors[index] == nil {
									portErrors[index] = conflict
								} else {
									portErrors[index] = fmt.Errorf("%v; %w", portErrors[index], conflict)
								}
							}
						}
					}
				}
			}
		}
	}
	result := api.OperationResult{ProjectID: p.ID, Success: true, Parts: []api.PartResult{}}
	for i, part := range parts {
		e := portErrors[i]
		var value any
		if e == nil {
			action := p.Action
			if action == "remove" {
				action = "down"
			}
			req, _ := api.NewRequest("compose.action", part.Engine, api.ComposeParams{Project: part, Action: action, RemoveVolumes: p.RemoveVolumes, Services: p.Services})
			value, e = c.engineCall(ctx, part.Engine, req)
		}
		pr := api.PartResult{Engine: part.Engine, Result: value}
		if e != nil {
			pr.Error = e.Error()
			result.Success = false
		}
		result.Parts = append(result.Parts, pr)
		message := p.Action + " completed"
		if e != nil {
			message = e.Error()
		}
		c.events.Publish(api.Event{Type: "project.operation", Project: p.ID, Engine: part.Engine, Message: message})
	}
	if p.Action == "remove" && result.Success {
		if logical.Discovered {
			return result, nil
		}
		err = c.store.Update(func(s *api.Settings) error {
			for i, q := range s.Projects {
				if q.ID != p.ID {
					continue
				}
				remaining := []api.ProjectPart{}
				for _, part := range q.Parts {
					if p.Engine != "" && part.Engine != p.Engine {
						remaining = append(remaining, part)
					}
				}
				if len(remaining) == 0 {
					s.Projects = append(s.Projects[:i], s.Projects[i+1:]...)
				} else {
					s.Projects[i].Parts = remaining
				}
				break
			}
			return nil
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}
func orderedParts(p api.Project, engine string) []api.ProjectPart {
	out := []api.ProjectPart{}
	for _, id := range append(append([]string{}, p.StartupOrder...), "windows", "linux") {
		for _, part := range p.Parts {
			if part.Engine != id || engine != "" && part.Engine != engine {
				continue
			}
			exists := false
			for _, old := range out {
				if old.Engine == part.Engine {
					exists = true
				}
			}
			if !exists {
				out = append(out, part)
			}
		}
	}
	return out
}
func (c *Controller) checkPorts(ctx context.Context, part api.ProjectPart, services []string) ([]api.Port, error) {
	req, _ := api.NewRequest("compose.ports", part.Engine, api.ComposeParams{Project: part, Services: services})
	value, err := c.engineCall(ctx, part.Engine, req)
	if err != nil {
		return nil, err
	}
	var desired []api.Port
	if err = convert(value, &desired); err != nil {
		return nil, err
	}
	return c.checkDesiredPorts(ctx, part.Engine, part.Name, desired)
}

func (c *Controller) checkDesiredPorts(ctx context.Context, engine, project string, desired []api.Port) ([]api.Port, error) {
	if len(desired) == 0 {
		return desired, nil
	}
	var err error
	owned := map[string]bool{}
	other := []api.Port{}
	for _, id := range []string{"windows", "linux"} {
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		value, e := c.engineCall(check, id, api.Request{Method: "container.list"})
		cancel()
		if e != nil {
			continue
		}
		var containers []api.Container
		if convert(value, &containers) != nil {
			continue
		}
		for _, container := range containers {
			for _, port := range container.Ports {
				if project != "" && id == engine && container.Project == project {
					owned[portKey(port)] = true
				} else {
					other = append(other, port)
				}
			}
		}
	}
	seen := []api.Port{}
	for _, port := range desired {
		if port.Published == 0 {
			continue
		}
		if port.HostIP == "" {
			port.HostIP = "127.0.0.1"
		}
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}
		for _, existing := range append(append([]api.Port{}, other...), seen...) {
			if portsOverlap(port, existing) {
				return desired, api.Fail("port_conflict", fmt.Sprintf("host %s port %d on %s is already reserved by another service", port.Protocol, port.Published, port.HostIP))
			}
		}
		seen = append(seen, port)
		if owned[portKey(port)] {
			continue
		}
		address := net.JoinHostPort(port.HostIP, strconv.Itoa(int(port.Published)))
		switch port.Protocol {
		case "tcp":
			var listener net.Listener
			listener, err = net.Listen("tcp", address)
			if err == nil {
				err = listener.Close()
			}
		case "udp":
			var listener net.PacketConn
			listener, err = net.ListenPacket("udp", address)
			if err == nil {
				err = listener.Close()
			}
		default:
			return desired, api.Fail("invalid_argument", "only TCP and UDP published ports are supported")
		}
		if err != nil {
			return desired, api.Fail("port_conflict", fmt.Sprintf("host %s port %s is unavailable: %v", port.Protocol, address, err))
		}
	}
	return desired, nil
}
func portKey(p api.Port) string {
	host := p.HostIP
	if host == "" {
		host = "127.0.0.1"
	}
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return fmt.Sprintf("%s/%s/%d", host, proto, p.Published)
}
func portsOverlap(a, b api.Port) bool {
	if a.Published == 0 || a.Published != b.Published {
		return false
	}
	pa, pb := a.Protocol, b.Protocol
	if pa == "" {
		pa = "tcp"
	}
	if pb == "" {
		pb = "tcp"
	}
	if pa != pb {
		return false
	}
	ha, hb := a.HostIP, b.HostIP
	if ha == "" {
		ha = "127.0.0.1"
	}
	if hb == "" {
		hb = "127.0.0.1"
	}
	return ha == hb || ha == "0.0.0.0" || hb == "0.0.0.0" || ha == "::" || hb == "::"
}
func (c *Controller) configure(ctx context.Context, r api.Request) (any, error) {
	c.settingsMu.Lock()
	defer c.settingsMu.Unlock()
	p, err := api.Decode[api.Settings](r)
	if err != nil {
		return nil, err
	}
	current := c.settingsSnapshot()
	next := current
	next.DockerProxy = p.DockerProxy
	next.ContainersProxy = p.ContainersProxy
	next.Network = p.Network
	next.ResourceSaver = p.ResourceSaver
	next.RegistryMirrors = p.RegistryMirrors
	if p.WSL.CPUs != current.WSL.CPUs || p.WSL.MemoryMB != current.WSL.MemoryMB || p.WSL.DiskGB != current.WSL.DiskGB {
		return nil, api.Fail("invalid_argument", "use wsl.configure to change WSL resource limits")
	}
	if err = state.Validate(next); err != nil {
		return nil, api.Fail("invalid_argument", err.Error())
	}
	if next.ResourceSaver.Enabled && c.saver == nil {
		return nil, api.Fail("unavailable", "Linux Resource Saver is unavailable; check the WSL infrastructure diagnostics")
	}
	// Engine configuration is explicit and never implicitly restarts workloads.
	results := []api.PartResult{}
	applied := []string{}
	failures := []string{}
	resourceOnly := next.ResourceSaver != current.ResourceSaver &&
		next.DockerProxy == current.DockerProxy && next.ContainersProxy == current.ContainersProxy &&
		next.Network == current.Network && slices.Equal(next.RegistryMirrors, current.RegistryMirrors)
	engines := []string{"windows", "linux"}
	if resourceOnly {
		engines = nil
	}
	restartRequired := false
	for _, id := range engines {
		value, e := c.writeEngineSettings(ctx, id, next)
		pr := api.PartResult{Engine: id, Result: value}
		if e != nil {
			pr.Error = e.Error()
			failures = append(failures, e.Error())
		} else {
			applied = append(applied, id)
			var outcome struct {
				RestartRequired bool `json:"restartRequired"`
			}
			if convert(value, &outcome) == nil && outcome.RestartRequired {
				restartRequired = true
			}
		}
		results = append(results, pr)
	}
	rollback := func() {
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, id := range applied {
			if _, e := c.writeEngineSettings(restore, id, current); e != nil {
				failures = append(failures, id+" configuration rollback failed: "+e.Error())
			}
		}
	}
	if len(failures) > 0 {
		rollback()
		return nil, api.Fail("configuration_failed", strings.Join(failures, "; "))
	}
	if c.saver != nil && next.ResourceSaver != current.ResourceSaver {
		if err := c.saver.Configure(ctx, resourcesaver.Config{Enabled: next.ResourceSaver.Enabled, IdleSeconds: next.ResourceSaver.IdleSeconds}); err != nil {
			failures = append(failures, err.Error())
			rollback()
			return nil, api.Fail("configuration_failed", strings.Join(failures, "; "))
		}
	}
	if err = c.store.Update(func(s *api.Settings) error {
		s.DockerProxy = next.DockerProxy
		s.ContainersProxy = next.ContainersProxy
		s.Network = next.Network
		s.ResourceSaver = next.ResourceSaver
		s.RegistryMirrors = next.RegistryMirrors
		s.WSL = current.WSL
		return nil
	}); err != nil {
		failures = append(failures, err.Error())
		if c.saver != nil && next.ResourceSaver != current.ResourceSaver {
			restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if err := c.saver.Configure(restore, resourcesaver.Config{Enabled: current.ResourceSaver.Enabled, IdleSeconds: current.ResourceSaver.IdleSeconds}); err != nil {
				failures = append(failures, "restore Resource Saver: "+err.Error())
			}
			cancel()
		}
		rollback()
		return nil, api.Fail("configuration_failed", strings.Join(failures, "; "))
	}
	return map[string]any{"settings": c.settingsSnapshot(), "restartRequired": restartRequired, "parts": results}, nil
}

func engineConfiguration(s api.Settings) api.EngineConfiguration {
	return api.EngineConfiguration{
		DockerProxy: s.DockerProxy, ContainersProxy: s.ContainersProxy,
		Network: s.Network, RegistryMirrors: s.RegistryMirrors,
	}
}

func (c *Controller) writeEngineSettings(ctx context.Context, id string, s api.Settings) (any, error) {
	req, _ := api.NewRequest("engine.configure", id, engineConfiguration(s))
	return c.engineCall(ctx, id, req)
}
func (c *Controller) diagnostics(ctx context.Context) any {
	wsl := any(nil)
	infraError := ""
	if c.platform != nil {
		v, err := c.platform.Call(ctx, api.Request{Method: "wsl.status"})
		if err != nil {
			infraError = err.Error()
		} else {
			wsl = v
		}
	}
	return map[string]any{"version": api.Version, "engines": c.engineList(ctx), "wsl": wsl, "infrastructureError": infraError, "events": c.events.Since(0), "limitations": []string{"Standalone Docker Engine on Windows 11 is outside Microsoft's documented Docker Desktop support configuration.", "Service-account WSL2 startup and multi-user persistence require target-machine acceptance testing."}}
}
