package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/image"
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

// LocalImageDigest returns the image config digest (Docker's ".Id" field) for
// the image pinned to ref. This is the SHA256 of the image config blob — the
// canonical identifier Docker reports as .Id.
//
// We compare config digests (not RepoDigests / manifest-list digests) against
// FetchRemoteConfigDigest in the registry package because it's the only
// apples-to-apples comparison that works across both single-arch and
// multi-arch images, and regardless of which Docker Engine version pulled
// them. RepoDigest behaviour has drifted between Docker releases; config
// digests are stable.
func LocalImageDigest(ctx context.Context, cli *client.Client, ref string) (string, error) {
	img, err := cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect image %s: %w", ref, err)
	}
	if img.ID == "" {
		return "", fmt.Errorf("image %s has no ID", ref)
	}
	return img.ID, nil
}

// RemoveImage removes an image by ID. Errors are returned to the caller but
// the caller should log-and-continue rather than crash.
func RemoveImage(ctx context.Context, cli *client.Client, imageID string) error {
	_, err := cli.ImageRemove(ctx, imageID, image.RemoveOptions{Force: false, PruneChildren: true})
	if err != nil {
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
