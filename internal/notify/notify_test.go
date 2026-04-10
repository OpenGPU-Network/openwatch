package notify

import (
	"testing"

	"github.com/rs/zerolog"
)

// TestNewEmptyURLReturnsNoop verifies the factory path that keeps
// the rest of the daemon shoutrrr-free when notifications are
// intentionally disabled. An empty URL must yield a NoopNotifier
// without error.
func TestNewEmptyURLReturnsNoop(t *testing.T) {
	n, err := New("", zerolog.Nop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := n.(NoopNotifier); !ok {
		t.Fatalf("expected NoopNotifier, got %T", n)
	}
}

// TestNewShoutrrrNotifierEmpty mirrors the factory path at the
// level below. The Phase 3 review clarified that NewShoutrrrNotifier
// itself should degrade to NoopNotifier on an empty URL rather than
// erroring — anything else is a caller bug worth regression-guarding.
func TestNewShoutrrrNotifierEmpty(t *testing.T) {
	n, err := NewShoutrrrNotifier("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := n.(NoopNotifier); !ok {
		t.Fatalf("expected NoopNotifier, got %T", n)
	}
}

// TestNewShoutrrrNotifierInvalid asserts that a non-empty URL that
// shoutrrr can't parse returns a sanitized error. The error message
// must not echo the raw URL — we use a URL with a fake credential
// in it and check the error string does not contain it.
func TestNewShoutrrrNotifierInvalid(t *testing.T) {
	badURL := "fakescheme://SECRETTOKEN@host/path"
	_, err := NewShoutrrrNotifier(badURL)
	if err == nil {
		t.Fatal("expected error for unknown shoutrrr scheme, got nil")
	}
	if containsSensitive(err.Error(), "SECRETTOKEN") {
		t.Errorf("error message leaked token content: %q", err.Error())
	}
}

// TestNoopNotifierSilent confirms NoopNotifier.Notify is a no-op
// that returns nil regardless of input.
func TestNoopNotifierSilent(t *testing.T) {
	var n NoopNotifier
	if err := n.Notify("UPDATE_STARTED", "x", "y"); err != nil {
		t.Errorf("NoopNotifier.Notify returned error: %v", err)
	}
}

// containsSensitive is a tiny case-sensitive substring check,
// isolated here so future tests can share the helper without
// depending on strings.Contains directly in assertion bodies.
func containsSensitive(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
