package diag

import (
	"log/slog"
	"sync"
	"time"
)

// Record is one captured diagnostic line. It carries enough structure to colour
// by level and order by time, with the fully-formatted (and already-redacted)
// message text — Secret values are resolved through slog before the text is
// rendered, exactly as on the file/stderr sink (see ringHandler in logger.go).
type Record struct {
	Time  time.Time
	Level slog.Level
	Text  string // "msg key=value …" with no trailing newline
}

// ringDefaultSubBuffer is the per-subscriber channel depth. A subscriber that
// falls this far behind drops records (best-effort live tail) rather than
// stalling the logger — the same discipline the server hub uses for events.
const ringDefaultSubBuffer = 256

// Ring is a bounded, observable in-memory buffer of recent diagnostic records.
// The diag.Logger tees every record into it (in addition to the file/stderr
// sink) so the TUI can surface logs in-app and the daemon can stream them to
// remote clients. It keeps a rolling history (for priming a viewer on open) and
// fans new records out to live subscribers without blocking the logger.
//
// A nil *Ring is a safe no-op for append, so a Logger built with a ring can be
// used identically whether or not one was supplied.
type Ring struct {
	mu   sync.Mutex
	buf  []Record
	size int
	subs map[chan Record]struct{}
}

// NewRing returns a Ring retaining at most size records of history. A size <= 0
// is treated as 1 so the buffer is never unbounded-by-accident.
func NewRing(size int) *Ring {
	if size <= 0 {
		size = 1
	}
	return &Ring{
		buf:  make([]Record, 0, size),
		size: size,
		subs: make(map[chan Record]struct{}),
	}
}

// append records rec into the rolling history (dropping the oldest past the
// bound) and fans it out to every live subscriber without blocking: a full
// subscriber buffer drops the record rather than stalling the caller. It is
// unexported because only the diag logger's ring handler produces records.
func (r *Ring) append(rec Record) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, rec)
	if len(r.buf) > r.size {
		r.buf = r.buf[len(r.buf)-r.size:]
	}
	for ch := range r.subs {
		select {
		case ch <- rec:
		default:
		}
	}
}

// Snapshot returns a copy of the current history, oldest first. A viewer calls
// it to prime itself with recent records before subscribing to live updates.
func (r *Ring) Snapshot() []Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.buf))
	copy(out, r.buf)
	return out
}

// Subscribe registers a live subscriber and returns its receive channel plus an
// unsubscribe func. The channel is buffered; records arriving while it is full
// are dropped (best-effort live tail). The caller MUST call the returned func
// when done to release the subscription. A nil *Ring yields a closed channel and
// a no-op unsubscribe so callers need no nil checks.
func (r *Ring) Subscribe() (<-chan Record, func()) {
	if r == nil {
		ch := make(chan Record)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan Record, ringDefaultSubBuffer)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, ch)
			r.mu.Unlock()
		})
	}
}
