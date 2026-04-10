package updater

import (
	"sync"
	"testing"
)

// TestStateStore_Lifecycle runs a full container through its status
// transitions and asserts that each marker updates the snapshot as
// expected. This is the API the HTTP layer reads, so an off-by-one
// here would show up as stale data in /api/v1/containers.
func TestStateStore_Lifecycle(t *testing.T) {
	s := NewStateStore()

	// Initially empty.
	if got := len(s.Snapshot()); got != 0 {
		t.Fatalf("expected empty snapshot, got %d entries", got)
	}

	s.MarkChecked("app", "myrepo/app:latest", StatusUpToDate)
	if !s.Has("app") {
		t.Fatal("Has returned false after MarkChecked")
	}

	got, ok := s.Get("app")
	if !ok {
		t.Fatal("Get returned !ok after MarkChecked")
	}
	if got.Status != StatusUpToDate {
		t.Errorf("expected status up_to_date, got %q", got.Status)
	}
	if got.LastChecked.IsZero() {
		t.Error("expected LastChecked to be set")
	}
	if !got.LastUpdated.IsZero() {
		t.Error("expected LastUpdated zero before first update")
	}

	s.MarkUpdating("app", "myrepo/app:latest")
	got, _ = s.Get("app")
	if got.Status != StatusUpdating {
		t.Errorf("expected status updating, got %q", got.Status)
	}

	s.MarkUpdated("app", "myrepo/app:latest")
	got, _ = s.Get("app")
	if got.Status != StatusUpToDate {
		t.Errorf("expected status up_to_date post update, got %q", got.Status)
	}
	if got.LastUpdated.IsZero() {
		t.Error("expected LastUpdated to be set after MarkUpdated")
	}
}

// TestStateStore_Prune confirms Prune removes containers not in the
// keep set, matching the tick() end-of-pass cleanup.
func TestStateStore_Prune(t *testing.T) {
	s := NewStateStore()
	s.MarkChecked("a", "img:a", StatusUpToDate)
	s.MarkChecked("b", "img:b", StatusUpToDate)
	s.MarkChecked("c", "img:c", StatusUpToDate)

	s.Prune(map[string]struct{}{"a": {}, "c": {}})

	if s.Has("b") {
		t.Error("b should have been pruned")
	}
	if !s.Has("a") || !s.Has("c") {
		t.Error("a and c should still be present")
	}
}

// TestStateStore_SnapshotSorted verifies the snapshot is sorted by
// name so /api/v1/containers has deterministic output regardless of
// map iteration order.
func TestStateStore_SnapshotSorted(t *testing.T) {
	s := NewStateStore()
	s.MarkChecked("charlie", "img:c", StatusUpToDate)
	s.MarkChecked("alpha", "img:a", StatusUpToDate)
	s.MarkChecked("bravo", "img:b", StatusUpToDate)

	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(snap))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if snap[i].Name != w {
			t.Errorf("position %d: expected %q, got %q", i, w, snap[i].Name)
		}
	}
}

// TestStateStore_NilSafe hits every nil-safe method. The store may
// be nil in tests and in any future path that disables the API, so
// the watcher must never panic on a nil receiver.
func TestStateStore_NilSafe(t *testing.T) {
	var s *StateStore

	s.MarkChecked("x", "y", StatusUpToDate) // must not panic
	s.MarkUpdating("x", "y")                // must not panic
	s.MarkUpdated("x", "y")                 // must not panic
	s.Prune(nil)                            // must not panic

	if s.Has("x") {
		t.Error("nil store reported Has=true")
	}
	if _, ok := s.Get("x"); ok {
		t.Error("nil store reported Get ok=true")
	}
	if snap := s.Snapshot(); snap != nil {
		t.Errorf("nil store returned non-nil snapshot: %v", snap)
	}
}

// TestStateStore_Concurrent exercises the RWMutex with many writers
// and readers racing each other. Intended to run under -race — the
// test is the assertion.
func TestStateStore_Concurrent(t *testing.T) {
	s := NewStateStore()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.MarkChecked("c", "img", StatusUpToDate)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()
}
