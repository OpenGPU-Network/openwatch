package docker

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/rs/zerolog"
)

// Container bundles a container summary with its inspected state so callers
// can look at labels, image ID, and name without reissuing API calls.
type Container struct {
	ID      string
	Name    string
	Image   string
	ImageID string
	Labels  map[string]string
	Inspect container.InspectResponse
}

// ListContainers returns every container the daemon knows about. When
// includeStopped is false, stopped containers are filtered out.
//
// Individual container inspect failures are logged and skipped — a container
// can vanish between list and inspect (user ran `docker rm` mid-poll) and
// that should never kill the whole poll cycle.
func ListContainers(ctx context.Context, cli *client.Client, log zerolog.Logger, includeStopped bool) ([]Container, error) {
	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: includeStopped})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		inspect, err := cli.ContainerInspect(ctx, s.ID)
		if err != nil {
			log.Warn().Err(err).Str("container_id", s.ID).Msg("inspect failed, skipping")
			continue
		}

		name := strings.TrimPrefix(inspect.Name, "/")
		out = append(out, Container{
			ID:      inspect.ID,
			Name:    name,
			Image:   inspect.Config.Image,
			ImageID: inspect.Image,
			Labels:  inspect.Config.Labels,
			Inspect: inspect,
		})
	}
	return out, nil
}

// IsSelf returns true when the given container is the OpenWatch daemon itself.
// Detection runs through several heuristics so it still works whether the
// container is named "openwatch" or not, and whether we are running inside
// Docker or on the host.
func IsSelf(c Container) bool {
	if strings.EqualFold(c.Name, "openwatch") {
		return true
	}

	// When running inside a container, os.Hostname() is the short container ID.
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		if strings.HasPrefix(c.ID, hostname) {
			return true
		}
	}

	// Explicit opt-out label so users can rename the daemon and still skip it.
	if v, ok := c.Labels[LabelSelf]; ok && strings.EqualFold(v, LabelValueTrue) {
		return true
	}

	return false
}

// StopAndRemove stops a container gracefully and then removes it. Volumes are
// preserved (we need them when recreating) and networks are left alone for the
// same reason.
//
// Graceful stop flow:
//  1. ContainerStop with stopTimeoutSec — Docker daemon sends SIGTERM and
//     waits up to that many seconds, then falls back to SIGKILL internally.
//  2. Re-inspect the container. If the daemon somehow returned early (older
//     engines, or the client hit its own ctx deadline first) and the process
//     is still running, we explicitly issue SIGKILL via ContainerKill.
//
// This belt-and-braces approach matches the PRD spec (SIGTERM → wait →
// SIGKILL) regardless of which Docker Engine version we're talking to.
func StopAndRemove(ctx context.Context, cli *client.Client, id string, stopTimeoutSec int) error {
	timeout := stopTimeoutSec
	stopErr := cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})

	inspect, inspectErr := cli.ContainerInspect(ctx, id)
	if inspectErr == nil && inspect.State != nil && inspect.State.Running {
		if killErr := cli.ContainerKill(ctx, id, "KILL"); killErr != nil {
			return fmt.Errorf("kill container %s after graceful stop timeout: %w", id, killErr)
		}
	} else if stopErr != nil {
		return fmt.Errorf("stop container %s: %w", id, stopErr)
	}

	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: false, RemoveVolumes: false}); err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

// Recreate stops the existing container, removes it, and creates a new one
// with the exact same Config, HostConfig, and NetworkingConfig but pointed at
// newImageRef. The new container is started before returning.
//
// The caller is expected to have already pulled newImageRef.
func Recreate(ctx context.Context, cli *client.Client, c Container, newImageRef string, stopTimeoutSec int) (string, error) {
	inspect := c.Inspect
	if inspect.Config == nil || inspect.HostConfig == nil {
		return "", fmt.Errorf("container %s has incomplete inspect data", c.ID)
	}

	// Copy Config and point it at the new image. Everything else — env, cmd,
	// entrypoint, workdir, user, labels, exposed ports, stop signal — is
	// preserved verbatim.
	newConfig := *inspect.Config
	newConfig.Image = newImageRef

	// Deep-copy HostConfig through assignment. Pointer-to-HostConfig fields
	// are shared, which is what we want: binds, port bindings, restart policy,
	// capabilities, resources, log config, extra hosts, etc. all carry over.
	newHostConfig := *inspect.HostConfig

	// NetworkMode / IpcMode / PidMode / UTSMode / UsernsMode can all reference
	// the now-dead original container via the "container:<id>" syntax. Clear
	// any of those so the new container gets its own namespace stack.
	if strings.HasPrefix(string(newHostConfig.NetworkMode), "container:") {
		newHostConfig.NetworkMode = ""
	}
	if strings.HasPrefix(string(newHostConfig.IpcMode), "container:") {
		newHostConfig.IpcMode = ""
	}
	if strings.HasPrefix(string(newHostConfig.PidMode), "container:") {
		newHostConfig.PidMode = ""
	}
	if strings.HasPrefix(string(newHostConfig.UTSMode), "container:") {
		newHostConfig.UTSMode = ""
	}
	if strings.HasPrefix(string(newHostConfig.UsernsMode), "container:") {
		newHostConfig.UsernsMode = ""
	}

	// Networks: copy each EndpointSettings and strip fields the daemon will
	// reject on create (runtime-assigned IPs, sandbox IDs, MAC addresses
	// picked by the driver, etc.). We only keep the knobs the user actually
	// configured.
	var extraNetworks map[string]*network.EndpointSettings
	var firstNetworkName string
	var firstNetworkSettings *network.EndpointSettings
	if inspect.NetworkSettings != nil && len(inspect.NetworkSettings.Networks) > 0 {
		for name, ep := range inspect.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			cleaned := &network.EndpointSettings{
				Aliases:    ep.Aliases,
				Links:      ep.Links,
				NetworkID:  ep.NetworkID,
				IPAMConfig: ep.IPAMConfig,
				DriverOpts: ep.DriverOpts,
			}
			if firstNetworkSettings == nil {
				firstNetworkName = name
				firstNetworkSettings = cleaned
				continue
			}
			if extraNetworks == nil {
				extraNetworks = map[string]*network.EndpointSettings{}
			}
			extraNetworks[name] = cleaned
		}
	}

	// Older Docker Engine versions (pre-1.44 API) reject a NetworkingConfig
	// with more than one EndpointsConfig entry on create. Supplying only the
	// first network at create time and attaching the rest via NetworkConnect
	// afterward is compatible with every engine we target.
	var newNetConfig *network.NetworkingConfig
	if firstNetworkSettings != nil {
		newNetConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				firstNetworkName: firstNetworkSettings,
			},
		}
	}

	name := c.Name

	if err := StopAndRemove(ctx, cli, c.ID, stopTimeoutSec); err != nil {
		return "", err
	}

	created, err := cli.ContainerCreate(ctx, &newConfig, &newHostConfig, newNetConfig, nil, name)
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", name, err)
	}

	// Attach remaining networks before starting so the container comes up with
	// full connectivity. If any connect fails we return the error with the new
	// container ID so the caller can log, trigger rollback, etc.
	for netName, ep := range extraNetworks {
		if err := cli.NetworkConnect(ctx, netName, created.ID, ep); err != nil {
			return created.ID, fmt.Errorf("connect container %s to network %s: %w", name, netName, err)
		}
	}

	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return created.ID, fmt.Errorf("start container %s: %w", name, err)
	}

	return created.ID, nil
}
