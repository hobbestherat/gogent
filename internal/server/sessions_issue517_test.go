package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Issue #517: GET /sessions gains optional ?live=&limit=&offset= bounding params.
// These tests pin the bounded-mode contract (archived exclusion, recency order,
// clamped pagination) and the back-compat guarantee (no params ⇒ unchanged full,
// ID-ascending listing).

// seedLive persists a live (in-memory) session with a controlled CreatedAt so the
// recency ordering is deterministic. RenameSession → persistSession writes the
// on-disk index the listing reads, and CreatedAt (an exported int64 Unix field on
// the UserSession) is read at save time, so setting it before persist pins the
// index's RFC3339 created_at.
func seedLive(t *testing.T, srv *Server, id string, createdAt int64, title string) {
	t.Helper()
	us := srv.g.NewSession(id)
	if us == nil {
		t.Fatalf("NewSession(%q) returned nil", id)
	}
	us.CreatedAt = createdAt
	srv.g.RenameSession(id, title)
	// Confirm the index was actually written so a silent persist failure surfaces
	// here rather than as a confusing assertion miss downstream.
	for _, m := range srv.g.ListSessions() {
		if m.ID == id {
			return
		}
	}
	t.Fatalf("seedLive: session %q did not appear in ListSessions after persist", id)
}

// seedArchived persists then archives a session, leaving it on disk but out of
// memory — the archived/closed case criterion C must exclude from the default
// (live) restore set.
func seedArchived(t *testing.T, srv *Server, id string, createdAt int64, title string) {
	t.Helper()
	seedLive(t, srv, id, createdAt, title)
	srv.g.RemoveSession(id) // archives the index, removes from memory → Live=false
}

// listSessions is a tiny helper: GET /api/sessions?<rawQuery> as loopback and
// decode the sessionView slice.
func listSessions(t *testing.T, srv *Server, rawQuery string) []sessionView {
	t.Helper()
	target := "/api/sessions"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	rec := serveOne(t, srv, loopbackReq(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions?%s: status = %d, want 200; body=%s", rawQuery, rec.Code, rec.Body.String())
	}
	var views []sessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode sessions: %v; body=%s", err, rec.Body.String())
	}
	return views
}

func idsOf(views []sessionView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.ID
	}
	return out
}

func TestListSessionsNoParamsIsFullIDAscending(t *testing.T) {
	// Back-compat: with no query params the listing is the full set in ID-ascending
	// order, exactly as before #517. Any other caller (e.g. a future browser) must
	// not see its ordering or contents change.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	seedLive(t, srv, "sess-charlie", 3000, "C")
	seedLive(t, srv, "sess-alpha", 1000, "A")
	seedLive(t, srv, "sess-bravo", 2000, "B")

	got := idsOf(listSessions(t, srv, ""))
	want := []string{"sess-alpha", "sess-bravo", "sess-charlie"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("no-param order = %v, want %v (full, ID-ascending)", got, want)
	}
}

func TestListSessionsLiveExcludesArchived(t *testing.T) {
	// Criterion C: ?live=true returns only live sessions, excluding archived/closed
	// ones (persisted-but-not-live). The archived session must still be present in
	// the legacy (param-less) listing.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	seedLive(t, srv, "live-1", 1000, "Live")
	seedArchived(t, srv, "archived-1", 2000, "Archived")

	live := listSessions(t, srv, "live=true")
	if len(live) != 1 || live[0].ID != "live-1" || !live[0].Live {
		t.Fatalf("?live=true = %+v, want only live-1 (Live=true); archived must be excluded", live)
	}

	// Legacy listing still surfaces the archived session (Live=false).
	legacy := listSessions(t, srv, "")
	var sawLive, sawArchived bool
	for _, v := range legacy {
		if v.ID == "live-1" {
			sawLive = true
		}
		if v.ID == "archived-1" {
			sawArchived = true
			if v.Live {
				t.Fatalf("archived-1 reported Live=true in legacy listing: %+v", v)
			}
		}
	}
	if !sawLive || !sawArchived {
		t.Fatalf("legacy listing = %+v, want both live-1 and archived-1", legacy)
	}
}

func TestListSessionsBoundedIsRecencyOrdered(t *testing.T) {
	// Bounded mode orders most-recent-first by CreatedAt (descending), the natural
	// order for "give me the recent N" — distinct from the legacy ID-ascending.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	seedLive(t, srv, "old", 1000, "old") // CreatedAt 1970-…:16:40Z
	seedLive(t, srv, "new", 3000, "new") // CreatedAt 1970-…:50:00Z
	seedLive(t, srv, "mid", 2000, "mid") // CreatedAt 1970-…:33:20Z

	got := idsOf(listSessions(t, srv, "live=true"))
	want := []string{"new", "mid", "old"} // recency descending
	if len(got) != 3 || got[0] != "new" || got[1] != "mid" || got[2] != "old" {
		t.Fatalf("?live=true order = %v, want %v (most-recent-first)", got, want)
	}

	// Bounding via limit alone (no live) must ALSO be recency-ordered — it opts into
	// bounded mode, so it is not the legacy ID-asc order.
	byLimit := idsOf(listSessions(t, srv, "limit=10"))
	if len(byLimit) != 3 || byLimit[0] != "new" || byLimit[1] != "mid" || byLimit[2] != "old" {
		t.Fatalf("?limit=10 order = %v, want recency %v (bounded mode applies)", byLimit, want)
	}
}

