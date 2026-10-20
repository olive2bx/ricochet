package rules

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Store holds the active rule set loaded from a file and allows it to be
// swapped atomically. Lookups are lock-free.
type Store struct {
	path string
	set  atomic.Pointer[Set]

	mu       sync.Mutex
	loadedAt time.Time
	lastErr  error
}

// Status describes the state of a Store.
type Status struct {
	Path     string
	Rules    []Rule
	LoadedAt time.Time // zero if never loaded successfully
	LastErr  error     // error of the last reload attempt, nil if it succeeded
}

// NewStore loads path. An empty path yields an empty store with no rules.
// The initial load must succeed.
func NewStore(path string) (*Store, error) {
	st := &Store{path: path}
	st.set.Store(&Set{})
	if path == "" {
		return st, nil
	}
	if err := st.Reload(); err != nil {
		return nil, err
	}
	return st, nil
}

// Reload re-reads the rules file. On error the current rules are kept and
// the error is recorded in Status.
func (st *Store) Reload() error {
	if st.path == "" {
		return nil
	}
	s, err := ParseFile(st.path)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastErr = err
	if err != nil {
		return err
	}
	st.set.Store(s)
	st.loadedAt = time.Now()
	return nil
}

// Path returns the rules file path.
func (st *Store) Path() string { return st.path }

// Lookup returns the rule applying to host in the current set.
func (st *Store) Lookup(host string) (Rule, bool) { return st.set.Load().Lookup(host) }

// Resolve returns the forced IP and port for host in the current set.
func (st *Store) Resolve(host string) (netip.Addr, uint16, bool) {
	return st.set.Load().Resolve(host)
}

// Status returns a snapshot of the store state.
func (st *Store) Status() Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	return Status{
		Path:     st.path,
		Rules:    st.set.Load().Rules(),
		LoadedAt: st.loadedAt,
		LastErr:  st.lastErr,
	}
}
