package snapshot

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// MockRegistryClient for testing
type MockRegistryClient struct {
	Tags    []string
	Digests map[string]string // Tag -> Digest
}

func (m *MockRegistryClient) ListTags(ctx context.Context, repo name.Repository) ([]string, error) {
	return m.Tags, nil
}

func (m *MockRegistryClient) GetDigest(ctx context.Context, ref name.Reference) (string, error) {
	tag := ref.Identifier()
	if d, ok := m.Digests[tag]; ok {
		return d, nil
	}
	return "", fmt.Errorf("digest not found for %s", tag)
}

func (m *MockRegistryClient) GetPlatformDigest(ctx context.Context, ref name.Reference, sys *v1.Platform) (string, error) {
	return m.GetDigest(ctx, ref)
}

func TestChooseCandidateTags(t *testing.T) {
	tests := []struct {
		name     string
		tags     []string
		expected []string
	}{
		{
			name:     "Sorts pure SemVer first descending",
			tags:     []string{"1.0.0", "1.1.0", "1.0.1"},
			expected: []string{"1.1.0", "1.0.1", "1.0.0"},
		},
		{
			name:     "Prioritizes pure SemVer over suffixed",
			tags:     []string{"1.0.0-amd64", "1.0.0", "1.1.0-arm64"},
			expected: []string{"1.0.0", "1.1.0-arm64", "1.0.0-amd64"},
		},
		{
			name:     "Handles v prefix",
			tags:     []string{"v1.0.0", "1.1.0"},
			expected: []string{"1.1.0", "v1.0.0"},
		},
		{
			name:     "Filters valid versions only",
			tags:     []string{"latest", "stable", "1.0.0", "random-tag", "v2.0"},
			expected: []string{"v2.0", "1.0.0"},
		},
		{
			name: "Mixed complex case",
			tags: []string{
				"0.14.0",
				"0.14.0-tensorrt",
				"0.14.1",
				"0.13.0",
				"latest",
				"0.14.1-da53865",
			},
			expected: []string{
				"0.14.1",
				"0.14.0",
				"0.13.0",
				"0.14.1-da53865",
				"0.14.0-tensorrt",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChooseCandidateTags(tt.tags)
			if len(got) != len(tt.expected) {
				t.Fatalf("Expected %d tags, got %d: %v", len(tt.expected), len(got), got)
			}
			for i, v := range got {
				if v != tt.expected[i] {
					t.Errorf("Index %d: expected %s, got %s", i, tt.expected[i], v)
				}
			}
		})
	}
}

func TestFindMatchingTag(t *testing.T) {
	mockClient := &MockRegistryClient{
		Tags: []string{"1.0.0", "1.0.1", "1.1.0"},
		Digests: map[string]string{
			"1.0.0": "sha256:old",
			"1.0.1": "sha256:current",
			"1.1.0": "sha256:new",
		},
	}

	repo, _ := name.NewRepository("test/repo")
	ctx := context.Background()

	tests := []struct {
		name         string
		validDigests []string
		want         string
	}{
		{
			name:         "Matches exact digest",
			validDigests: []string{"sha256:current"},
			want:         "1.0.1",
		},
		{
			name:         "Matches repo digest format",
			validDigests: []string{"repo@sha256:current"},
			want:         "1.0.1",
		},
		{
			name:         "No match",
			validDigests: []string{"sha256:missing"},
			want:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindMatchingTag(ctx, mockClient, repo, mockClient.Tags, tt.validDigests, nil)
			if got != tt.want {
				t.Errorf("FindMatchingTag() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetRemoteVersion(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		imgMeta  InspectedImage
		want     string
		mockTags []string
	}{
		{
			name: "Returns empty if ID is not sha256",
			imgMeta: InspectedImage{
				ID: "shortid",
			},
			want: "",
		},
		{
			name: "Returns empty if no repo digests",
			imgMeta: InspectedImage{
				ID: "sha256:digest",
			},
			want: "",
		},
		{
			name: "Uses RepoTags[0] for lookup",
			imgMeta: InspectedImage{
				ID:          "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				RepoTags:    []string{"nginx:latest"},
				RepoDigests: []string{"nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			},
			mockTags: []string{"1.20.0"},
			want:     "1.20.0",
		},
		{
			name: "Fallback to RepoDigests if RepoTags empty",
			imgMeta: InspectedImage{
				ID:          "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				RepoDigests: []string{"nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			},
			mockTags: []string{"1.20.0"},
			want:     "1.20.0",
		},
		{
			name: "Returns empty if resolved name is still a digest",
			imgMeta: InspectedImage{
				ID:          "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				RepoTags:    []string{"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
				RepoDigests: []string{"nginx@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &MockRegistryClient{
				Tags: tt.mockTags,
				Digests: map[string]string{
					"1.20.0": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				},
			}

			got, err := GetRemoteVersion(ctx, client, tt.imgMeta)
			if err != nil {
				t.Errorf("GetRemoteVersion() returned unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetRemoteVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}
