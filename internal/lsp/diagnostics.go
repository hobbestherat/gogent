package lsp

import (
	"sync"
	"time"
)

// debounceWindow is how long the freshness wait lets a burst of pushes settle
// after the correlating version arrives, so several rapid publishDiagnostics
// collapse to one settled set (the LSP support design §3, §11.4).
const debounceWindow = 150 * time.Millisecond

// freshnessCeiling bounds the freshness wait for servers that neither version
// their pushes nor report progress idle (§11.4, fallback 3).
const freshnessCeiling = 3 * time.Second

// diagnosticsStore is the per-file push cache plus the version-keyed freshness
// machinery (the LSP support design §11.4). Push diagnostics are deduped and
// cached per path; a waiter registers the version it expects *before* the
// owning didOpen/didChange is sent (§9), so a fast push for that version is never
// dropped. It carries its own lock and condition so the "register before send"
// ordering is race-free without entangling the Client mutex.
type diagnosticsStore struct {
	mu     sync.Mutex
	cond   *sync.Cond
	byPath map[string]*diagEntry
	// progress tracks in-flight work-done-progress streams by token. The server is
	// "idle" (the §11.4 fallback-2 signal) only when zero streams are outstanding, so
	// one stream's "end" cannot mark the server idle while another is still running.
	progress map[string]struct{}
}

type diagEntry struct {
	diags      []Diagnostic
	version    int32 // version of the most recent versioned push
	hasVersion bool  // the most recent push carried a version
	pushSeq    int   // total pushes received (monotonic), used by the idle fallback
	awaited    int32 // version a waiter is correlating on
	lastPush   time.Time
}

func newDiagnosticsStore() *diagnosticsStore {
	s := &diagnosticsStore{byPath: map[string]*diagEntry{}, progress: map[string]struct{}{}}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *diagnosticsStore) entry(path string) *diagEntry {
	e := s.byPath[path]
	if e == nil {
		e = &diagEntry{}
		s.byPath[path] = e
	}
	return e
}

// expect records the version the next settled read of path correlates on. It
// MUST be called before the didOpen/didChange that bumps the document to version
// is sent (§9), so a fast push for that version is never dropped. Keying on the
// last version sent via either didOpen or didChange means a freshly opened,
// unedited file correlates on its didOpen version rather than the slow fallback.
func (s *diagnosticsStore) expect(path string, version int32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry(path).awaited = version
}

// publish records a push for path: the deduped diagnostics and, when present, the
// version they reflect. It broadcasts so any blocked wait re-evaluates.
func (s *diagnosticsStore) publish(path string, version int32, hasVersion bool, diags []Diagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry(path)
	e.diags = dedupDiagnostics(diags)
	if hasVersion {
		e.version = version
		e.hasVersion = true
	}
	e.pushSeq++
	e.lastPush = time.Now()
	s.cond.Broadcast()
}

// progressBegin records that a work-done-progress stream identified by token is
// in flight (server busy). It is idempotent for a token, so a server that both
// creates a token and then sends a "begin" for it is counted once.
func (s *diagnosticsStore) progressBegin(token string) {
	s.mu.Lock()
	s.progress[token] = struct{}{}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// progressEnd records that the stream identified by token finished. The server
// is treated as idle only once every outstanding stream has ended (§11.4).
func (s *diagnosticsStore) progressEnd(token string) {
	s.mu.Lock()
	delete(s.progress, token)
	s.cond.Broadcast()
	s.mu.Unlock()
}

// isIdleLocked reports whether no work-done-progress stream is in flight. The
// caller holds s.mu.
func (s *diagnosticsStore) isIdleLocked() bool {
	return len(s.progress) == 0
}

// resetEntry clears an entry's cached push state in place while preserving the
// awaited version, so an in-flight freshness waiter keeps its correlation target
// and re-waits for the next push rather than being stranded. The caller holds s.mu.
func resetEntry(e *diagEntry) {
	e.diags = nil
	e.version = 0
	e.hasVersion = false
	e.pushSeq = 0
	e.lastPush = time.Time{}
}

// invalidate drops the cached push set for path (e.g. on a pull-cache refresh
// request) so the next read re-pulls or re-waits. It resets the entry in place
// rather than deleting it so a concurrent waiter's pointer stays valid, then wakes
// any waiter.
func (s *diagnosticsStore) invalidate(path string) {
	s.mu.Lock()
	if e := s.byPath[path]; e != nil {
		resetEntry(e)
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// invalidateAll drops every cached push set (e.g. on workspace/diagnostic/refresh).
// It resets entries in place (not replacing the map) so an in-flight wait that
// already holds an entry pointer is not orphaned, and broadcasts so each waiter
// re-evaluates and re-waits for a fresh push (§11.4).
func (s *diagnosticsStore) invalidateAll() {
	s.mu.Lock()
	for _, e := range s.byPath {
		resetEntry(e)
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// current returns the cached diagnostics for path without waiting.
func (s *diagnosticsStore) current(path string) []Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.byPath[path]; e != nil {
		return append([]Diagnostic(nil), e.diags...)
	}
	return nil
}

// wait blocks until the cached diagnostics for path are settled — ranked by
// reliability (§11.4): a versioned push for the awaited version (primary), an
// unversioned push while the server is idle (fallback), or the freshness ceiling
// (final fallback). It returns the deduped set and whether it actually settled
// (false means the ceiling fired first, so a pull-capable caller may re-pull). A
// deadline goroutine broadcasts so the cond loop always makes progress.
func (s *diagnosticsStore) wait(path string) (diags []Diagnostic, settled bool) {
	deadline := time.Now().Add(freshnessCeiling)
	// A single ticker re-evaluates the debounce-since-last-push check, and the
	// ceiling timer guarantees the loop terminates; both just broadcast.
	ticker := time.NewTicker(debounceWindow / 2)
	defer ticker.Stop()
	ceiling := time.AfterFunc(freshnessCeiling, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer ceiling.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.mu.Lock()
				s.cond.Broadcast()
				s.mu.Unlock()
			}
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		// Re-fetch the entry every iteration: an invalidate/refresh resets it in
		// place (and never replaces the map), but re-fetching keeps the wait correct
		// even if the entry table is ever rebuilt under us.
		e := s.entry(path)
		// Version correlation (primary): a push reported the version we last sent,
		// so the findings reflect the latest content rather than a stale-empty set.
		versioned := e.hasVersion && e.version >= e.awaited
		// Idle fallback: a server that omits the version field is settled once it
		// has pushed at least once and reports no in-flight work (§11.4, fallback 2).
		idleFallback := e.pushSeq > 0 && s.isIdleLocked()
		if (versioned || idleFallback) && time.Since(e.lastPush) >= debounceWindow {
			return append([]Diagnostic(nil), e.diags...), true
		}
		if time.Now().After(deadline) {
			return append([]Diagnostic(nil), e.diags...), false
		}
		s.cond.Wait()
	}
}

// dedupDiagnostics removes duplicate findings, keyed by
// (code, severity, message, source, range), preserving first-seen order (§3).
func dedupDiagnostics(in []Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	type key struct {
		code, source, message string
		severity              int
		rng                   Range
	}
	seen := make(map[key]bool, len(in))
	out := make([]Diagnostic, 0, len(in))
	for _, d := range in {
		k := key{d.Code, d.Source, d.Message, d.Severity, d.Range}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, d)
	}
	return out
}
