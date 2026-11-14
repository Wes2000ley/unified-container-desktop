package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

func (b *Backend) status(ctx context.Context) api.EngineStatus {
	status := api.EngineStatus{ID: b.id, OS: b.id, ActiveOperations: b.activeOperations()}
	result, err := b.docker.Info(ctx, client.InfoOptions{})
	if err != nil {
		status.Error = err.Error()
		return status
	}
	i := result.Info
	status.Online, status.OS, status.Version = true, i.OSType, i.ServerVersion
	status.Containers, status.Running, status.Stopped = i.Containers, i.ContainersRunning, i.ContainersStopped
	status.CPUs = i.NCPU
	if i.MemTotal > 0 {
		status.MemoryBytes = uint64(i.MemTotal)
	}
	return status
}

func (b *Backend) containerCall(ctx context.Context, req api.Request) (any, error) {
	if strings.HasPrefix(req.Method, "container.exec.") {
		return b.containerExecCall(ctx, req)
	}
	if strings.HasPrefix(req.Method, "container.logs.") {
		return b.containerLogStreamCall(ctx, req)
	}
	if strings.HasPrefix(req.Method, "container.files.") {
		return b.containerFilesCall(ctx, req)
	}
	switch req.Method {
	case "container.create", "container.run", "container.rename", "container.update", "container.top", "container.diff", "container.commit", "container.export":
		return b.containerOperation(ctx, req)
	case "container.logs":
		return b.containerLogs(ctx, req)
	}
	if req.Method == "container.list" {
		params, err := api.Decode[struct {
			Metrics bool `json:"metrics"`
		}](req)
		if err != nil {
			return nil, err
		}
		result, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("list containers: %w", err)
		}
		out := make([]api.Container, 0, len(result.Items))
		for _, c := range result.Items {
			name := ""
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			var forwarded []api.Port
			if b.id == "windows" && string(c.State) == "running" {
				forwarded = b.forwardedPorts(c.ID)
			}
			ports := make([]api.Port, 0, len(c.Ports)+len(forwarded))
			for _, p := range c.Ports {
				// Docker reports the exposed target separately; the UCD listener
				// represents the same port with its actual host binding.
				duplicate := false
				for _, f := range forwarded {
					if p.PublicPort == 0 && p.PrivatePort == f.Target && p.Type == f.Protocol {
						duplicate = true
						break
					}
				}
				if duplicate {
					continue
				}
				ip := ""
				if p.IP.IsValid() {
					ip = p.IP.String()
				}
				ports = append(ports, api.Port{HostIP: ip, Published: p.PublicPort, Target: p.PrivatePort, Protocol: p.Type})
			}
			ports = append(ports, forwarded...)
			out = append(out, api.Container{ID: c.ID, Name: name, Engine: b.id, Image: c.Image, State: string(c.State), Status: c.Status, Project: c.Labels["com.docker.compose.project"], Service: c.Labels["com.docker.compose.service"], Ports: ports})
		}
		if params.Metrics {
			if err := b.populateMetrics(ctx, out); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	p, err := api.Decode[api.ContainerParams](req)
	if err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, api.Fail("invalid_argument", "container ID is required")
	}
	switch req.Method {
	case "container.inspect":
		result, err := b.docker.ContainerInspect(ctx, p.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect container: %w", err)
		}
		if err := b.decorateManagedInspectPorts(&result.Container); err != nil {
			return nil, fmt.Errorf("decorate inspected container ports: %w", err)
		}
		return result.Container, nil
	case "container.stats":
		return b.containerStats(ctx, p.ID)
	case "container.action":
		switch p.Action {
		case "start":
			_, err = b.docker.ContainerStart(ctx, p.ID, client.ContainerStartOptions{})
		case "stop":
			_, err = b.docker.ContainerStop(ctx, p.ID, client.ContainerStopOptions{})
		case "restart":
			_, err = b.docker.ContainerRestart(ctx, p.ID, client.ContainerRestartOptions{})
		case "pause":
			_, err = b.docker.ContainerPause(ctx, p.ID, client.ContainerPauseOptions{})
		case "unpause":
			_, err = b.docker.ContainerUnpause(ctx, p.ID, client.ContainerUnpauseOptions{})
		case "kill":
			_, err = b.docker.ContainerKill(ctx, p.ID, client.ContainerKillOptions{})
		case "remove":
			_, err = b.docker.ContainerRemove(ctx, p.ID, client.ContainerRemoveOptions{Force: p.Force})
		default:
			return nil, api.Fail("invalid_argument", "unknown container action: "+p.Action)
		}
		if err != nil {
			err = fmt.Errorf("container %s: %w", p.Action, err)
		}
		if b.id == "windows" {
			if portErr := b.reconcileActionPorts(ctx, func(c container.Summary) bool {
				if strings.HasPrefix(c.ID, p.ID) {
					return true
				}
				for _, name := range c.Names {
					if strings.TrimPrefix(name, "/") == strings.TrimPrefix(p.ID, "/") {
						return true
					}
				}
				return false
			}); portErr != nil {
				return nil, errors.Join(err, fmt.Errorf("reconcile published ports: %w", portErr))
			}
		}
		if err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown container method: "+req.Method)
	}
}

