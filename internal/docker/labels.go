package docker

// Label keys OpenWatch reads off user containers. Centralised here so every
// lookup in the codebase flows through the same constants — no stray inline
// strings, no chance of typos drifting between files.
const (
	// LabelEnable opts a container in (or out, when the value is "false").
	LabelEnable = "openwatch.enable"

	// LabelSelf explicitly marks a container as the OpenWatch daemon so we
	// never try to update ourselves.
	LabelSelf = "openwatch.self"

	// LabelRollback overrides the daemon's rollback-on-failure setting for
	// this specific container.
	LabelRollback = "openwatch.rollback"

	// LabelCleanup overrides the daemon's cleanup setting for this specific
	// container (remove old image after successful update).
	LabelCleanup = "openwatch.cleanup"

	// LabelNotifyOnly signals that OpenWatch should notify about an update
	// but not actually apply it.
	LabelNotifyOnly = "openwatch.notify_only"

	// LabelStopTimeout overrides the daemon's stop timeout for this container,
	// expressed as an integer number of seconds.
	LabelStopTimeout = "openwatch.stop_timeout"

	// LabelDependsOn names a container that must be updated before this one.
	LabelDependsOn = "openwatch.depends_on"
)

// Boolean truthy/falsey label values. Keeping these as constants means
// comparisons in watcher.go and container.go stay in sync.
const (
	LabelValueTrue  = "true"
	LabelValueFalse = "false"
)
