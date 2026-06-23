package watcher_test

import (
	"errors"
	"testing"
	"time"

	"gogent/internal/watcher"
)

// ---------------------------------------------------------------------------
// SuppressNotify (issue #329 fixes round 1 — honor on_complete.notify=false)
// ---------------------------------------------------------------------------

// suppressRunner builds a free-running runner with an explicit SuppressNotify flag
// (the shared newRunner helper always leaves it false).
func suppressRunner(id, name string, suppress bool) *watcher.Runner {
	return watcher.NewRunner(watcher.Spec{
		ID:             id,
		Name:           name,
		Task:           "task for " + name,
		Kind:           watcher.KindFree,
		Schedule:       constSchedule{time.Hour},
		Enabled:        true,
		SuppressNotify: suppress,
	})
}

// TestSuppressNotifyFreeRunningDoesNotNotify proves a free-running watcher with
// SuppressNotify=true completes a fire but emits no completion notification — the
// on_complete.notify=false path. (Without the fix the manager notified
// unconditionally on every successful free-running fire.)
func TestSuppressNotifyFreeRunningDoesNotNotify(t *testing.T) {
	host := &fakeHost{setResult: "did the thing"}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	if err := m.Add(suppressRunner("w1", "silent", true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.RunNow("w1"); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	// The fire must run to completion...
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() == 1 }) {
		t.Fatalf("suppressed watcher did not fire")
	}
	// ...but never notify. Give any erroneous notify a chance to land first.
	time.Sleep(40 * time.Millisecond)
	if got := len(host.notifyList()); got != 0 {
		t.Fatalf("notify-suppressed watcher notified %d times, want 0", got)
	}
}

// TestSuppressNotifyIsPerRunner proves the suppression is per-watcher, not global:
// a suppressed and a non-suppressed free-running watcher fire together, and only
// the non-suppressed one notifies.
func TestSuppressNotifyIsPerRunner(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	m.Start()
	defer m.Stop()
	if err := m.Add(suppressRunner("loud", "loud", false)); err != nil {
		t.Fatalf("Add loud: %v", err)
	}
	if err := m.Add(suppressRunner("quiet", "quiet", true)); err != nil {
		t.Fatalf("Add quiet: %v", err)
	}
	if err := m.RunNow("loud"); err != nil {
		t.Fatalf("RunNow loud: %v", err)
	}
	if err := m.RunNow("quiet"); err != nil {
		t.Fatalf("RunNow quiet: %v", err)
	}
	// Wait until both fired.
	if !waitFor(t, waitTimeout, func() bool { return host.fireCount() == 2 }) {
		t.Fatalf("expected 2 fires, got %d", host.fireCount())
	}
	if !waitFor(t, waitTimeout, func() bool { return len(host.notifyList()) == 1 }) {
		t.Fatalf("expected exactly 1 notify, got %d", len(host.notifyList()))
	}
	// Settle, then confirm it stayed at exactly one and it was the loud one.
	time.Sleep(40 * time.Millisecond)
	list := host.notifyList()
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 notify after settle, got %d", len(list))
	}
	if list[0].title != "loud" {
		t.Fatalf("the notifying watcher was %q, want \"loud\"", list[0].title)
	}
}

// ---------------------------------------------------------------------------
// Resolve vs Get (issue #329 fixes round 1 — distinguish ambiguous from missing)
// ---------------------------------------------------------------------------

// TestResolveDistinguishesAmbiguousFromNotFound proves Manager.Resolve returns the
// specific resolution error: ErrAmbiguous for a name matching two watchers,
// ErrNotFound for an unknown key, and the exact watcher for a unique name or any
// id (ids stay unambiguous even when a name collides).
func TestResolveDistinguishesAmbiguousFromNotFound(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	// Two watchers sharing the name "dup", plus one unique name.
	if err := m.Add(newRunner("id-1", "dup", watcher.KindFree, "", constSchedule{time.Hour}, true)); err != nil {
		t.Fatalf("Add id-1: %v", err)
	}
	if err := m.Add(newRunner("id-2", "dup", watcher.KindFree, "", constSchedule{time.Hour}, true)); err != nil {
		t.Fatalf("Add id-2: %v", err)
	}
	if err := m.Add(newRunner("id-3", "solo", watcher.KindFree, "", constSchedule{time.Hour}, true)); err != nil {
		t.Fatalf("Add id-3: %v", err)
	}

	if _, err := m.Resolve("dup"); !errors.Is(err, watcher.ErrAmbiguous) {
		t.Errorf("Resolve(ambiguous name) err = %v, want ErrAmbiguous", err)
	}
	if _, err := m.Resolve("missing"); !errors.Is(err, watcher.ErrNotFound) {
		t.Errorf("Resolve(unknown) err = %v, want ErrNotFound", err)
	}
	if info, err := m.Resolve("solo"); err != nil || info.ID != "id-3" {
		t.Errorf("Resolve(unique name) = (%+v,%v), want id-3 and no error", info, err)
	}
	// An exact id resolves even though its name is ambiguous.
	if info, err := m.Resolve("id-1"); err != nil || info.ID != "id-1" {
		t.Errorf("Resolve(id under an ambiguous name) = (%+v,%v), want id-1 and no error", info, err)
	}
}

// TestGetStillCollapsesToOk confirms the back-compat Get wrapper keeps its
// boolean contract: ok=false for both ambiguous and missing (callers that need to
// tell them apart use Resolve).
func TestGetStillCollapsesToOk(t *testing.T) {
	host := &fakeHost{}
	m := watcher.NewManager(host)
	if err := m.Add(newRunner("g1", "dup", watcher.KindFree, "", constSchedule{time.Hour}, true)); err != nil {
		t.Fatalf("Add g1: %v", err)
	}
	if err := m.Add(newRunner("g2", "dup", watcher.KindFree, "", constSchedule{time.Hour}, true)); err != nil {
		t.Fatalf("Add g2: %v", err)
	}
	if _, ok := m.Get("dup"); ok {
		t.Error("Get(ambiguous) should report ok=false")
	}
	if _, ok := m.Get("nope"); ok {
		t.Error("Get(missing) should report ok=false")
	}
	if info, ok := m.Get("g1"); !ok || info.ID != "g1" {
		t.Errorf("Get(exact id) = (%+v,%v), want g1/true", info, ok)
	}
}