func (b *Backend) imageCall(ctx context.Context, req api.Request) (any, error) {
	switch req.Method {
	case "image.inspect", "image.history", "image.tag", "image.push", "image.load", "image.save", "image.import", "image.build":
		return b.imageResourceCall(ctx, req)
	}
	if req.Method == "image.list" {
		result, err := b.docker.ImageList(ctx, client.ImageListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("list images: %w", err)
		}
		containers, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("check image usage: %w", err)
		}
		usage := make(map[string]int64)
		for _, c := range containers.Items {
			usage[c.ImageID]++
		}
		out := make([]api.Image, 0, len(result.Items))
		for _, i := range result.Items {
			out = append(out, api.Image{ID: i.ID, Engine: b.id, Tags: i.RepoTags, Size: i.Size, Created: i.Created, Containers: usage[i.ID]})
		}
		return out, nil
	}
	p, err := api.Decode[api.ImageParams](req)
	if err != nil {
		return nil, err
	}
	switch req.Method {
	case "image.pull":
		if p.Reference == "" {
			return nil, api.Fail("invalid_argument", "image reference is required")
		}
		auth, secrets, err := b.machineRegistryAuth(ctx, p.Reference)
		if err != nil {
			return nil, fmt.Errorf("resolve machine registry credentials: %w", err)
		}
		stream, err := b.docker.ImagePull(ctx, p.Reference, client.ImagePullOptions{RegistryAuth: auth})
		if err != nil {
			return nil, fmt.Errorf("pull image: %w", redactResourceError(err, secrets))
		}
		for message, err := range stream.JSONMessages(ctx) {
			if err != nil {
				return nil, fmt.Errorf("pull image: %w", redactResourceError(err, secrets))
			}
			if message.Error != nil {
				return nil, fmt.Errorf("pull image: %w", redactResourceError(message.Error, secrets))
			}
			if message.Status != "" {
				b.events.Publish(api.Event{Engine: b.id, Type: "image.progress", Message: redactResourceText(strings.TrimSpace(message.ID+" "+message.Status), secrets)})
			}
		}
		return nil, nil
	case "image.remove":
		id := p.ID
		if id == "" {
			id = p.Reference
		}
		if id == "" {
			return nil, api.Fail("invalid_argument", "image ID is required")
		}
		image, err := b.docker.ImageInspect(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("inspect image: %w", err)
		}
		containers, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("check image usage: %w", err)
		}
		for _, c := range containers.Items {
			if c.ImageID == image.ID {
				return nil, fmt.Errorf("image is used by container %s", c.ID)
			}
		}
		// Deleting by immutable ID makes the daemon enforce its own in-use check,
		// including when a container appeared after our explicit check.
		_, err = b.docker.ImageRemove(ctx, image.ID, client.ImageRemoveOptions{})
		if err != nil {
			return nil, fmt.Errorf("remove image: %w", err)
		}
		return nil, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown image method: "+req.Method)
	}
}

