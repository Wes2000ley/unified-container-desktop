package api

import "github.com/moby/moby/api/types/network"

// ImageResourceParams describes image operations and bounded archive transfers.
type ImageResourceParams struct {
	Reference      string            `json:"reference,omitempty"`
	Target         string            `json:"target,omitempty"`
	References     []string          `json:"references,omitempty"`
	Data           []byte            `json:"data,omitempty"`
	Dockerfile     string            `json:"dockerfile,omitempty"`
	DockerfilePath string            `json:"dockerfilePath,omitempty"`
	Files          []File            `json:"files,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
	BuildArgs      map[string]string `json:"buildArgs,omitempty"`
	OperationID    string            `json:"operationId,omitempty"`
}

type ResourceOutput struct {
	Output string `json:"output"`
}
type ResourceData struct {
	Data []byte `json:"data"`
	Name string `json:"name,omitempty"`
}

type RegistryParams struct {
	Server   string `json:"server"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// RegistryCredentialParams is the Docker credential-helper wire value. Secret
// is never included in a list response or event.
type RegistryCredentialParams struct {
	ServerURL string `json:"serverURL"`
	Username  string `json:"username,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

// RegistryAuthParams is used by internal controller calls to save a credential
// after an available engine has validated it.
type RegistryAuthParams struct {
	Server string       `json:"server"`
	Auth   RegistryAuth `json:"auth"`
}

// RegistryAuth is a credential snapshot sent only from the Windows controller
// to its mutually authenticated Linux helper for Compose and Engine SDK calls.
type RegistryAuth struct {
	Server        string `json:"server,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identityToken,omitempty"`
	RegistryToken string `json:"registryToken,omitempty"`
}

// RegistrySnapshot carries the machine credential store to the Linux helper.
// It is an internal controller-to-helper value, never a GUI response.
type RegistrySnapshot struct {
	AuthConfigs map[string]RegistryAuth `json:"authConfigs"`
}

// RegistryOperation carries one engine request and the matching registry
// snapshot over the controller's authenticated helper channel.
type RegistryOperation struct {
	Request  Request          `json:"request"`
	Snapshot RegistrySnapshot `json:"snapshot"`
}

type NetworkParams struct {
	ID         string            `json:"id,omitempty"`
	Name       string            `json:"name,omitempty"`
	Driver     string            `json:"driver,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	EnableIPv4 *bool             `json:"enableIPv4,omitempty"`
	EnableIPv6 *bool             `json:"enableIPv6,omitempty"`
	IPAM       *network.IPAM     `json:"ipam,omitempty"`
	Container  string            `json:"container,omitempty"`
	Aliases    []string          `json:"aliases,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Force      bool              `json:"force,omitempty"`
}

type Network struct {
	network.Summary
	Engine string `json:"engine"`
}

type StoragePruneParams struct {
	Scope     string              `json:"scope"`
	All       bool                `json:"all,omitempty"`
	Filters   map[string][]string `json:"filters,omitempty"`
	Confirmed bool                `json:"confirmed,omitempty"`
}

// StorageUsage is an aggregate and contains no individual resource metadata.
type StorageUsage struct {
	TotalCount  int64 `json:"TotalCount"`
	ActiveCount int64 `json:"ActiveCount"`
	Reclaimable int64 `json:"Reclaimable"`
	TotalSize   int64 `json:"TotalSize"`
}

type StorageDiskUsage struct {
	Containers StorageUsage `json:"Containers"`
	Images     StorageUsage `json:"Images"`
	Volumes    StorageUsage `json:"Volumes"`
	BuildCache StorageUsage `json:"BuildCache"`
}

type StoragePreview struct {
	Scope       string `json:"scope"`
	All         bool   `json:"all"`
	TotalCount  int64  `json:"totalCount"`
	ActiveCount int64  `json:"activeCount"`
	Reclaimable int64  `json:"reclaimable"`
	Description string `json:"description"`
}

type VolumeResourceParams struct {
	Name       string            `json:"name"`
	Target     string            `json:"target,omitempty"`
	Driver     string            `json:"driver,omitempty"`
	DriverOpts map[string]string `json:"driverOpts,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Path       string            `json:"path,omitempty"`
	Data       []byte            `json:"data,omitempty"`
	Mode       uint32            `json:"mode,omitempty"`
	Recursive  bool              `json:"recursive,omitempty"`
	Confirmed  bool              `json:"confirmed,omitempty"`
}

type VolumeDetails struct {
	Volume     Volume      `json:"volume"`
	Containers []Container `json:"containers"`
}
