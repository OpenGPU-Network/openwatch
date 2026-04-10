package notify

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/router"
	"github.com/rs/zerolog"
)

// notifyTimeout caps how long a single Notify call may spend waiting
// on shoutrrr before the watcher loop gets control back. A dead or
// wedged notification provider is bounded to this value per event, so
// the worst case for a tick with N containers emitting events is
// N * notifyTimeout — orders of magnitude less than an unbounded block
// would allow. Ten seconds matches shoutrrr's own router default.
const notifyTimeout = 10 * time.Second

// ShoutrrrNotifier sends event notifications through the shoutrrr
// library. It wraps a single *router.ServiceRouter and caps every
// Notify call at notifyTimeout so a slow or dead notification provider
// can never stall the update loop indefinitely.
//
// This is the only file in the repository that imports shoutrrr.
// Everything else talks through the Notifier interface.
type ShoutrrrNotifier struct {
	sender *router.ServiceRouter

	logMu sync.RWMutex
	log   zerolog.Logger
}

// NewShoutrrrNotifier builds a Notifier from a shoutrrr URL. Its three
// cases cover every runtime configuration:
//
//   - empty URL → NoopNotifier{}, nil  (notifications intentionally off)
//   - invalid URL → nil, error        (configuration bug, fatal at startup)
//   - valid URL → *ShoutrrrNotifier, nil
//
// The error branch NEVER wraps the underlying shoutrrr error — shoutrrr
// parse errors can echo the raw URL (scheme, host, query parameters
// that carry tokens), so the returned message is a sanitized constant.
// Any caller that logs the error is guaranteed not to leak secrets.
//
// On the happy path the notifier starts with a no-op zerolog logger;
// call SetLogger to wire in the daemon logger if you want
// send-failure diagnostics. The package-level New factory in
// notifier.go does this automatically.
func NewShoutrrrNotifier(url string) (Notifier, error) {
	if strings.TrimSpace(url) == "" {
		return NoopNotifier{}, nil
	}
	sender, err := shoutrrr.CreateSender(url)
	if err != nil {
		return nil, errors.New("invalid notification URL: shoutrrr could not parse it (check scheme, host, and credentials)")
	}
	return &ShoutrrrNotifier{
		sender: sender,
		log:    zerolog.Nop(),
	}, nil
}

// SetLogger swaps the notifier's internal logger. Safe to call after
// construction; subsequent send failures log against the new logger.
// Only non-sensitive fields (event name, container name) are ever
// logged — URLs, tokens, and the detailed shoutrrr error text stay
// out of the log by design.
func (n *ShoutrrrNotifier) SetLogger(log zerolog.Logger) {
	n.logMu.Lock()
	defer n.logMu.Unlock()
	n.log = log
}

// Notify formats the event payload, hands it to shoutrrr on a
// short-lived goroutine, and waits up to notifyTimeout for the send
// to complete. The timeout is mandatory: we must be able to return a
// send-failure error to the caller (the watcher uses the return value
// to decide if a notification failed in tests / future observers) but
// we must also never stall the update loop on a wedged provider.
//
// The returned error is always generic. The underlying shoutrrr error
// is deliberately NOT wrapped — it may contain URL fragments or
// credentials — so callers can log the returned error at any level
// without risking a leak.
func (n *ShoutrrrNotifier) Notify(event, containerName, details string) error {
	msg := fmt.Sprintf("[OpenWatch] %s — %s\n%s", event, containerName, details)

	// Send on a goroutine so we can race it against a hard timeout.
	// The channel is buffered with the exact slot we need so the
	// goroutine never blocks on send even if we time out first.
	done := make(chan []error, 1)
	go func() {
		done <- n.sender.Send(msg, nil)
	}()

	select {
	case errs := <-done:
		failures := 0
		for _, e := range errs {
			if e != nil {
				failures++
			}
		}
		if failures == 0 {
			return nil
		}
		n.logFailure(event, containerName, "shoutrrr send returned errors", failures, len(errs))
		return errors.New("notification send failed")

	case <-time.After(notifyTimeout):
		n.logFailure(event, containerName, "shoutrrr send timed out", 0, 0)
		return errors.New("notification send timed out")
	}
}

// logFailure centralises the sanitized error-log pattern. Every field
// here is vetted as non-sensitive: event name and container name come
// from the caller (watcher-controlled constants and container labels);
// failed_services / total_services are integers; cause is a short
// fixed string picked from a whitelist. The raw shoutrrr error text,
// the notification URL, and any HTTP response bodies never reach
// this code path.
func (n *ShoutrrrNotifier) logFailure(event, containerName, cause string, failed, total int) {
	n.logMu.RLock()
	log := n.log
	n.logMu.RUnlock()

	evt := log.Error().
		Str("event", event).
		Str("container", containerName).
		Str("cause", cause)
	if total > 0 {
		evt = evt.Int("failed_services", failed).Int("total_services", total)
	}
	evt.Msg("notification send failed")
}
