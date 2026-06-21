package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// ptr is a local *int helper for building Config values in tests (the in-package
// intPtr is unexported; a local alias keeps the expectations readable).
func ptr(v int) *int { return &v }

// TestDefaultMaxStepsMatchesHistoricalBound guards the issue #249 invariant that
// an unset max_steps reproduces gogent's prior fixed cap exactly, so older
// config.json files behave as before.
func TestDefaultMaxStepsMatchesHistoricalBound(t *testing.T) {
	if DefaultMaxSteps != 25 {
		t.Errorf("DefaultMaxSteps = %d, want 25 (the historical hardcoded bound)", DefaultMaxSteps)
	}
}

// TestMaxStepsOrDefaultNilFieldGivesDefault verifies an absent max_steps (nil
// pointer) resolves to the built-in default rather than 0.
func TestMaxStepsOrDefaultNilFieldGivesDefault(t *testing.T) {
	c := &Config{} // MaxSteps left nil — the "key absent" case
	if got := c.MaxStepsOrDefault(); got != DefaultMaxSteps {
		t.Errorf("nil MaxSteps -> %d, want default %d", got, DefaultMaxSteps)
	}
}

// TestMaxStepsOrDefaultNilReceiverIsSafe guards the wiring call site
// (g.config.MaxStepsOrDefault()): a nil Config must not panic.
func TestMaxStepsOrDefaultNilReceiverIsSafe(t *testing.T) {
	var c *Config
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil receiver panicked: %v", r)
		}
	}()
	if got := c.MaxStepsOrDefault(); got != DefaultMaxSteps {
		t.Errorf("nil Config receiver -> %d, want default %d", got, DefaultMaxSteps)
	}
}

// TestMaxStepsOrDefaultExplicitZeroIsUnlimited is the core regression for the
// *int pointer design: explicit 0 (the "yolo"/unlimited sentinel) MUST pass
// through verbatim and NOT collapse to the default 25. A plain int field — where
// 0 is indistinguishable from "unset" — would fail this and silently re-cap.
func TestMaxStepsOrDefaultExplicitZeroIsUnlimited(t *testing.T) {
	c := &Config{MaxSteps: ptr(0)}
	if got := c.MaxStepsOrDefault(); got != 0 {
		t.Errorf("explicit MaxSteps 0 -> %d, want 0 (unlimited, passed through verbatim)", got)
	}
}

func TestMaxStepsOrDefaultPositiveValuePassesThrough(t *testing.T) {
	c := &Config{MaxSteps: ptr(10)}
	if got := c.MaxStepsOrDefault(); got != 10 {
		t.Errorf("MaxSteps 10 -> %d, want 10", got)
	}
}

// TestMaxStepsOrDefaultNegativeValuePassesThrough confirms a non-positive value
// is returned as-is; runLoop's `<= 0` check then treats it as unlimited. The
// accessor must not normalize negatives (e.g. to the default).
func TestMaxStepsOrDefaultNegativeValuePassesThrough(t *testing.T) {
	c := &Config{MaxSteps: ptr(-1)}
	if got := c.MaxStepsOrDefault(); got != -1 {
		t.Errorf("MaxSteps -1 -> %d, want -1 (returned verbatim)", got)
	}
}

// TestGetDefaultConfigExposesMaxSteps checks a freshly written default config
// documents the setting and resolves to the safe historical bound (0 here would
// mean unlimited, which is NOT an acceptable default).
func TestGetDefaultConfigExposesMaxSteps(t *testing.T) {
	cfg := GetDefaultConfig()
	if cfg.MaxSteps == nil {
		t.Fatal("GetDefaultConfig left MaxSteps nil; a fresh config should serialize the value so users see it")
	}
	if got := *cfg.MaxSteps; got != DefaultMaxSteps {
		t.Errorf("GetDefaultConfig MaxSteps = %d, want %d", got, DefaultMaxSteps)
	}
	if got := cfg.MaxStepsOrDefault(); got != DefaultMaxSteps {
		t.Errorf("GetDefaultConfig MaxStepsOrDefault = %d, want %d", got, DefaultMaxSteps)
	}
}

// TestMaxStepsRoundTripsThroughJSON covers the save/load cycle for every shape.
// The explicit-0 case is the critical one: it must serialize as "max_steps":0
// (omitempty on a *int only drops a nil pointer, not a pointer to 0) and reload
// to an unlimited 0, never to the default 25.
func TestMaxStepsRoundTripsThroughJSON(t *testing.T) {
	cases := []struct {
		name    string
		value   *int
		wantSub string // expected substring in serialized JSON, or "" if the key must be absent
		want    int    // expected MaxStepsOrDefault after a marshal/unmarshal cycle
	}{
		{"absent", nil, "", DefaultMaxSteps},
		{"explicit 25", ptr(25), `"max_steps":25`, 25},
		{"explicit 0 unlimited", ptr(0), `"max_steps":0`, 0},
		{"explicit 7", ptr(7), `"max_steps":7`, 7},
		{"explicit negative", ptr(-3), `"max_steps":-3`, -3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(&Config{MaxSteps: tc.value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tc.wantSub == "" {
				if strings.Contains(string(raw), "max_steps") {
					t.Errorf("nil MaxSteps must be omitted; JSON was %s", raw)
				}
			} else if !strings.Contains(string(raw), tc.wantSub) {
				t.Errorf("serialized JSON %s missing %s", raw, tc.wantSub)
			}

			var loaded Config
			if err := json.Unmarshal(raw, &loaded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := loaded.MaxStepsOrDefault(); got != tc.want {
				t.Errorf("after round-trip MaxStepsOrDefault = %d, want %d (raw=%s)", got, tc.want, raw)
			}
		})
	}
}

// TestMaxStepsExplicitZeroSurvivesConfigFileLoad is the focused regression for
// the issue's headline requirement: a user writing {"max_steps": 0} to opt into
// unlimited MUST get 0 back after loading. If the field were a plain int (or
// omitempty on a plain int), 0 would round-trip to nil/25 and silently re-enable
// the 25-step cap — the exact regression #249 exists to prevent.
func TestMaxStepsExplicitZeroSurvivesConfigFileLoad(t *testing.T) {
	const doc = `{"max_steps": 0}`
	var loaded Config
	if err := json.Unmarshal([]byte(doc), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.MaxSteps == nil {
		t.Fatal(`"max_steps": 0 loaded as nil; explicit 0 must survive as a non-nil pointer`)
	}
	if got := loaded.MaxStepsOrDefault(); got != 0 {
		t.Errorf(`loaded "max_steps": 0 resolved to %d, want 0 (unlimited)`, got)
	}
}

// TestGetDefaultConfigRoundTripsMaxSteps verifies the default config survives a
// save/load cycle and still resolves to the historical bound.
func TestGetDefaultConfigRoundTripsMaxSteps(t *testing.T) {
	raw, err := json.Marshal(GetDefaultConfig())
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	if !strings.Contains(string(raw), `"max_steps":25`) {
		t.Errorf("default config JSON missing \"max_steps\":25; got %s", raw)
	}
	var loaded Config
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal default: %v", err)
	}
	if got := loaded.MaxStepsOrDefault(); got != DefaultMaxSteps {
		t.Errorf("round-tripped default resolved to %d, want %d", got, DefaultMaxSteps)
	}
}
