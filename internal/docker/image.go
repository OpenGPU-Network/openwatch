package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
)

// PullImage pulls the given image reference. authConfig may be nil for
// anonymous pulls. The pull stream is drained so the daemon finishes the
// operation before we return.
func PullImage(ctx context.Context, cli *client.Client, ref string, authConfig *registry.AuthConfig) error {
	opts := image.PullOptions{}
	if authConfig != nil {
		encoded, err := encodeAuth(authConfig)
		if err != nil {
			return fmt.Errorf("encode auth: %w", err)
		}
		opts.RegistryAuth = encoded
	}

	rc, err := cli.ImagePull(ctx, ref, opts)
	if err != nil {
		return fmt.Errorf("pull image %s: %w", ref, err)
	}
	defer rc.Close()

	// Draining is required — ImagePull returns as soon as the stream opens,
	// and the daemon only completes the pull once we read the stream to EOF.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("drain pull stream for %s: %w", ref, err)
	}
	return nil
}

// LocalImageIdentifiers returns every sha256 identifier Docker has locally
// recorded for ref. We return a set rather than a single value because what
// counts as "the" identifier has changed between Docker releases:
//
//   - Traditional Docker Engine (pre-containerd store): .Id is the SHA256 of
//     the image config blob.
//   - Docker Desktop with the containerd image store (default since 27.x):
//     .Id is the top-level manifest / OCI index digest — the same value the
//     registry returns in the Docker-Content-Digest header at the tag.
//   - After any pull or push, .RepoDigests carries repo@sha256:<digest>,
//     where the digest is whatever the registry handed back — leaf manifest
//     digest for an old-Docker multi-arch pull, index digest for a modern
//     containerd pull.
//
// The watcher treats the image as up-to-date if any one of these locally known
// identifiers matches any of the identifiers the registry advertises, which
// removes the need to reason about which Docker version wrote the local state.
func LocalImageIdentifiers(ctx context.Context, cli *client.Client, ref string) ([]string, error) {
	seen := make(map[string]struct{}, 4)
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	// Always include the ref itself when it looks like a digest. This
	// covers the case where the caller passes a container's ImageID
	// (sha256:…) and the image record has been garbage-collected after
	// the tag moved to a newer pull — the container is still running
	// on the old layers but ImageInspect will 404.
	if strings.HasPrefix(ref, "sha256:") {
		add(ref)
	}

	img, err := cli.ImageInspect(ctx, ref)
	if err == nil {
		add(img.ID)
		for _, rd := range img.RepoDigests {
			if i := strings.Index(rd, "@"); i >= 0 {
				add(rd[i+1:])
			}
		}
	}

	if len(ids) == 0 {
		return nil, fmt.Errorf("image %s has no identifiers", ref)
	}
	return ids, nil
}

// RemoveImage removes an image by ID. Errors are returned to the caller but
// the caller should log-and-continue rather than crash.
func RemoveImage(ctx context.Context, cli *client.Client, imageID string) error {
	_, err := cli.ImageRemove(ctx, imageID, image.RemoveOptions{Force: false, PruneChildren: true})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("remove image %s: %w", imageID, err)
	}
	return nil
}

func encodeAuth(auth *registry.AuthConfig) (string, error) {
	buf, err := json.Marshal(auth)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}
