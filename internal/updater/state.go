package updater

import (
	"sync"
	"time"
)

// ContainerStatus is the coarse-grained lifecycle state of a
// monitored container, as reported via the HTTP API's
// /api/v1/containers endpoint. It is deliberately small — the API
// surface committed to is a single opaque string, so a consumer can
// render it verbatim without parsing.
type ContainerStatus string

const (
	StatusUpToDate ContainerStatus = "up_to_date"
	StatusUpdating ContainerStatus = "updating"
	StatusUnknown  ContainerStatus = "unknown"
)

// ContainerState is the snapshot the API returns for a single
// container. It is intentionally a value type: the store hands out
// copies (never pointers to its internal map) so concurrent readers
// cannot race with concurrent writers.
type ContainerState struct {
	Name        string          `json:"name"`
	Image       string          `json:"image"`
	Status      ContainerStatus `json:"status"`
	LastChecked time.Time       `json:"last_checked"`
	// LastUpdated is zero when the container has never been updated
	// through OpenWatch in this process's lifetime. The JSON encoder
	// converts a zero time.Time to "0001-01-01T00:00:00Z" — the API
	// layer rewrites that to JSON null before sending to clients.
	LastUpdated time.Time `json:"last_updated"`
}

// StateStore keeps a thread-safe snapshot of every container the
// watcher has inspected in the current process. Writers (the watcher)
// hold the write lock for the duration of each mutation; readers
// (the HTTP API) hold the read lock for the duration of a snapshot
// copy. This is cheaper and more forgiving than sync.Map for our
// access pattern: many small writes, occasional full-list reads.
type StateStore struct {
	mu    sync.RWMutex
	items map[string]ContainerState
}

// NewStateStore constructs an empty store. The watcher creates one
// per daemon; the HTTP API holds a reference to the same instance.
func NewStateStore() *StateStore {
	return &StateStore{items: map[string]ContainerState{}}
}

// All mutation methods below are nil-safe on the receiver so tests
// (and any future code path that disables the API) can pass a nil
// StateStore through to the watcher without special-casing every
// call site.

// MarkChecked records that a container was inspected during a tick.
// Status is usually StatusUpToDate or StatusUnknown; callers use
// MarkUpdating / MarkUpdated for lifecycle transitions during an
// active update.
func (s *StateStore) MarkChecked(name, image string, status ContainerStatus) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.items[name]
	entry.Name = name
	entry.Image = image
	entry.Status = status
	entry.LastChecked = time.Now().UTC()
	s.items[name] = entry
}

// MarkUpdating flips the status to "updating" and refreshes
// last_checked. Called the moment processContainer decides a pull
// is needed so an API consumer sees the state change live.
func (s *StateStore) MarkUpdating(name, image string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.items[name]
	entry.Name = name
	entry.Image = image
	entry.Status = StatusUpdating
	entry.LastChecked = time.Now().UTC()
	s.items[name] = entry
}

// MarkUpdated records a successful update: the container is now
// back in the "up_to_date" state and last_updated is stamped with
// the current wall clock. Called after the post-update healthcheck
// passes (or is absent).
func (s *StateStore) MarkUpdated(name, image string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	entry := s.items[name]
	entry.Name = name
	entry.Image = image
	entry.Status = StatusUpToDate
	entry.LastChecked = now
	entry.LastUpdated = now
	s.items[name] = entry
}

// Snapshot returns a copy of every tracked container state, sorted
// by name for deterministic API output. The returned slice is
// detached from the store — callers can mutate it freely without
// racing the watcher.
func (s *StateStore) Snapshot() []ContainerState {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ContainerState, 0, len(s.items))
	for _, v := range s.items {
		out = append(out, v)
	}
	sortContainerStates(out)
	return out
}

// Get returns a single container's state, or false if unknown.
// Used by the HTTP API's POST /api/v1/update/:name handler to
// validate the target container before triggering an update.
func (s *StateStore) Get(name string) (ContainerState, bool) {
	if s == nil {
		return ContainerState{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[name]
	return v, ok
}

// Has reports whether a container with the given name is currently
// tracked. Marginally cheaper than Get when the caller only needs a
// boolean.
func (s *StateStore) Has(name string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.items[name]
	return ok
}

// Prune removes any entries not listed in keep. Called at the end of
// each tick so containers removed outside of OpenWatch stop showing
// up in the API. The input is a set of names that were visible in
// the latest ListContainers call.
func (s *StateStore) Prune(keep map[string]struct{}) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.items {
		if _, ok := keep[name]; !ok {
			delete(s.items, name)
		}
	}
}

// sortContainerStates sorts in place by name. Pulled out into a
// small helper so Snapshot stays readable.
func sortContainerStates(s []ContainerState) {
	// Insertion sort — we rarely have more than a few dozen
	// containers in a single host and the alternative (importing
	// sort) adds nothing material here.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Name > s[j].Name; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
