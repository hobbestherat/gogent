package diag

import (
	"strings"
	"testing"
	"time"
)

// These tests exercise the bounded, observable diagnostic ring (issue #562) that
// the diag.Logger tees into. The ring is the in-memory surface for the in-app
// Logs window and the daemon's /api/logs/stream, so its bounds, fan-out and
// nil-safety are acceptance-critical.

func TestRing_NewRingClampsNonPositiveSize(t *testing.T) {
	t.Parallel()
	for _, in := range []int{0, -1, -100} {
		if got := NewRing(in).size; got != 1 {
			t.Fatalf("NewRing(%d).size = %d, want 1 (never unbounded-by-accident)", in, got)
		}
	}
}

func TestRing_SnapshotOldestFirstAndBounded(t *testing.T) {
	t.Parallel()
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		r.append(Record{Time: time.Unix(int64(i), 0), Text: string(rune('a' + i))})
	}
	got := r.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot len = %d, want 3 (bounded to size)", len(got))
	}
	// The two oldest (a, b) are dropped; the ring keeps the newest three in order.
	if got[0].Text != "c" || got[1].Text != "d" || got[2].Text != "e" {
		t.Fatalf("Snapshot = %v, want [c d e] oldest-first after trim", got)
	}
}

func TestRing_SnapshotIsACopy(t *testing.T) {
	t.Parallel()
	r := NewRing(4)
	r.append(Record{Text: "x"})
	snap := r.Snapshot()
	snap[0].Text = "mutated"
	if again := r.Snapshot(); again[0].Text != "x" {
		t.Fatalf("Snapshot is not a copy: mutating it changed the ring (%q)", again[0].Text)
	}
}

func TestRing_SubscribeReceivesLiveRecords(t *testing.T) {
	t.Parallel()
	r := NewRing(10)
	ch, unsub := r.Subscribe()
	defer unsub()
	r.append(Record{Text: "hello"})
	select {
	case rec := <-ch:
		if rec.Text != "hello" {
			t.Fatalf("received %q, want hello", rec.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the live record")
	}
}

// TestRing_PrimeThenSubscribe covers the exact open-the-view pattern the Logs
// window uses: Snapshot() for history, then Subscribe() for the live tail.
func TestRing_PrimeThenSubscribe(t *testing.T) {
	t.Parallel()
	r := NewRing(10)
	r.append(Record{Text: "old"})
	hist := r.Snapshot()
	ch, unsub := r.Subscribe()
	defer unsub()
	r.append(Record{Text: "new"})

	if len(hist) != 1 || hist[0].Text != "old" {
		t.Fatalf("history = %+v, want [old]", hist)
	}
	select {
	case rec := <-ch:
		if rec.Text != "new" {
			t.Fatalf("live = %q, want new", rec.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive the record appended after Subscribe")
	}
}

// TestRing_SubscribeDropOnFullNeverBlocks is the backpressure guarantee: a
// subscriber that never drains must not stall append (and thus the logger / the
// agent loop). If the non-blocking select-default send regresses to a blocking
// send, this test times out.
func TestRing_SubscribeDropOnFullNeverBlocks(t *testing.T) {
	t.Parallel()
	r := NewRing(8)
	_, unsub := r.Subscribe()
	defer unsub()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			r.append(Record{Text: strings.Repeat("x", 64)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("append blocked on a full subscriber buffer — drop-on-full regressed")
	}
	if got := len(r.Snapshot()); got != 8 {
		t.Fatalf("Snapshot len = %d, want 8 after many appends", got)
	}
}

func TestRing_SubscribeMultipleSubscribersEachGetRecords(t *testing.T) {
	t.Parallel()
	r := NewRing(4)
	a, unA := r.Subscribe()
	defer unA()
	b, unB := r.Subscribe()
	defer unB()
	r.append(Record{Text: "m"})
	for _, ch := range []<-chan Record{a, b} {
		select {
		case rec := <-ch:
			if rec.Text != "m" {
				t.Fatalf("received %q, want m", rec.Text)
			}
		case <-time.After(time.Second):
			t.Fatal("a subscriber missed the fanned-out record")
		}
	}
}

func TestRing_UnsubscribeRemovesSubscriber(t *testing.T) {
	t.Parallel()
	r := NewRing(4)
	ch, unsub := r.Subscribe()
	unsub()
	r.append(Record{Text: "after"})
	// The unsubscribed channel must not receive the post-unsubscribe record.
	select {
	case rec, ok := <-ch:
		if ok {
			t.Fatalf("unsubscribed channel received %q after unsubscribe", rec.Text)
		}
	case <-time.After(50 * time.Millisecond):
		// expected: nothing delivered, channel left open but inert
	}
}

func TestRing_UnsubscribeIdempotent(t *testing.T) {
	t.Parallel()
	r := NewRing(4)
	_, unsub := r.Subscribe()
	unsub()
	unsub() // the sync.Once guard must keep this from panicking
}

func TestRing_NilRingSafe(t *testing.T) {
	t.Parallel()
	var r *Ring
	r.append(Record{Text: "x"}) // must not panic
	if got := r.Snapshot(); got != nil {
		t.Fatalf("nil Snapshot = %v, want nil", got)
	}
	ch, unsub := r.Subscribe()
	defer unsub()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("nil-ring Subscribe channel should be closed immediately")
		}
	case <-time.After(time.Second):
		t.Fatal("nil-ring Subscribe channel should be closed immediately")
	}
}
