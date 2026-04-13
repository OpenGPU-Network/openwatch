package updater

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dockercli "github.com/docker/docker/client"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"

	"github.com/openwatch/openwatch/internal/config"
	"github.com/openwatch/openwatch/internal/docker"
	"github.com/openwatch/openwatch/internal/metrics"
	"github.com/openwatch/openwatch/internal/notify"
	"github.com/openwatch/openwatch/internal/registry"
)

// Notification event names. Defined as constants so the watcher and any
// future notifier backends agree on the exact strings.
const (
	EventUpdateStarted    = "UPDATE_STARTED"
	EventUpdateSuccess    = "UPDATE_SUCCESS"
	EventUpdateFailed     = "UPDATE_FAILED"
	EventUpdateAvailable  = "UPDATE_AVAILABLE"
	EventRollbackTrigger  = "ROLLBACK_TRIGGERED"
	EventRollbackSuccess  = "ROLLBACK_SUCCESS"
	EventRollbackFailed   = "ROLLBACK_FAILED"
)

// Default tick interval, used when cfg.Interval is zero or negative and
// no cron schedule is configured.
const defaultInterval = 24 * time.Hour

// Watcher runs the main poll loop. One instance per daemon.
//
// The state store and metrics collector are both optional at the type
// level (the HTTP API may be disabled, and tests may pass nil) but
// production always supplies real instances. Every method below that
// touches them guards on nil so zero-dependency unit tests can exercise
// the core update logic without standing up the whole Phase 4 stack.
type Watcher struct {
	cfg     *config.Config
	cli     *dockercli.Client
	log     zerolog.Logger
	notify  notify.Notifier
	state   *StateStore
	metrics *metrics.Metrics
}

// New constructs a Watcher. The notifier must be non-nil — callers
// should go through notify.New, which returns a NoopNotifier when
// notifications are intentionally disabled. This keeps watcher.go
// free of any concrete notify type references.
//
// state and mtx may be nil; pass nil when the HTTP API is disabled or
// in tests that don't care about observability. Production startup in
// main.go always supplies real instances.
func New(cfg *config.Config, cli *dockercli.Client, log zerolog.Logger, n notify.Notifier, state *StateStore, mtx *metrics.Metrics) *Watcher {
	return &Watcher{
		cfg:     cfg,
		cli:     cli,
		log:     log,
		notify:  n,
		state:   state,
		metrics: mtx,
	}
}

// State returns the watcher's container state store so the HTTP API
// can serve /api/v1/containers from the same snapshot the watcher
// writes to. Returns nil if the watcher was constructed without a
// store (tests).
func (w *Watcher) State() *StateStore { return w.state }

// Metrics returns the watcher's metrics collector so main.go can
// hand its HTTP handler to the API server. Returns nil if the
// watcher was constructed without metrics (tests).
func (w *Watcher) Metrics() *metrics.Metrics { return w.metrics }

// TriggerAll runs a full update tick in a new goroutine. Used by the
// HTTP API's POST /api/v1/update endpoint: the handler returns 202
// Accepted immediately and the tick runs to completion against the
// supplied context. The caller is responsible for passing a context
// with an appropriate lifetime — typically the daemon's main ctx so
// the tick is cancelled cleanly on shutdown.
func (w *Watcher) TriggerAll(ctx context.Context) {
	go w.tick(ctx)
}

// TriggerByName runs an update check for a single container, async.
// Returns false when the named container is not currently in the
// state store; the API handler turns that into a 404 without ever
// touching the Docker socket.
func (w *Watcher) TriggerByName(ctx context.Context, name string) bool {
	if w.state == nil || !w.state.Has(name) {
		return false
	}
	go func() {
		containers, err := docker.ListContainers(ctx, w.cli, w.log, w.cfg.IncludeStopped)
		if err != nil {
			w.log.Error().Err(err).Msg("api trigger: list containers failed")
			return
		}
		for _, c := range containers {
			if c.Name == name {
				w.processContainer(ctx, c)
				return
			}
		}
		w.log.Warn().Str("container", name).Msg("api trigger: container vanished before update")
	}()
	return true
}

