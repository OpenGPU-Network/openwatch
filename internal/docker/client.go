package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/client"
)

// NewClient builds a Docker Engine API client using environment-based
// configuration (DOCKER_HOST, DOCKER_TLS_VERIFY, etc.) and API version
// negotiation. Negotiation is the core difference from Watchtower: it lets
// OpenWatch talk to any Docker Engine from 19.03 through 29+ without pinning.
func NewClient(ctx context.Context) (*client.Client, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	if _, err := cli.Ping(ctx); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("ping docker daemon: %w", err)
	}

	return cli, nil
}
