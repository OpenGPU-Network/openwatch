package updater

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	dockercli "github.com/docker/docker/client"
	"github.com/rs/zerolog"

	"github.com/openwatch/openwatch/internal/config"
	"github.com/openwatch/openwatch/internal/docker"
	"github.com/openwatch/openwatch/internal/metrics"
)

// healthPollInterval is the period between container inspect calls while
// we wait for a post-update healthcheck verdict. Two seconds strikes a
// reasonable balance: fast enough to react within a human-noticeable
// window, slow enough that a dozen updating containers don't hammer the
// Docker socket.
const healthPollInterval = 2 * time.Second

// errNoHealthcheck is a sentinel used internally to distinguish "the
// container did not define a healthcheck" from actual failures. Callers
// downgrade it to a debug log.
var errNoHealthcheck = errors.New("container has no healthcheck configured")

// waitForHealthy blocks until the given container reports "healthy",
// reports "unhealthy", stops running, or the timeout elapses. A
// container without a healthcheck defined returns errNoHealthcheck
// immediately so the caller can fall through without treating the
// absence of a check as a failure.
//
// timeoutSec is the total budget in seconds, sourced from
// cfg.HealthcheckTimeout by the caller. A non-positive value degrades
// to config.DefaultHealthcheckTimeout so we never divide by zero or
// hang forever on a malformed config; there are no magic numbers in
// this function.
func waitForHealthy(ctx context.Context, cli *dockercli.Client, containerID string, timeoutSec int) error {
	initial, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect container %s for healthcheck: %w", containerID, err)
	}
	if initial.Config == nil || initial.Config.Healthcheck == nil || len(initial.Config.Healthcheck.Test) == 0 {
		return errNoHealthcheck
	}
	// A Test of {"NONE"} explicitly disables the inherited healthcheck.
	if len(initial.Config.Healthcheck.Test) == 1 && initial.Config.Healthcheck.Test[0] == "NONE" {
		return errNoHealthcheck
	}

	if timeoutSec <= 0 {
		timeoutSec = config.DefaultHealthcheckTimeout
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

	for {
		inspect, err := cli.ContainerInspect(ctx, containerID)
		if err != nil {
			return fmt.Errorf("inspect container %s: %w", containerID, err)
		}

		if inspect.State != nil {
			if !inspect.State.Running && !inspect.State.Restarting {
				return fmt.Errorf("container %s exited before becoming healthy (status=%s, exit_code=%d)",
					containerID, inspect.State.Status, inspect.State.ExitCode)
			}
			if inspect.State.Health != nil {
				switch inspect.State.Health.Status {
				case container.Healthy:
					return nil
				case container.Unhealthy:
					return fmt.Errorf("container %s reported unhealthy (failing streak=%d)",
						containerID, inspect.State.Health.FailingStreak)
				}
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not become healthy within %d seconds", containerID, timeoutSec)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthPollInterval):
		}
	}
}

// handleHealthFailure is the rollback decision point: given a freshly
// updated container whose healthcheck failed, it consults the
// per-container Policy, fires the right notification events, and
// delegates the actual recreate to performRollback.
//
// It lives in rollback.go (alongside performRollback) rather than
// watcher.go so that the entire rollback subsystem — decision, recreate,
// notification sequencing — is self-contained in one file. A crash in
// performRollback surfaces as a returned error, never a panic; the
// daemon logs and continues processing subsequent containers.
//
// Notification scope: per the Phase 3 spec, UPDATE_FAILED covers only
// pull/recreate errors, so this function never emits it. A healthcheck
// failure that triggers rollback emits ROLLBACK_TRIGGERED followed by
// ROLLBACK_SUCCESS or ROLLBACK_FAILED. A healthcheck failure with
// rollback disabled emits nothing — the warning log is the operator's
// only signal, which matches the spec's "no more, no less" directive.
func (w *Watcher) handleHealthFailure(
	ctx context.Context,
	clog zerolog.Logger,
	policy Policy,
	prev docker.Container,
	newContainerID string,
	previousImageID string,
	healthErr error,
) {
	clog.Error().
		Err(healthErr).
		Str("container", prev.Name).
		Str("image", prev.Image).
		Str("new_container_id", newContainerID).
		Msg("post-update healthcheck failed")

	w.metrics.RecordUpdate(prev.Name, metrics.StatusFailed)

	if !policy.Rollback {
		clog.Warn().
			Str("container", prev.Name).
			Str("new_container_id", newContainerID).
			Msg("rollback disabled — leaving unhealthy container in place for inspection")
		w.state.MarkChecked(prev.Name, prev.Image, StatusUnknown)
		return
	}
	if previousImageID == "" {
		clog.Error().
			Str("container", prev.Name).
			Msg("rollback requested but no previous image ID captured — cannot roll back")
		w.metrics.RecordRollback(prev.Name, metrics.RollbackFailed)
		w.state.MarkChecked(prev.Name, prev.Image, StatusUnknown)
		_ = w.notify.Notify(EventRollbackFailed, prev.Name, "no previous image id captured, cannot roll back")
		return
	}

	_ = w.notify.Notify(
		EventRollbackTrigger,
		prev.Name,
		fmt.Sprintf("Healthcheck failed after update, reverting to %s", shortImageID(previousImageID)),
	)

	if err := performRollback(ctx, w.cli, clog, prev, previousImageID, newContainerID, policy.StopTimeoutSec); err != nil {
		clog.Error().
			Err(err).
			Str("container", prev.Name).
			Str("previous_image_id", previousImageID).
			Msg("rollback failed")
		w.metrics.RecordRollback(prev.Name, metrics.RollbackFailed)
		w.state.MarkChecked(prev.Name, prev.Image, StatusUnknown)
		_ = w.notify.Notify(EventRollbackFailed, prev.Name, err.Error())
		return
	}

	clog.Info().
		Str("container", prev.Name).
		Str("image_id", previousImageID).
		Msg("rollback succeeded")
	w.metrics.RecordRollback(prev.Name, metrics.RollbackSuccess)
	w.state.MarkChecked(prev.Name, prev.Image, StatusUpToDate)
	_ = w.notify.Notify(EventRollbackSuccess, prev.Name, "Reverted successfully")
	// Intentionally no cleanup of previousImageID here — the container is
	// running on it again and deleting it would brick the rollback.
}

// performRollback recreates a just-updated container using the previous
// image ID. The caller must have already decided rollback is warranted
// (post-update healthcheck failed and policy allows rollback). The new
// unhealthy container ID is `newContainerID`; `previous` is the
// pre-update snapshot captured before the pull, so we can feed the same
// Config/HostConfig/Networking back in to Recreate.
//
// On success the container is running under previousImageID. On failure
// the caller gets a wrapped error — the daemon keeps running, the
// container is left in whatever state Docker ended up in, and a human
// needs to investigate. We deliberately do not delete previousImageID
// during rollback; cleanup of the new image is also the caller's
// responsibility and must be skipped when a rollback happened.
func performRollback(
	ctx context.Context,
	cli *dockercli.Client,
	log zerolog.Logger,
	previous docker.Container,
	previousImageID string,
	newContainerID string,
	stopTimeoutSec int,
) error {
	if previousImageID == "" {
		return fmt.Errorf("rollback requested but previous image ID is empty")
	}

	log.Warn().
		Str("container", previous.Name).
		Str("unhealthy_container_id", newContainerID).
		Str("previous_image_id", previousImageID).
		Msg("rolling back to previous image")

	// The new container is the one we just recreated — it shares the same
	// name as the previous container, so Recreate can't be reused directly:
	// we first need to remove the new unhealthy container by its ID, then
	// create a fresh container from the captured pre-update config.
	//
	// docker.Recreate handles stop + remove + create + start, and it takes
	// a Container value whose Inspect field carries the config to restore.
	// The original previous.Container value (captured before pull) is
	// exactly that, but its ID now points at a dead container — we swap in
	// the newContainerID so StopAndRemove targets the live one.
	target := previous
	target.ID = newContainerID
	target.ImageID = ""

	rolledBackID, err := docker.Recreate(ctx, cli, target, previousImageID, stopTimeoutSec)
	if err != nil {
		return fmt.Errorf("recreate container %s with previous image: %w", previous.Name, err)
	}

	log.Info().
		Str("container", previous.Name).
		Str("rolled_back_container_id", rolledBackID).
		Str("image_id", previousImageID).
		Msg("rollback complete")

	return nil
}
