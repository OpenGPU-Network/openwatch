package registry

import (
	"fmt"
	"strings"
)

// Reference is a parsed container image reference broken into the pieces the
// rest of OpenWatch actually needs: the registry host, the repository path,
// and a tag (or digest).
type Reference struct {
	Registry   string // e.g. "registry-1.docker.io", "ghcr.io"
	Repository string // e.g. "library/nginx", "openwatch/openwatch"
	Tag        string // e.g. "latest", "1.2.3"
	Digest     string // populated when the ref was pinned by digest
}

const (
	defaultRegistry   = "registry-1.docker.io"
	defaultTag        = "latest"
	dockerHubShortcut = "docker.io"
)

// Parse splits a Docker image reference into its components, handling the
// usual shorthand conventions:
//
//   - "nginx"                 → registry-1.docker.io/library/nginx:latest
//   - "nginx:1.25"            → registry-1.docker.io/library/nginx:1.25
//   - "user/app:tag"          → registry-1.docker.io/user/app:tag
//   - "ghcr.io/org/app:tag"   → ghcr.io/org/app:tag
//   - "host:5000/img"         → host:5000/img:latest
//   - "repo@sha256:…"         → Digest set, Tag empty
func Parse(ref string) (*Reference, error) {
	if ref == "" {
		return nil, fmt.Errorf("empty image reference")
	}

	out := &Reference{}

	remainder := ref

	// Digest first — it's unambiguous.
	if idx := strings.Index(remainder, "@"); idx >= 0 {
		out.Digest = remainder[idx+1:]
		remainder = remainder[:idx]
	}

	// Decide whether the first path segment is a registry host or a Docker Hub
	// user/library namespace. Docker's rule: if it contains a "." or a ":" or
	// is literally "localhost", it's a registry.
	var registry, path string
	if slash := strings.Index(remainder, "/"); slash >= 0 {
		first := remainder[:slash]
		if first == "localhost" || strings.ContainsAny(first, ".:") {
			registry = first
			path = remainder[slash+1:]
		} else {
			registry = defaultRegistry
			path = remainder
		}
	} else {
		registry = defaultRegistry
		path = remainder
	}

	// Normalise the Docker Hub shorthand aliases to the canonical host.
	if registry == dockerHubShortcut || registry == "index.docker.io" {
		registry = defaultRegistry
	}

	// On Docker Hub, a bare name like "nginx" lives under the "library/" namespace.
	if registry == defaultRegistry && !strings.Contains(path, "/") {
		path = "library/" + path
	}

	// Split repository from tag.
	var repo, tag string
	if colon := strings.LastIndex(path, ":"); colon >= 0 && !strings.Contains(path[colon:], "/") {
		repo = path[:colon]
		tag = path[colon+1:]
	} else {
		repo = path
	}

	if repo == "" {
		return nil, fmt.Errorf("invalid image reference %q: empty repository", ref)
	}

	if tag == "" && out.Digest == "" {
		tag = defaultTag
	}

	out.Registry = registry
	out.Repository = repo
	out.Tag = tag
	return out, nil
}
