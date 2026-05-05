package snapshot

import (
	"context"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// MockResolutionClient for testing
type MockResolutionClient struct{}

func (m *MockResolutionClient) ListTags(ctx context.Context, repo name.Repository) ([]string, error) {
	return []string{}, nil
}

func (m *MockResolutionClient) GetDigest(ctx context.Context, ref name.Reference) (string, error) {
	return "sha256:mockdigest", nil
}

func (m *MockResolutionClient) GetPlatformDigest(ctx context.Context, ref name.Reference, sys *v1.Platform) (string, error) {
	return "sha256:mockplatformdigest", nil
}

func TestGetVersionFromInspection(t *testing.T) {
	images := []InspectedImage{
		{
			ID:       "sha256:image1",
			RepoTags: []string{"my-image:1.2.3", "my-image:latest"},
		},
		{
			ID:          "sha256:image2",
			RepoDigests: []string{"repo@sha256:digest"},
		},
	}

	tests := []struct {
		name      string
		container InspectedContainer
		images    []InspectedImage
		expected  VersionDetails
	}{
		{
			name: "Prioritizes OCI Label",
			container: InspectedContainer{
				ImageDigest: "sha256:image1",
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					Labels: map[string]string{
						"org.opencontainers.image.version": "OCI-v1",
						"version":                          "fallback-v1",
					},
				},
			},
			images: images,
			expected: VersionDetails{
				OCI:      "OCI-v1",
				Raw:      "sha256:image1",
				RepoTags: []string{"my-image:1.2.3", "my-image:latest"},
				Other:    []string{"fallback-v1"},
			},
		},
		{
			name: "Uses RepoTags if OCI missing",
			container: InspectedContainer{
				ImageDigest: "sha256:image1",
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					Labels: map[string]string{},
				},
			},
			images: images,
			expected: VersionDetails{
				Raw:      "sha256:image1",
				RepoTags: []string{"my-image:1.2.3", "my-image:latest"},
			},
		},
		{
			name: "Uses Fallback Label if RepoTags missing",
			container: InspectedContainer{
				ImageDigest: "sha256:missing-image",
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					Labels: map[string]string{
						"io.hass.version": "HASS-v1",
					},
				},
			},
			images: images,
			expected: VersionDetails{
				Raw:   "sha256:missing-image",
				Other: []string{"HASS-v1"},
			},
		},
		{
			name: "Falls back to Empty if no local versions",
			container: InspectedContainer{
				ImageDigest: "sha256:1234567890abcdef",
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					Labels: map[string]string{},
				},
			},
			images: images,
			expected: VersionDetails{
				Raw: "sha256:1234567890abcdef",
			},
		},
		{
			name: "Filters out Config ImageName from RepoTags",
			container: InspectedContainer{
				ImageDigest: "sha256:image1",
				Config: struct {
					ImageName string            `json:"Image"`
					Labels    map[string]string `json:"Labels"`
				}{
					ImageName: "my-image:latest",
					Labels:    map[string]string{},
				},
			},
			images: images,
			expected: VersionDetails{
				Raw:      "sha256:image1",
				RepoTags: []string{"my-image:1.2.3"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var imgMeta InspectedImage
			for _, img := range tt.images {
				if img.ID == tt.container.ImageDigest {
					imgMeta = img
					break
				}
			}

			got := GetVersionFromInspection(tt.container, imgMeta)

			if got.Created != tt.expected.Created {
				t.Errorf("Created: got %v, want %v", got.Created, tt.expected.Created)
			}
			if got.OCI != tt.expected.OCI {
				t.Errorf("OCI: got %v, want %v", got.OCI, tt.expected.OCI)
			}
			if got.Raw != tt.expected.Raw {
				t.Errorf("Raw: got %v, want %v", got.Raw, tt.expected.Raw)
			}
			if !equalStringSlices(got.RepoTags, tt.expected.RepoTags) {
				t.Errorf("RepoTags: got %v, want %v", got.RepoTags, tt.expected.RepoTags)
			}
			if !equalStringSlices(got.Other, tt.expected.Other) {
				t.Errorf("Other: got %v, want %v", got.Other, tt.expected.Other)
			}
		})
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	mA := make(map[string]int)
	for _, x := range a {
		mA[x]++
	}
	for _, x := range b {
		mA[x]--
		if mA[x] < 0 {
			return false
		}
	}
	return true
}
