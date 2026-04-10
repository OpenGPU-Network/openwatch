package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewRegisters sanity-checks that New returns a usable Metrics
// bundle whose Handler serves the Prometheus exposition text and
// whose counters/gauge/histogram can be written without panicking.
// Registration failures in New are the only error path that matters;
// this test exercises it by calling New twice and ensuring the second
// call does NOT blow up (each call owns its own registry, so there
// is no collision).
func TestNewRegisters(t *testing.T) {
	m1, err := New()
	if err != nil {
		t.Fatalf("first New() returned error: %v", err)
	}
	if m1 == nil {
		t.Fatal("first New() returned nil")
	}

	m2, err := New()
	if err != nil {
		t.Fatalf("second New() returned error: %v", err)
	}
	if m2 == nil {
		t.Fatal("second New() returned nil")
	}
	if m1.registry == m2.registry {
		t.Fatal("expected distinct registries across New() calls")
	}
}

// TestRecordersNilSafe confirms the nil-receiver pattern: every
// recorder method must be callable on a nil *Metrics without panicking,
// so test code and future optional-metrics paths don't have to guard
// on nil at every call site.
func TestRecordersNilSafe(t *testing.T) {
	var m *Metrics // nil

	// All four must run without panicking.
	m.RecordUpdate("c", StatusSuccess)
	m.RecordRollback("c", RollbackSuccess)
	m.SetContainersMonitored(3)
	m.ObserveUpdateDuration("c", 1.5)
}

// TestHandlerServesMetrics asserts that after we increment a known
// counter the /metrics endpoint serves exposition text containing
// that metric name. This covers the Handler() + registry wiring.
func TestHandlerServesMetrics(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	m.RecordUpdate("myapp", StatusSuccess)
	m.RecordRollback("myapp", RollbackSuccess)
	m.SetContainersMonitored(7)
	m.ObserveUpdateDuration("myapp", 2.3)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"openwatch_updates_total",
		"openwatch_rollbacks_total",
		"openwatch_containers_monitored",
		"openwatch_update_duration_seconds",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}
