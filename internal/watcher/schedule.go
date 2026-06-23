// Package watcher is the scheduling engine for recurring "Watcher" agent
// actions: tasks that fire on their own cadence (every N minutes, or daily at a
// fixed local time), run a full agent task loop via the host, and either report
// into a user session (attached) or run independently (free-running).
//
// The package is deliberately self-contained — it depends only on the standard
// library (time/context/sync) and knows nothing about gogent, sessions, config,
// notifications or the TUI. The host wires the engine to the rest of the system
// through the small WatcherHost seam (see manager.go). This keeps the scheduling
// core unit-testable with fakes and free of import cycles; later phases (gogent
// StartWatchers wiring, attached watchers, tools, TUI, HTTP API) build on this
// exact API.
//
// Scheduling is decentralised: there is no central tick loop. Each enabled
// Runner owns one sleeping goroutine that arms a timer for its next fire, wakes,
// fires, and re-arms. This keeps each watcher independent and cheap.
package watcher

import "time"

// Schedule computes when a watcher should next fire.
//
// Next returns the next fire instant strictly after now. Implementations must be
// safe to call repeatedly and from a single goroutine per Runner (the manager
// never calls Next for the same Runner concurrently).
type Schedule interface {
	// Next returns the next fire time strictly after now. There is no
	// catch-up: a missed window is not replayed, the schedule simply advances
	// to the next future occurrence.
	Next(now time.Time) time.Time
}

// IntervalSchedule fires every D after the previous arming. Next is simply
// now.Add(D), so the first fire after Start is one interval out (no immediate
// catch-up burst) and subsequent fires are spaced one interval apart relative to
// when each fire was scheduled.
type IntervalSchedule struct {
	// D is the interval between fires. A non-positive D is clamped to a single
	// nanosecond by Next so the schedule still advances rather than spinning on
	// an instant in the past.
	D time.Duration
}

// Next returns now.Add(D).
func (s IntervalSchedule) Next(now time.Time) time.Time {
	d := s.D
	if d <= 0 {
		d = time.Nanosecond
	}
	return now.Add(d)
}

// DailySchedule fires once per day at Hour:Min in Loc. The next fire is today's
// Hour:Min if it is still strictly in the future, otherwise tomorrow's.
//
// The target instant is constructed with time.Date in Loc, so for an ordinary
// day the local wall-clock time resolves to the correct absolute instant
// (including the usual ±1h offset shift across a DST boundary). The wall-clock
// gap and fold cases are handled by Go's normalization rather than special-
// cased: on a spring-forward day a Hour:Min that does not exist locally (e.g.
// 02:30 when the clock jumps 02:00→03:00) is normalized by time.Date to a valid
// neighbouring instant, and on a fall-back day a Hour:Min that occurs twice
// resolves to the first occurrence. Both are reasonable for a daily watcher;
// callers needing exact gap/fold semantics should use an interval schedule.
type DailySchedule struct {
	Hour int // 0-23
	Min  int // 0-59
	Loc  *time.Location
}

// Next returns the next occurrence of Hour:Min in Loc strictly after now. If Loc
// is nil, UTC is used.
func (s DailySchedule) Next(now time.Time) time.Time {
	loc := s.Loc
	if loc == nil {
		loc = time.UTC
	}
	n := now.In(loc)
	candidate := time.Date(n.Year(), n.Month(), n.Day(), s.Hour, s.Min, 0, 0, loc)
	// Strictly after now: if today's slot has already passed (or is exactly
	// now), roll to tomorrow. Adding 24h to the wall-clock date via AddDate
	// keeps the schedule DST-correct.
	if !candidate.After(now) {
		candidate = time.Date(n.Year(), n.Month(), n.Day()+1, s.Hour, s.Min, 0, 0, loc)
	}
	return candidate
}