func TestListSessionsLimitOffsetPages(t *testing.T) {
	// ?offset=/?limit= page the (recency-ordered) live set. Out-of-range values
	// clamp to a valid (possibly empty) slice rather than erroring.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	seedLive(t, srv, "old", 1000, "")
	seedLive(t, srv, "mid", 2000, "")
	seedLive(t, srv, "new", 3000, "")
	// Recency order: [new, mid, old]

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"first page", "live=true&limit=2", []string{"new", "mid"}},
		{"second page", "live=true&limit=2&offset=1", []string{"mid", "old"}},
		{"offset past page", "live=true&limit=2&offset=2", []string{"old"}},
		{"offset no limit", "live=true&offset=1", []string{"mid", "old"}},
		{"limit larger than set", "live=true&limit=100", []string{"new", "mid", "old"}},
		{"offset beyond end clamps to empty", "live=true&offset=100", nil},
		{"offset at end is empty", "live=true&offset=3", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := idsOf(listSessions(t, srv, tc.query))
			if len(got) != len(tc.want) {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
				}
			}
		})
	}
}

func TestListSessionsLiveTruthinessValues(t *testing.T) {
	// Only "true"/"1" enable the live filter; other spellings opt into bounded mode
	// (recency order) but do NOT filter, so the archived session remains.
	srv, _, _ := newTestServer(t, Options{Password: "x"})
	seedLive(t, srv, "live-1", 1000, "")
	seedArchived(t, srv, "archived-1", 2000, "")

	for _, val := range []string{"true", "1"} {
		t.Run("truthy/"+val, func(t *testing.T) {
			got := listSessions(t, srv, "live="+val)
			if len(got) != 1 || got[0].ID != "live-1" {
				t.Fatalf("live=%q => %+v, want only live-1", val, got)
			}
		})
	}
	// "false"/"0"/"yes" do not filter: both live and archived are returned (in
	// bounded/recency order). This documents that an explicit non-truthy live value
	// still enters bounded mode.
	for _, val := range []string{"false", "0", "yes"} {
		t.Run("nontruthy/"+val, func(t *testing.T) {
			got := listSessions(t, srv, "live="+val)
			if len(got) != 2 {
				t.Fatalf("live=%q => %+v, want both sessions (filter not applied)", val, got)
			}
		})
	}
}

// --- pure unit tests for the extracted helpers --------------------------------

func TestLiveTruthy(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"1":     true,
		"":      false,
		"false": false,
		"0":     false,
		"yes":   false,
		"TRUE":  false, // case-sensitive: only the lowercase token counts
		"true ": false,
	}
	for in, want := range cases {
		if got := liveTruthy(in); got != want {
			t.Errorf("liveTruthy(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPaginateClampsAndWindows(t *testing.T) {
	// paginate owns the "clamp, never error" edge-case contract (#517). It is pure,
	// so exhaustively table-driving it is the most direct way to lock the bounds.
	mk := func(n int) []sessionView {
		out := make([]sessionView, n)
		for i := range out {
			out[i] = sessionView{ID: string(rune('a' + i))}
		}
		return out
	}
	all := mk(4) // [a b c d]

	cases := []struct {
		name   string
		offset int
		limit  int
		want   string
	}{
		{"no cap", 0, 0, "abcd"},
		{"limit from start", 0, 2, "ab"},
		{"limit mid window", 1, 2, "bc"},
		{"offset no limit", 2, 0, "cd"},
		{"offset at len", 4, 2, ""},
		{"limit exceeds len", 0, 100, "abcd"},
		{"offset beyond len clamps", 100, 2, ""},
		{"negative offset treated as 0", -1, 2, "ab"},
		{"negative limit means no cap", 1, -5, "bcd"},
		{"zero limit means no cap", 1, 0, "bcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := paginate(all, tc.offset, tc.limit)
			gotIDs := ""
			for _, v := range got {
				gotIDs += v.ID
			}
			if gotIDs != tc.want {
				t.Fatalf("paginate(offset=%d, limit=%d) = %q, want %q", tc.offset, tc.limit, gotIDs, tc.want)
			}
		})
	}

	// Empty/nil inputs never panic.
	if got := paginate(nil, 0, 2); len(got) != 0 {
		t.Fatalf("paginate(nil,…) = %v, want empty", got)
	}
	if got := paginate([]sessionView{}, 5, 5); len(got) != 0 {
		t.Fatalf("paginate(empty,…) = %v, want empty", got)
	}
}

func TestPaginateDoesNotMutateUntouchedPrefix(t *testing.T) {
	// paginate returns a sub-slice of the input (it must not reallocate or reorder),
	// so the caller's ordering is preserved exactly. Assert the window shares the
	// backing array's values for the chosen page.
	src := []sessionView{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := paginate(src, 1, 1)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("paginate([a b c],1,1) = %+v, want [b]", got)
	}
}