// cronParser is shared across validation and scheduling so both phases
// use the same grammar. The Descriptor flag accepts shortcuts like
// "@hourly" in addition to standard 5-field expressions.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Run executes an initial pass and then either drives the poll loop from
// a cron schedule (when cfg.Schedule is set) or a fixed interval ticker.
// Both paths respect ctx cancellation for graceful shutdown.
//
// An invalid cron expression is validated up front — before the first
// tick — so main.go can fatal-exit with a clear message instead of
// silently falling back to the interval ticker, and we don't perform
// a pointless initial pass against misconfigured state.
func (w *Watcher) Run(ctx context.Context) error {
	schedule := strings.TrimSpace(w.cfg.Schedule)
	if schedule != "" {
		if _, err := cronParser.Parse(schedule); err != nil {
			return fmt.Errorf("invalid OPENWATCH_SCHEDULE %q: %w", schedule, err)
		}
	}

	w.tick(ctx)

	if schedule != "" {
		return w.runCron(ctx, schedule)
	}
	return w.runInterval(ctx)
}

// runInterval is the original Phase 1 behaviour: fire on a fixed wall
// clock interval. It stays here so deployments that prefer simplicity
// over cron expressions keep working unchanged.
func (w *Watcher) runInterval(ctx context.Context) error {
	interval := time.Duration(w.cfg.Interval) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}

	w.log.Info().Str("interval", fmt.Sprintf("%ds", int(interval.Seconds()))).Msg("watcher using interval scheduler")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info().Msg("watcher stopping")
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// runCron schedules ticks against a robfig cron expression. The caller
// has already validated the expression via cronParser, so AddFunc's
// parse error path is effectively dead code here but we still check it
// defensively. The cron runner manages its own goroutine; we just wait
// for ctx to be cancelled, then call Stop which blocks until any
// in-flight tick finishes.
func (w *Watcher) runCron(ctx context.Context, schedule string) error {
	c := cron.New(cron.WithParser(cronParser))
	if _, err := c.AddFunc(schedule, func() { w.tick(ctx) }); err != nil {
		return fmt.Errorf("register cron job: %w", err)
	}

	w.log.Info().Str("schedule", schedule).Msg("watcher using cron scheduler")
	c.Start()

	<-ctx.Done()
	w.log.Info().Msg("watcher stopping")
	stopCtx := c.Stop()
	<-stopCtx.Done()
	return nil
}

// tick performs a single pass over all containers. Any failure on an
// individual container is logged and skipped — the daemon never crashes
// on a per-container error.
//
// After processing, tick prunes state-store entries for containers
// that were not visible this pass so the HTTP API's container list
// stays accurate when containers are removed outside of OpenWatch.
func (w *Watcher) tick(ctx context.Context) {
	w.log.Info().Msg("checking containers")

	containers, err := docker.ListContainers(ctx, w.cli, w.log, w.cfg.IncludeStopped)
	if err != nil {
		w.log.Error().Err(err).Msg("list containers failed")
		return
	}

	w.metrics.SetContainersMonitored(len(containers))

	seen := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		seen[c.Name] = struct{}{}
		w.processContainer(ctx, c)
	}

	if w.state != nil {
		w.state.Prune(seen)
	}
}

