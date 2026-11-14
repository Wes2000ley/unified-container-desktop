package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

func (b *Backend) storageCall(ctx context.Context, req api.Request) (any, error) {
	var p api.StoragePruneParams
	var err error
	if req.Method == "storage.preview" || req.Method == "storage.prune" {
		params := req.Params
		if len(params) == 0 {
			params = []byte(`{}`)
		}
		if !json.Valid(params) {
			return nil, api.Fail("invalid_argument", "invalid "+req.Method+" parameters: invalid JSON")
		}
		decoder := json.NewDecoder(bytes.NewReader(params))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&p); err != nil {
			return nil, api.Fail("invalid_argument", fmt.Sprintf("invalid %s parameters: %v", req.Method, err))
		}
	} else {
		p, err = api.Decode[api.StoragePruneParams](req)
	}
	if err != nil {
		return nil, err
	}
	var filters client.Filters
	if req.Method == "storage.preview" || req.Method == "storage.prune" {
		filters, err = storagePruneFilters(p.Scope, p.Filters)
		if err != nil {
			return nil, err
		}
	}
	if req.Method == "storage.df" || req.Method == "storage.preview" {
		disk, err := b.docker.DiskUsage(ctx, client.DiskUsageOptions{Containers: true, Images: true, Volumes: true, BuildCache: true})
		if err != nil {
			return nil, fmt.Errorf("inspect Docker disk usage: %w", err)
		}
		usage := api.StorageDiskUsage{
			Containers: api.StorageUsage{TotalCount: disk.Containers.TotalCount, ActiveCount: disk.Containers.ActiveCount, Reclaimable: disk.Containers.Reclaimable, TotalSize: disk.Containers.TotalSize},
			Images:     api.StorageUsage{TotalCount: disk.Images.TotalCount, ActiveCount: disk.Images.ActiveCount, Reclaimable: disk.Images.Reclaimable, TotalSize: disk.Images.TotalSize},
			Volumes:    api.StorageUsage{TotalCount: disk.Volumes.TotalCount, ActiveCount: disk.Volumes.ActiveCount, Reclaimable: disk.Volumes.Reclaimable, TotalSize: disk.Volumes.TotalSize},
			BuildCache: api.StorageUsage{TotalCount: disk.BuildCache.TotalCount, ActiveCount: disk.BuildCache.ActiveCount, Reclaimable: disk.BuildCache.Reclaimable, TotalSize: disk.BuildCache.TotalSize},
		}
		if req.Method == "storage.df" {
			return usage, nil
		}
		preview := api.StoragePreview{Scope: p.Scope, All: p.All}
		switch p.Scope {
		case "containers":
			preview.TotalCount, preview.ActiveCount, preview.Reclaimable = usage.Containers.TotalCount, usage.Containers.ActiveCount, usage.Containers.Reclaimable
			preview.Description = "Remove all stopped containers."
		case "images":
			preview.TotalCount, preview.ActiveCount, preview.Reclaimable = usage.Images.TotalCount, usage.Images.ActiveCount, usage.Images.Reclaimable
			preview.Description = "Remove dangling images."
			if p.All {
				preview.Description = "Remove all images not used by a container."
			}
		case "networks":
			preview.Description = "Remove networks not used by any container."
		case "volumes":
			preview.TotalCount, preview.ActiveCount, preview.Reclaimable = usage.Volumes.TotalCount, usage.Volumes.ActiveCount, usage.Volumes.Reclaimable
			preview.Description = "Remove unused anonymous volumes; named volumes are preserved."
			if p.All {
				preview.Description = "Remove all unused volumes, including named volumes and their data."
			}
		case "build-cache":
			preview.TotalCount, preview.ActiveCount, preview.Reclaimable = usage.BuildCache.TotalCount, usage.BuildCache.ActiveCount, usage.BuildCache.Reclaimable
			preview.Description = "Remove dangling build cache."
			if p.All {
				preview.Description = "Remove all unused build cache."
			}
		default:
			return nil, api.Fail("invalid_argument", "invalid prune scope")
		}
		if p.Scope == "networks" {
			preview.Description += " Docker does not provide a network space estimate."
		} else {
			preview.Description += " Reclaimable space is an aggregate estimate and may differ from the actual prune result."
		}
		if len(filters) > 0 {
			preview.Description += " Filters restrict the prune; these counts and estimates are not filtered."
		}
		return preview, nil
	}
	if req.Method != "storage.prune" {
		return nil, api.Fail("invalid_argument", "unknown storage method: "+req.Method)
	}
	if !p.Confirmed {
		return nil, api.Fail("confirmation_required", "confirm the selected prune scope before deleting data")
	}
	switch p.Scope {
	case "containers":
		result, err := b.docker.ContainerPrune(ctx, client.ContainerPruneOptions{Filters: filters})
		if err != nil {
			return nil, fmt.Errorf("prune containers: %w", err)
		}
		return result.Report, nil
	case "images":
		if p.All {
			filters.Add("dangling", "false")
		}
		result, err := b.docker.ImagePrune(ctx, client.ImagePruneOptions{Filters: filters})
		if err != nil {
			return nil, fmt.Errorf("prune images: %w", err)
		}
		return result.Report, nil
	case "networks":
		result, err := b.docker.NetworkPrune(ctx, client.NetworkPruneOptions{Filters: filters})
		if err != nil {
			return nil, fmt.Errorf("prune networks: %w", err)
		}
		return result.Report, nil
	case "volumes":
		result, err := b.docker.VolumePrune(ctx, client.VolumePruneOptions{All: p.All, Filters: filters})
		if err != nil {
			return nil, fmt.Errorf("prune volumes: %w", err)
		}
		return result.Report, nil
	case "build-cache":
		result, err := b.docker.BuildCachePrune(ctx, client.BuildCachePruneOptions{All: p.All, Filters: filters})
		if err != nil {
			return nil, fmt.Errorf("prune build cache: %w", err)
		}
		return result.Report, nil
	default:
		return nil, api.Fail("invalid_argument", "invalid prune scope")
	}
}

// Docker's builder accepts label filters but can ignore them; never let a
// caller believe those labels restrict a cache prune.
func storagePruneFilters(scope string, supplied map[string][]string) (client.Filters, error) {
	var allowed map[string]bool
	switch scope {
	case "containers", "images", "networks":
		allowed = map[string]bool{"label": true, "label!": true, "until": true}
	case "volumes":
		allowed = map[string]bool{"label": true, "label!": true}
	case "build-cache":
		allowed = map[string]bool{"id": true, "type": true, "description": true, "until": true, "unused-for": true}
	default:
		return nil, api.Fail("invalid_argument", "invalid prune scope")
	}
	filters := client.Filters{}
	for key, values := range supplied {
		if !allowed[key] {
			return nil, api.Fail("invalid_argument", fmt.Sprintf("filter %q is not supported for %s pruning", key, scope))
		}
		if key == "label!" && len(values) > 1 {
			return nil, api.Fail("invalid_argument", "only one label! exclusion is accepted per prune")
		}
		if len(values) == 0 || scope == "build-cache" && len(values) > 1 {
			return nil, api.Fail("invalid_argument", fmt.Sprintf("invalid values for prune filter %q", key))
		}
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return nil, api.Fail("invalid_argument", fmt.Sprintf("prune filter %q must not be empty", key))
			}
			filters.Add(key, value)
		}
	}
	return filters, nil
}
