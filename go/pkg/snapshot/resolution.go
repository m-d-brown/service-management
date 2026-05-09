package snapshot

const (
	sha256Prefix = "sha256:"
)

// GetVersionFromInspection gathers all potential version strings for a container from local metadata.
func GetVersionFromInspection(c InspectedContainer, imgMeta InspectedImage) VersionDetails {
	details := VersionDetails{
		Raw: c.ImageDigest,
		// Metadata from Labels
		Created: c.Config.Labels["org.opencontainers.image.created"],
		OCI:     c.Config.Labels["org.opencontainers.image.version"],
	}

	// RepoTags
	// Filter out the main image name from RepoTags to avoid duplication
	for _, tag := range imgMeta.RepoTags {
		if tag != c.Config.ImageName {
			details.RepoTags = append(details.RepoTags, tag)
		}
	}

	// Fallback Labels
	details.Other = append(details.Other, otherVersionsFromLabels(c.Config.Labels)...)

	// Deduplicate
	details.RepoTags = uniqueStrings(details.RepoTags)
	details.Other = uniqueStrings(details.Other)
	return details
}

// FindImageWithDigest finds an image metadata object with the matching ID/Digest.
func FindImageWithDigest(images []InspectedImage, digest string) (InspectedImage, bool) {
	for _, img := range images {
		if img.ID == digest {
			return img, true
		}
	}
	return InspectedImage{}, false
}

func uniqueStrings(input []string) []string {
	keys := make(map[string]bool)
	var list []string
	for _, entry := range input {
		if _, value := keys[entry]; !value {
			keys[entry] = true
			list = append(list, entry)
		}
	}
	return list
}

func otherVersionsFromLabels(labels map[string]string) []string {
	fallbackLabels := []string{"io.hass.version", "version", "image.version"}
	var found []string
	for _, l := range fallbackLabels {
		if v, ok := labels[l]; ok && v != "" {
			found = append(found, v)
		}
	}
	return found
}
