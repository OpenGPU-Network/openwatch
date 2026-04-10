package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/openwatch/openwatch/internal/metrics"
	"github.com/openwatch/openwatch/internal/updater"
)

// fakeTrigger is a minimal Trigger implementation used by the HTTP
// tests below. It records how many times the trigger methods were
// called and returns a configurable decision from TriggerByName so
// both the 202 and 404 branches of the handler can be exercised.
type fakeTrigger struct {
	state       *updater.StateStore
	allCalls    int32
	nameCalls   int32
	nameAccepts bool
}

func (f *fakeTrigger) TriggerAll(ctx context.Context) {
	atomic.AddInt32(&f.allCalls, 1)
}

func (f *fakeTrigger) TriggerByName(ctx context.Context, name string) bool {
	atomic.AddInt32(&f.nameCalls, 1)
	return f.nameAccepts
}

func (f *fakeTrigger) State() *updater.StateStore { return f.state }

func newTestServer(t *testing.T, trigger Trigger) *Server {
	t.Helper()
	m, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	return New(Config{
		Addr:    ":0",
		Trigger: trigger,
		Metrics: m,
		Version: "test-1.2.3",
		Log:     zerolog.Nop(),
	})
}

func TestHealth(t *testing.T) {
	s := newTestServer(t, &fakeTrigger{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", body["status"])
	}
	if body["version"] != "test-1.2.3" {
		t.Errorf("expected version=test-1.2.3, got %q", body["version"])
	}
}

func TestHealthRejectsPost(t *testing.T) {
	s := newTestServer(t, &fakeTrigger{})
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestContainersListsFromStateStore(t *testing.T) {
	store := updater.NewStateStore()
	store.MarkChecked("alpha", "img:a", updater.StatusUpToDate)
	store.MarkUpdated("bravo", "img:b")

	s := newTestServer(t, &fakeTrigger{state: store})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/containers", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Name        string  `json:"name"`
		Status      string  `json:"status"`
		LastChecked string  `json:"last_checked"`
		LastUpdated *string `json:"last_updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(got))
	}
	if got[0].Name != "alpha" {
		t.Errorf("expected alpha first (sorted), got %q", got[0].Name)
	}
	if got[0].LastUpdated != nil {
		t.Error("alpha has never been updated — last_updated should be null")
	}
	if got[1].LastUpdated == nil {
		t.Error("bravo has been updated — last_updated must not be null")
	}
	if _, err := time.Parse(time.RFC3339, got[0].LastChecked); err != nil {
		t.Errorf("last_checked not RFC3339: %v", err)
	}
}

func TestUpdateAllReturns202(t *testing.T) {
	f := &fakeTrigger{}
	s := newTestServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/update", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if atomic.LoadInt32(&f.allCalls) != 1 {
		t.Errorf("expected TriggerAll called once, got %d", f.allCalls)
	}
}

func TestUpdateByNameAccepted(t *testing.T) {
	f := &fakeTrigger{nameAccepts: true}
	s := newTestServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/update/myapp", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["accepted"] != true {
		t.Error("expected accepted=true")
	}
	if body["container"] != "myapp" {
		t.Errorf("expected container=myapp, got %v", body["container"])
	}
	if atomic.LoadInt32(&f.nameCalls) != 1 {
		t.Errorf("expected TriggerByName called once, got %d", f.nameCalls)
	}
}

func TestUpdateByNameNotFound(t *testing.T) {
	f := &fakeTrigger{nameAccepts: false}
	s := newTestServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/update/ghost", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestUpdateByNameRequiresName(t *testing.T) {
	f := &fakeTrigger{}
	s := newTestServer(t, f)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/update/", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	f := &fakeTrigger{}
	s := newTestServer(t, f)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if len(body) == 0 {
		t.Error("metrics response body is empty")
	}
}
