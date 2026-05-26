package snapshot

import "time"

// InspectedContainer represents the subset of container data we need.
type InspectedContainer struct {
	// ID is the unique identifier for the container.
	ID string `json:"Id"`
	// Name is the human-readable name assigned to the container.
	Name string `json:"Name"`
	// ImageDigest is the Image ID (e.g. "sha256:...").
	ImageDigest string `json:"Image"`
	// Config contains the container configuration metadata.
	Config struct {
		// ImageName is the declared Image name from config (e.g. "nginx:latest").
		ImageName string `json:"Image"`
		// Labels is a map of key-value pairs applied to the container.
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// InspectedImage represents the subset of image data we need.
type InspectedImage struct {
	// ID is the Image ID (e.g. "sha256:...").
	ID string `json:"Id"`
	// RepoTags is a list of tags associated with this image (e.g. ["nginx:latest"]).
	RepoTags []string `json:"RepoTags"`
	// RepoDigests is a list of repo digests (e.g. ["nginx@sha256:..."]).
	RepoDigests []string `json:"RepoDigests"`
	// Architecture is the hardware architecture the image is built for.
	Architecture string `json:"Architecture"`
	// Os is the operating system the image is built for.
	Os string `json:"Os"`
}

// VersionDetails holds structured version information
type VersionDetails struct {
	// Created is the timestamp when the image was created.
	Created string `json:"created,omitempty"`
	// OCI is the version extracted from org.opencontainers.image.version label.
	OCI string `json:"oci,omitempty"`
	// Raw is the raw image digest string.
	Raw string `json:"raw,omitempty"`
	// RepoTags contains any tags associated with the image on the host.
	RepoTags []string `json:"other_repo_tags,omitempty"`
	// Other contains version strings extracted from fallback labels.
	Other []string `json:"other,omitempty"`
}

// RemoteDetails holds version information from remote registry
type RemoteDetails struct {
	// Version is the human-readable version resolved from the registry.
	Version string `json:"version,omitempty"`
}

// ContainerInfo represents a row in the final JSON output
type ContainerInfo struct {
	// Host is the name or IP of the scanned target.
	Host string `json:"host"`
	// Container is the name of the container.
	Container string `json:"container"`
	// ImageLabel is the declared image name from the container's config.
	ImageLabel string `json:"image_label"`
	// OnHost contains version information extracted directly from the host.
	OnHost VersionDetails `json:"on_host_versioning"`
	// Remote contains version information resolved from the remote registry.
	Remote *RemoteDetails `json:"remote_registry_info,omitempty"`
	// Error contains any error encountered during version resolution for this container.
	Error string `json:"error,omitempty"`
}

// Target represents a scanned host and its context
type Target struct {
	// Host is the address or alias of the target machine.
	Host string `json:"host"`
	// User is the SSH username used for the connection.
	User string `json:"user,omitempty"`
	// Sudo indicates whether podman commands are run with elevated privileges.
	Sudo bool `json:"sudo"`
}

// Snapshot is the top-level JSON output structure.
type Snapshot struct {
	// Timestamp is the time when the snapshot was generated.
	Timestamp time.Time `json:"timestamp"`
	// Targets is the list of scanned hosts.
	Targets []Target `json:"targets"`
	// Containers is the aggregated list of container version data across all targets.
	Containers []ContainerInfo `json:"containers"`
}
