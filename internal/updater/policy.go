package updater

import (
	"strconv"
	"strings"

	"github.com/openwatch/openwatch/internal/config"
	"github.com/openwatch/openwatch/internal/docker"
)

// Policy is the fully-resolved, per-container decision set the watcher
// acts on. Every knob here folds together the global config and any
// container labels into one concrete value so processContainer never has
// to look at raw labels directly.
type Policy struct {
	// Skip is true when the container should be ignored entirely this pass
	// (either because of label_enable policy or an explicit disable label).
	Skip bool
	// SkipReason is a short human-readable explanation used for logging.
	SkipReason string

	// NotifyOnly means: detect updates and log/notify, but never pull or
	// recreate. Used for staging containers where the operator wants
	// visibility without automation.
	NotifyOnly bool

	// Rollback indicates whether this container should be rolled back to
	// its previous image if the post-update healthcheck fails.
	Rollback bool

	// Cleanup indicates whether the old image should be removed after a
	// successful update.
	Cleanup bool

	// StopTimeoutSec is the number of seconds to wait for a graceful stop
	// before the daemon escalates to SIGKILL.
	StopTimeoutSec int
}

// resolvePolicy is the single source of truth for turning
// (config + container labels) into a concrete Policy. Every call site in
// the updater package goes through this helper so label semantics can
// never drift between watcher.go and rollback.go.
func resolvePolicy(cfg *config.Config, c docker.Container) Policy {
	p := Policy{
		Rollback:       cfg.RollbackOnFailure,
		Cleanup:        cfg.Cleanup,
		StopTimeoutSec: cfg.StopTimeout,
	}
	if p.StopTimeoutSec <= 0 {
		p.StopTimeoutSec = config.DefaultStopTimeout
	}

	enableVal, enableSet := c.Labels[docker.LabelEnable]
	enableTrue := enableSet && strings.EqualFold(enableVal, docker.LabelValueTrue)
	enableFalse := enableSet && strings.EqualFold(enableVal, docker.LabelValueFalse)

	// Enable/disable resolution. An explicit openwatch.enable=true always
	// wins (even over label_enable=true, per the spec). An explicit
	// openwatch.enable=false always skips. Otherwise we honour the global
	// label_enable opt-in switch.
	switch {
	case enableFalse:
		p.Skip = true
		p.SkipReason = "openwatch.enable=false"
	case enableTrue:
		// Explicitly enabled — fall through, never skipped.
	case cfg.LabelEnable:
		p.Skip = true
		p.SkipReason = "label_enable=true and container missing openwatch.enable=true"
	}

	if v, ok := c.Labels[docker.LabelNotifyOnly]; ok && strings.EqualFold(v, docker.LabelValueTrue) {
		p.NotifyOnly = true
	}

	if v, ok := c.Labels[docker.LabelRollback]; ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			p.Rollback = b
		}
	}

	if v, ok := c.Labels[docker.LabelCleanup]; ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			p.Cleanup = b
		}
	}

	if v, ok := c.Labels[docker.LabelStopTimeout]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			p.StopTimeoutSec = n
		}
	}

	return p
}