func (w *Watcher) processContainer(ctx context.Context, c docker.Container) {
	clog := w.log.With().Str("container", c.Name).Str("image", c.Image).Logger()

	if docker.IsSelf(c) {
		clog.Debug().Msg("skipped self")
		return
	}

	policy := resolvePolicy(w.cfg, c)
	if policy.Skip {
		clog.Debug().Str("reason", policy.SkipReason).Msg("skipped by label policy")
		w.metrics.RecordUpdate(c.Name, metrics.StatusSkipped)
		w.state.MarkChecked(c.Name, c.Image, StatusUnknown)
		return
	}

	ref, err := registry.Parse(c.Image)
	if err != nil {
		clog.Error().Err(err).Msg("parse image ref failed")
		w.metrics.RecordUpdate(c.Name, metrics.StatusFailed)
		w.state.MarkChecked(c.Name, c.Image, StatusUnknown)
		return
	}

	auth, err := registry.LoadAuth(ctx, w.cfg.DockerConfig, ref.Registry, w.cfg.RegistryUser, w.cfg.RegistryPassword)
	if err != nil {
		// Never log auth details (even on failure) — err may come from JSON
		// parsing of config.json and contain nothing sensitive, but staying
		// disciplined here prevents a future refactor from leaking.
		clog.Warn().Err(err).Msg("load auth failed, falling back to anonymous")
		auth = nil
	}

	remoteIDs, err := registry.FetchRemoteIdentifiers(ref, auth)
	if err != nil {
		clog.Error().Err(err).Msg("fetch remote digest failed")
		w.metrics.RecordUpdate(c.Name, metrics.StatusFailed)
		w.state.MarkChecked(c.Name, c.Image, StatusUnknown)
		return
	}

	localIDs, err := docker.LocalImageIdentifiers(ctx, w.cli, c.Image)
	if err != nil {
		clog.Error().Err(err).Msg("read local digest failed")
		w.metrics.RecordUpdate(c.Name, metrics.StatusFailed)
		w.state.MarkChecked(c.Name, c.Image, StatusUnknown)
		return
	}

	if identifiersIntersect(localIDs, remoteIDs) {
		clog.Info().Str("digest", shortDigest(localIDs[0])).Msg("up to date")
		w.state.MarkChecked(c.Name, c.Image, StatusUpToDate)
		return
	}

	localDigest := localIDs[0]
	remoteDigest := remoteIDs[0]

	if policy.NotifyOnly {
		clog.Info().
			Str("local", shortDigest(localDigest)).
			Str("remote", shortDigest(remoteDigest)).
			Msg("update available (notify_only, not updating)")
		w.metrics.RecordUpdate(c.Name, metrics.StatusNotifyOnly)
		w.state.MarkChecked(c.Name, c.Image, StatusUpToDate)
		_ = w.notify.Notify(
			EventUpdateAvailable,
			c.Name,
			fmt.Sprintf("New image available for %s — update not applied (notify_only)", c.Image),
		)
		return
	}

	clog.Info().
		Str("local", shortDigest(localDigest)).
		Str("remote", shortDigest(remoteDigest)).
		Msg("update available, pulling")

	w.state.MarkUpdating(c.Name, c.Image)

	// Capture the image ID the container was running on BEFORE pulling the
	// new image. This is the rollback target: if the post-update
	// healthcheck fails we recreate the container against this SHA, and
	// we always skip cleanup of this image when rollback fires even if
	// cfg.Cleanup / openwatch.cleanup says otherwise. Capturing here
	// (rather than after the pull) makes the guarantee obvious to any
	// future reader tracing the rollback flow.
	previousImageID := c.ImageID

	_ = w.notify.Notify(
		EventUpdateStarted,
		c.Name,
		fmt.Sprintf("New digest detected for %s", c.Image),
	)

	// Time the pull → recreate window. The histogram measures the
	// operator-visible duration of an update and is only observed on
	// the happy path (healthcheck pass or no healthcheck) so failures
	// don't pollute the percentile distribution.
	updateStart := time.Now()

	if err := docker.PullImage(ctx, w.cli, c.Image, auth); err != nil {
		clog.Error().Err(err).Msg("pull failed")
		w.metrics.RecordUpdate(c.Name, metrics.StatusFailed)
		w.state.MarkChecked(c.Name, c.Image, StatusUnknown)
		_ = w.notify.Notify(EventUpdateFailed, c.Name, err.Error())
		return
	}

	newContainerID, err := docker.Recreate(ctx, w.cli, c, c.Image, policy.StopTimeoutSec)
	if err != nil {
		clog.Error().Err(err).Msg("recreate failed")
		w.metrics.RecordUpdate(c.Name, metrics.StatusFailed)
		w.state.MarkChecked(c.Name, c.Image, StatusUnknown)
		_ = w.notify.Notify(EventUpdateFailed, c.Name, err.Error())
		return
	}

	clog.Info().Str("new_container_id", newContainerID).Msg("container recreated")

	// Post-update healthcheck. If the container has no healthcheck we
	// treat the update as successful the moment it starts; this matches
	// Docker's own view that a container without HEALTHCHECK has no
	// health verdict to offer. A genuine failure (unhealthy / exited /
	// timed out) is routed through handleHealthFailure, which owns all
	// rollback-related notifications and metrics — processContainer
	// only records success on the happy path.
	healthErr := waitForHealthy(ctx, w.cli, newContainerID, w.cfg.HealthcheckTimeout)
	switch {
	case healthErr == nil:
		clog.Info().Str("new_container_id", newContainerID).Msg("update succeeded")
	case errors.Is(healthErr, errNoHealthcheck):
		clog.Debug().Str("new_container_id", newContainerID).Msg("no healthcheck defined, assuming healthy")
	default:
		w.handleHealthFailure(ctx, clog, policy, c, newContainerID, previousImageID, healthErr)
		return
	}

	w.metrics.ObserveUpdateDuration(c.Name, time.Since(updateStart).Seconds())
	w.metrics.RecordUpdate(c.Name, metrics.StatusSuccess)
	w.state.MarkUpdated(c.Name, c.Image)

	// Resolve the new image ID so the UPDATE_SUCCESS details can quote
	// both old and new SHAs. A failure here is not fatal — it only
	// means the notification loses a diagnostic field; the update
	// itself is already complete.
	var newImageID string
	if newIDs, idErr := docker.LocalImageIdentifiers(ctx, w.cli, c.Image); idErr != nil {
		clog.Warn().Err(idErr).Msg("could not resolve new image id for notification")
	} else if len(newIDs) > 0 {
		newImageID = newIDs[0]
	}
	_ = w.notify.Notify(
		EventUpdateSuccess,
		c.Name,
		fmt.Sprintf("Updated from %s to %s", shortImageID(previousImageID), shortImageID(newImageID)),
	)

	if policy.Cleanup && previousImageID != "" {
		if err := docker.RemoveImage(ctx, w.cli, previousImageID); err != nil {
			clog.Warn().Err(err).Str("image_id", previousImageID).Msg("cleanup old image failed")
		} else {
			clog.Debug().Str("image_id", previousImageID).Msg("cleaned up old image")
		}
	}
}

// identifiersIntersect reports whether the local and remote identifier sets
// share any sha256 value after trimming and case-folding. Empty values on
// either side never count as a match, so a registry that returned no digests
// cannot masquerade as "up to date".
func identifiersIntersect(local, remote []string) bool {
	if len(local) == 0 || len(remote) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(local))
	for _, id := range local {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	for _, id := range remote {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			return true
		}
	}
	return false
}

func shortDigest(d string) string {
	if i := strings.Index(d, ":"); i >= 0 {
		rest := d[i+1:]
		if len(rest) > 12 {
			return d[:i+1] + rest[:12]
		}
	}
	return d
}

// shortImageID returns the first 12 hex characters of an image ID,
// stripping the "sha256:" prefix if present. Used for notification
// details where readability matters more than precision — 12 hex chars
// are enough to disambiguate images in any sane deployment and match
// the short-form Docker itself prints in `docker images`.
func shortImageID(id string) string {
	if i := strings.Index(id, ":"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "unknown"
	}
	return id
}
