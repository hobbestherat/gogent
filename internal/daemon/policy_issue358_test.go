package daemon

import "testing"

func TestIssue358ClassifyStatusMatrix(t *testing.T) {
	tests := []struct {
		name       string
		pidPresent bool
		pidAlive   bool
		residue    bool
		live       bool
		wantRun    bool
		wantStale  bool
	}{
		{
			name: "no lifecycle files and no liveness",
		},
		{
			name:       "healthy pid and transport is running",
			pidPresent: true,
			pidAlive:   true,
			residue:    true,
			live:       true,
			wantRun:    true,
		},
		{
			name:      "live transport without pidfile is stale",
			residue:   true,
			live:      true,
			wantStale: true,
		},
		{
			name:       "alive pid without live transport is stale",
			pidPresent: true,
			pidAlive:   true,
			wantStale:  true,
		},
		{
			name:       "dead pidfile is stale",
			pidPresent: true,
			wantStale:  true,
		},
		{
			name:      "transport residue alone is stale",
			residue:   true,
			wantStale: true,
		},
		{
			name:      "pidAlive ignored when pidfile absent",
			pidAlive:  true,
			residue:   true,
			wantStale: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRun, gotStale := ClassifyStatus(tt.pidPresent, tt.pidPresent && tt.pidAlive, tt.residue, tt.live)
			if gotRun != tt.wantRun || gotStale != tt.wantStale {
				t.Fatalf("ClassifyStatus() = running %v stale %v, want running %v stale %v",
					gotRun, gotStale, tt.wantRun, tt.wantStale)
			}
		})
	}
}

func TestIssue358StopModeIsChosenOnlyFromTransportLiveness(t *testing.T) {
	if got := DecideStopMode(false); got != StopModeReclaim {
		t.Fatalf("DecideStopMode(false) = %v, want StopModeReclaim", got)
	}
	if got := DecideStopMode(true); got != StopModeGraceful {
		t.Fatalf("DecideStopMode(true) = %v, want StopModeGraceful", got)
	}
}
