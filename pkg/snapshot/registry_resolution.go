package snapshot

import (
	"context"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/mod/semver"
	"golang.org/x/sync/errgroup"
)

// RegistryClient defines the interface for registry interactions, enabling mocking.
type RegistryClient interface {
	ListTags(ctx context.Context, repo name.Repository) ([]string, error)
	GetDigest(ctx context.Context, ref name.Reference) (string, error)
	GetPlatformDigest(ctx context.Context, ref name.Reference, sys *v1.Platform) (string, error)
}

// RealRegistryClient is the actual implementation using go-containerregistry.
type RealRegistryClient struct{}

// ListTags retrieves all tags for a given repository.
func (c *RealRegistryClient) ListTags(ctx context.Context, repo name.Repository) ([]string, error) {
	return remote.List(repo, remote.WithContext(ctx))
}

// GetDigest retrieves the manifest digest for a specific tag or reference.
func (c *RealRegistryClient) GetDigest(ctx context.Context, ref name.Reference) (string, error) {
	desc, err := remote.Head(ref, remote.WithContext(ctx))
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// GetPlatformDigest retrieves the manifest digest for a reference targeting a specific architecture and OS.
func (c *RealRegistryClient) GetPlatformDigest(ctx context.Context, ref name.Reference, sys *v1.Platform) (string, error) {
	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithPlatform(*sys))
	if err != nil {
		return "", err
	}
	return desc.Digest.String(), nil
}

// isIgnoredTag checks for non-specific tags
func isIgnoredTag(t string) bool {
	switch t {
	case "latest", "stable", "master", "main", "nightly":
		return true
	}
	return false
}

// ChooseCandidateTags filters and sorts tags.
func ChooseCandidateTags(tags []string) []string {
	var pureSemVer []string
	var otherVersionLike []string

	for _, t := range tags {
		if isIgnoredTag(t) {
			continue
		}
		if !matchSemver(t) {
			continue
		}

		if isPureSemVer(t) {
			pureSemVer = append(pureSemVer, t)
		} else {
			otherVersionLike = append(otherVersionLike, t)
		}
	}

	sort.Slice(pureSemVer, func(i, j int) bool {
		return semver.Compare(normalizeSemVer(pureSemVer[i]), normalizeSemVer(pureSemVer[j])) > 0
	})

	sort.Slice(otherVersionLike, func(i, j int) bool {
		vI := normalizeSemVer(otherVersionLike[i])
		vJ := normalizeSemVer(otherVersionLike[j])
		c := semver.Compare(vI, vJ)
		if c != 0 {
			return c > 0
		}
		return otherVersionLike[i] > otherVersionLike[j]
	})

	candidates := append(pureSemVer, otherVersionLike...)
	if len(candidates) > 300 {
		candidates = candidates[:300]
	}
	return candidates
}

// FindMatchingTag checks the candidate tags against the valid digests.
func FindMatchingTag(ctx context.Context, client RegistryClient, repo name.Repository, candidateTags []string, validDigests []string, platform *v1.Platform) string {
	repoDigests := make(map[string]bool)
	for _, d := range validDigests {
		repoDigests[extractDigest(d)] = true
	}

	var g errgroup.Group
	g.SetLimit(10)

	var foundTag string
	var mu sync.Mutex

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	checkAndSetFound := func(tag, digest string) bool {
		if !repoDigests[digest] {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if foundTag == "" {
			foundTag = tag
			cancel()
		}
		return true
	}

	for _, t := range candidateTags {
		tag := t
		g.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}

			tagRef := repo.Tag(tag)

			if digest, err := client.GetDigest(ctx, tagRef); err == nil {
				if checkAndSetFound(tag, digest) {
					return nil
				}
			}

			if platform != nil {
				if platDigest, err := client.GetPlatformDigest(ctx, tagRef, platform); err == nil {
					if checkAndSetFound(tag, platDigest) {
						return nil
					}
				}
			}
			return nil
		})
	}

	_ = g.Wait()
	return foundTag
}

func matchSemver(s string) bool {
	matched, _ := regexp.MatchString(`\d+\.\d+`, s)
	return matched
}

func normalizeSemVer(v string) string {
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

func isPureSemVer(v string) bool {
	normalized := normalizeSemVer(v)
	if !semver.IsValid(normalized) {
		return false
	}
	return semver.Prerelease(normalized) == "" && semver.Build(normalized) == ""
}

// ResolveVersionFromRegistry attempts to find a human-readable tag (like "1.2.3")
// that matches the image's specific digest.
func ResolveVersionFromRegistry(ctx context.Context, image string, validDigests []string, client RegistryClient, architecture, os string) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", err
	}
	repo := ref.Context()

	tags, err := client.ListTags(ctx, repo)
	if err != nil {
		return "", err
	}
	log.Printf("Found %d tags for %s", len(tags), repo.String())

	candidateTags := ChooseCandidateTags(tags)
	log.Printf("Found %d candidate tags for %s", len(candidateTags), repo.String())

	platform := getPlatform(architecture, os)

	return FindMatchingTag(ctx, client, repo, candidateTags, validDigests, platform), nil
}

func getPlatform(architecture, os string) *v1.Platform {
	if architecture == "" || os == "" {
		return nil
	}
	return &v1.Platform{
		Architecture: architecture,
		OS:           os,
	}
}

func extractDigest(repoDigest string) string {
	if idx := strings.LastIndex(repoDigest, "@"); idx != -1 {
		return repoDigest[idx+1:]
	}
	return repoDigest
}

// GetRemoteVersion attempts to resolve a human-readable version from a remote registry
func GetRemoteVersion(ctx context.Context, client RegistryClient, imgMeta InspectedImage) (string, error) {
	imageName := ""
	if len(imgMeta.RepoTags) > 0 {
		imageName = imgMeta.RepoTags[0]
	} else if len(imgMeta.RepoDigests) > 0 {
		imageName = imgMeta.RepoDigests[0]
	}

	if imageName == "" || strings.HasPrefix(imageName, sha256Prefix) {
		return "", nil
	}

	return ResolveVersionFromRegistry(ctx, imageName, imgMeta.RepoDigests, client, imgMeta.Architecture, imgMeta.Os)
}