func (b *Backend) populateMetrics(ctx context.Context, containers []api.Container) error {
	// Keep inventory usable for large lists and leave time to return the rows.
	budget := 4 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)-time.Second < budget {
		budget = time.Until(deadline) - time.Second
	}
	if budget <= 0 {
		return ctx.Err()
	}
	metricsCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				stats, err := b.containerStats(metricsCtx, containers[i].ID)
				if err == nil {
					containers[i].CPUPercent = stats.CPUPercent
					containers[i].MemoryBytes = stats.MemoryBytes
					containers[i].MetricsAvailable = true
				}
			}
		}()
	}
loop:
	for i := range containers {
		if containers[i].State != "running" {
			continue
		}
		select {
		case jobs <- i:
		case <-metricsCtx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("read container statistics: %w", err)
	}
	return nil
}

func (b *Backend) containerStats(ctx context.Context, id string) (api.Container, error) {
	result, err := b.docker.ContainerStats(ctx, id, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return api.Container{}, fmt.Errorf("read container statistics: %w", err)
	}
	defer result.Body.Close()
	data, err := readLimited(result.Body, 4<<20)
	if err != nil {
		return api.Container{}, fmt.Errorf("read container statistics: %w", err)
	}
	var s container.StatsResponse
	if err := json.Unmarshal(data, &s); err != nil {
		return api.Container{}, fmt.Errorf("decode container statistics: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return api.Container{}, fmt.Errorf("decode container statistics fields: %w", err)
	}
	fieldPresent := func(section, field string) bool {
		var group map[string]json.RawMessage
		if json.Unmarshal(fields[section], &group) != nil {
			return false
		}
		value, ok := group[field]
		return ok && string(value) != "null"
	}
	if s.PreRead.IsZero() {
		return api.Container{}, fmt.Errorf("container statistics lack a previous CPU sample")
	}
	current, previous := s.CPUStats.CPUUsage.TotalUsage, s.PreCPUStats.CPUUsage.TotalUsage
	if current < previous {
		return api.Container{}, fmt.Errorf("container CPU counter moved backwards")
	}
	var cpu float64
	if b.id == "windows" {
		elapsed := s.Read.Sub(s.PreRead).Nanoseconds()
		if elapsed <= 0 || s.NumProcs == 0 {
			return api.Container{}, fmt.Errorf("container statistics lack a previous CPU sample")
		}
		cpu = float64(current-previous) / (float64(elapsed) / 100 * float64(s.NumProcs)) * 100
	} else {
		cpus := s.CPUStats.OnlineCPUs
		if cpus == 0 {
			cpus = uint32(len(s.CPUStats.CPUUsage.PercpuUsage))
		}
		if cpus == 0 || s.CPUStats.SystemUsage <= s.PreCPUStats.SystemUsage {
			return api.Container{}, fmt.Errorf("container statistics lack a previous CPU sample")
		}
		cpu = float64(current-previous) / float64(s.CPUStats.SystemUsage-s.PreCPUStats.SystemUsage) * float64(cpus) * 100
	}
	memory := s.MemoryStats.Usage
	if b.id == "windows" {
		memory = s.MemoryStats.PrivateWorkingSet
	} else if cache, ok := s.MemoryStats.Stats["total_inactive_file"]; ok && cache < memory {
		memory -= cache
	} else if cache := s.MemoryStats.Stats["inactive_file"]; cache < memory {
		memory -= cache
	}
	out := api.Container{ID: id, Engine: b.id, CPUPercent: cpu, MemoryBytes: memory, SampleTime: s.Read, MetricsAvailable: true}
	if fieldPresent("memory_stats", "limit") {
		out.MemoryLimitBytes = &s.MemoryStats.Limit
	}
	if fieldPresent("pids_stats", "current") {
		out.Pids = &s.PidsStats.Current
	}
	var rx, tx uint64
	for _, network := range s.Networks {
		rx += network.RxBytes
		tx += network.TxBytes
	}
	if raw, ok := fields["networks"]; ok && string(raw) != "null" {
		var interfaces map[string]map[string]json.RawMessage
		if json.Unmarshal(raw, &interfaces) == nil {
			rxAvailable, txAvailable := true, true
			for _, counters := range interfaces {
				if value, exists := counters["rx_bytes"]; !exists || string(value) == "null" {
					rxAvailable = false
				}
				if value, exists := counters["tx_bytes"]; !exists || string(value) == "null" {
					txAvailable = false
				}
			}
			if rxAvailable {
				out.NetworkRxBytes = &rx
			}
			if txAvailable {
				out.NetworkTxBytes = &tx
			}
		}
	}
	if b.id == "windows" {
		if fieldPresent("storage_stats", "read_size_bytes") {
			out.BlockReadBytes = &s.StorageStats.ReadSizeBytes
		}
		if fieldPresent("storage_stats", "write_size_bytes") {
			out.BlockWriteBytes = &s.StorageStats.WriteSizeBytes
		}
	} else {
		var read, write uint64
		for _, entry := range s.BlkioStats.IoServiceBytesRecursive {
			switch strings.ToLower(entry.Op) {
			case "read":
				read += entry.Value
			case "write":
				write += entry.Value
			}
		}
		if fieldPresent("blkio_stats", "io_service_bytes_recursive") {
			out.BlockReadBytes, out.BlockWriteBytes = &read, &write
		}
	}
	return out, nil
}

func normalizeVolume(v volume.Volume, engine string) api.Volume {
	out := api.Volume{Name: v.Name, Engine: engine, Driver: v.Driver, Mountpoint: v.Mountpoint, Labels: v.Labels, Size: -1, UsageCount: -1}
	if v.UsageData != nil {
		out.Size, out.UsageCount = v.UsageData.Size, v.UsageData.RefCount
	}
	return out
}

func (b *Backend) volumeCall(ctx context.Context, req api.Request) (any, error) {
	switch {
	case req.Method == "volume.create", req.Method == "volume.inspect", strings.HasPrefix(req.Method, "volume.files."), req.Method == "volume.backup", req.Method == "volume.restore", req.Method == "volume.clone", req.Method == "volume.empty":
		return b.volumeResourceCall(ctx, req)
	}
	if req.Method == "volume.list" {
		result, err := b.docker.VolumeList(ctx, client.VolumeListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list volumes: %w", err)
		}
		containers, err := b.docker.ContainerList(ctx, client.ContainerListOptions{All: true})
		if err != nil {
			return nil, fmt.Errorf("check volume usage: %w", err)
		}
		usage := make(map[string]int64)
		for _, c := range containers.Items {
			seen := make(map[string]bool)
			for _, mount := range c.Mounts {
				if mount.Type == "volume" && mount.Name != "" && !seen[mount.Name] {
					usage[mount.Name]++
					seen[mount.Name] = true
				}
			}
		}
		out := make([]api.Volume, 0, len(result.Items))
		for _, v := range result.Items {
			item := normalizeVolume(v, b.id)
			item.UsageCount = usage[v.Name]
			out = append(out, item)
		}
		return out, nil
	}
	p, err := api.Decode[api.VolumeParams](req)
	if err != nil {
		return nil, err
	}
	if p.Name == "" {
		return nil, api.Fail("invalid_argument", "volume name is required")
	}
	switch req.Method {
	case "volume.remove":
		_, err := b.docker.VolumeRemove(ctx, p.Name, client.VolumeRemoveOptions{})
		if err != nil {
			return nil, fmt.Errorf("remove volume: %w", err)
		}
		return nil, nil
	default:
		return nil, api.Fail("invalid_argument", "unknown volume method: "+req.Method)
	}
}
