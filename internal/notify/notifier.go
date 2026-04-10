package notify

import (
	"strings"

	"github.com/rs/zerolog"
)

// Notifier sends update lifecycle events to an external destination.
// The interface is deliberately small: one method, three strings. The
// watcher only ever depends on this interface, never on any concrete
// implementation, so swapping shoutrrr for something else (or a fake
// in tests) is a matter of constructing a different Notifier.
//
// Notify is expected to be non-blocking from the caller's perspective.
// Concrete implementations that talk to the network MUST dispatch the
// actual send on a goroutine so a slow or dead notification provider
// cannot stall the update loop.
type Notifier interface {
	Notify(event, containerName, details string) error
}

// NoopNotifier discards every event. Returned by New when the
// configured notification URL is empty — i.e. the operator has not set
// up notifications at all. It is also used in tests as a zero-dependency
// stand-in.
type NoopNotifier struct{}

func (NoopNotifier) Notify(event, containerName, details string) error { return nil }

// New is the Notifier factory used by main.go. It wraps
// NewShoutrrrNotifier, attaches the daemon logger to a real shoutrrr
// notifier, and emits the "notifications disabled" debug line when
// the operator has not configured a URL. Keeping this policy in one
// place means the rest of the codebase can stay schema-free of any
// concrete notifier type.
//
// The debug line fires exactly once, at startup — never per event —
// so a quiet log does not turn into a flood when the operator
// intentionally disables notifications.
func New(url string, log zerolog.Logger) (Notifier, error) {
	if strings.TrimSpace(url) == "" {
		log.Debug().Msg("notifications disabled")
	}
	n, err := NewShoutrrrNotifier(url)
	if err != nil {
		return nil, err
	}
	if sn, ok := n.(*ShoutrrrNotifier); ok {
		sn.SetLogger(log)
	}
	return n, nil
}
