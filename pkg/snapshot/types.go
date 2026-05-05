package snapshot

import "time"

// InspectedContainer represents the subset of container data we need.
type InspectedContainer struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	ImageDigest string `json:"Image"` // The Image ID (e.g. "sha256:...")
	Config      struct {
		ImageName string            `json:"Image"` // The declared Image name from config (e.g. "nginx:latest")
		Labels    map[string]string `json:"Labels"`
	} `json:"Config"`
}

// InspectedImage represents the subset of image data we need.
type InspectedImage struct {
	ID           string   `json:"Id"`          // The Image ID (e.g. "sha256:...")
	RepoTags     []string `json:"RepoTags"`    // List of tags associated with this image (e.g. ["nginx:latest"])
	RepoDigests  []string `json:"RepoDigests"` // List of repo digests (e.g. ["nginx@sha256:..."])
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
}

// VersionDetails holds structured version information
type VersionDetails struct {
	Created  string   `json:"created,omitempty"`
	OCI      string   `json:"oci,omitempty"`
	Raw      string   `json:"raw,omitempty"`
	RepoTags []string `json:"other_repo_tags,omitempty"`
	Other    []string `json:"other,omitempty"`
}

// RemoteDetails holds version information from remote registry
type RemoteDetails struct {
	Version string `json:"version,omitempty"`
}

// ContainerInfo represents a row in the final JSON output
type ContainerInfo struct {
	Host       string         `json:"host"`
	Container  string         `json:"container"`
	ImageLabel string         `json:"image_label"`
	OnHost     VersionDetails `json:"on_host_versioning"`
	Remote     *RemoteDetails `json:"remote_registry_info,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// Snapshot is the top-level JSON output structure.
type Snapshot struct {
	Timestamp  time.Time       `json:"timestamp"`
	Targets    []string        `json:"targets"`
	Containers []ContainerInfo `json:"containers"`
}
