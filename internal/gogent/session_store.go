package gogent

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
	"gogent/internal/config"
	"gogent/internal/model"
)

// Session transcripts are persisted as a set of sharded JSON-lines files plus a
// small per-session index (issue #26). A live session occupies a "base" prefix
//
//	<iso>_<id>_session
//
// laid out as
//
//	<base>.index              the index (meta + shard table; the source of truth)
//	<base>.0000.jsonl         shard 0 (oldest)
//	<base>.0001.jsonl         shard 1 ...
//
// Each shard holds only "message" records and is capped at shardMaxEvents
// records or shardMaxBytes bytes, whichever is hit first, so a single file can
// no longer grow without bound. The index stores the session meta (id/title/
// created) and the shard table, so listing sessions is O(sessions) rather than
// O(total events): it reads only the tiny index files instead of replaying every
// transcript line. Listing and restore read just the current (latest) shard —
// for any session that fits in one shard (the common case) that is the entire
// transcript, identical to before; only sessions that actually exceeded the cap
// start up showing their recent shard, with older shards still on disk.
//
// Archiving renames the base from "_session" to "_session_archived" across all
// of its files, so an archived session is skipped by listing. A crash leaves the
// index written last: as long as an index is present and points at shards that
// exist, the on-disk state is consistent.
const (
	activeBaseSuffix   = "_session"             // base prefix for a live session
	archivedTag        = "_archived"            // appended to the base to archive it
	indexFileExt       = ".index"               // the per-session index file
	shardFileExt       = ".jsonl"               // one shard of the transcript
	shardNumberWidth   = 4                      // zero-padded shard index in filenames
	shardMaxEvents     = 5000                   // roll a shard at this many message records
	shardMaxBytes      = 10 * 1024 * 1024       // roll a shard at ~10 MiB
	shardCacheCapacity = 16                     // parsed frozen shards kept hot in memory
	flushInterval      = 250 * time.Millisecond // cadence of the batched durability flush
)

// jsonlRecord is one line of a shard file. Shards hold only "message" records;
// "meta" exists solely so the legacy single-file format can still be read.
type jsonlRecord struct {
	Kind      string         `json:"kind"`
	SessionID string         `json:"session_id,omitempty"`
	Title     string         `json:"title,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Message   *model.Message `json:"message,omitempty"`
}

// sessionIndex is the small JSON document at <base>.index: the session meta plus
// the ordered shard table. It is the source of truth for listing and for the
// shard layout, and is rewritten (temp file + rename) whenever the layout or
// meta changes.
//
// The summary fields (Turns/TokensIn/TokensOut/Model) let the Sessions browser
// list per-session usage straight from the index without replaying any
// transcript (issue #58) — they are refreshed on every Save from the live
// session snapshot. Older index files predating them decode with zero values,
// which list cleanly; the next save backfills them.
type sessionIndex struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Turns     int    `json:"turns,omitempty"`
	TokensIn  int    `json:"tokens_in,omitempty"`
	TokensOut int    `json:"tokens_out,omitempty"`
	Model     string `json:"model,omitempty"`
	// ModelLabel and ModelID freeze the primary model's display label
	// (DisplayName) and provider model id (Model) as they were at save time, so
	// the Saved Sessions dialog can show the model the session actually ran on —
	// faithful history, independent of later config edits (issue #389). Older
	// index files predating these fields decode them as empty; the dialog then
	// falls back to the bare Model key. They are deliberately NOT live-resolved.
	ModelLabel string      `json:"model_label,omitempty"`
	ModelID    string      `json:"model_id,omitempty"`
	Shards     []shardMeta `json:"shards"`
	// Watchers holds the session's attached (session-scoped) watcher definitions so
	// they are restored with the session via OnSessionRestored (issue #329 Phase 3).
	// Free-running watchers live in ~/.gogent/watchers.json, never here. Older index
	// files predating the field decode as nil (no attached watchers).
	Watchers []config.WatcherConfig `json:"attached_watchers,omitempty"`
}

// sessionSummary is the lightweight per-session usage summary persisted in the
// index so ListSessions can show turns/tokens/model without replaying
// transcripts (issue #58). It is captured once per Save from the live session.
type sessionSummary struct {
	turns     int
	tokensIn  int
	tokensOut int
	model     string // primary model's stable config Name (lookup key)
	// modelLabel/modelID freeze the primary model's DisplayName and provider model
	// id at save time for the Saved Sessions dialog (issue #389). Empty when the
	// config can't be resolved — the dialog falls back to the bare model key.
	modelLabel string
	modelID    string
}

// shardMeta is one entry in the index's shard table.
type shardMeta struct {
	Index  int   `json:"index"`  // shard number (0-based, zero-padded in the filename)
	Events int   `json:"events"` // message records in this shard
	Bytes  int64 `json:"bytes"`  // shard file size
}

// SessionStore manages the sharded JSONL session files on disk.
type SessionStore struct {
	dir string

	mu     sync.Mutex
	base   map[string]string        // sessionID -> base prefix
	state  map[string]*persistState // sessionID -> per-agent persisted frontier
	shards map[string][]shardMeta   // sessionID -> shard table (last = active shard)
	cache  *shardCache              // parsed-shard LRU (hot turns)

	// flusher: the data writes themselves stay synchronous (an append is cheap
	// and keeps the file readable immediately + crash-recoverable), but the
	// expensive durability flush (fsync) is batched off the turn's critical path
	// by a lazily-scheduled task. A graceful shutdown should call Sync.
	flushMu sync.Mutex
	dirty   map[string]struct{}
	timer   *time.Timer
	stopped bool

	// attachedWatchersFn, when set, supplies a session's attached (session-scoped)
	// watcher configs to persist in its index so they restore with the session
	// (issue #329 Phase 3). The host (gogent) installs it via
	// SetAttachedWatchersProvider; nil persists no watchers. It is read under the
	// store lock during Save, so it must not call back into the store.
	attachedWatchersFn func(sessionID string) []config.WatcherConfig

	// modelSnapshotFn, when set, resolves a model's stable config Name to its
	// current DisplayName (label) and provider model id, captured at save time to
	// freeze a faithful display record in the session index (issue #389). The host
	// (gogent) installs it via SetModelSnapshotProvider; nil leaves the snapshot
	// empty (the dialog falls back to the bare model key). ok is false when the
	// name resolves to no config. Invoked under the store lock during Save, so it
	// must not re-enter the store.
	modelSnapshotFn func(name string) (label, id string, ok bool)
}

// SetModelSnapshotProvider installs the callback Save consults to freeze the
// primary model's display label and provider id in the session index, so the
// Saved Sessions dialog shows the model the session actually ran on rather than a
// bare config key that drifts after edits (issue #389). A nil fn leaves the
// snapshot empty. The provider is invoked while the store lock is held and must
// not re-enter the store.
func (s *SessionStore) SetModelSnapshotProvider(fn func(name string) (label, id string, ok bool)) {
	s.mu.Lock()
	s.modelSnapshotFn = fn
	s.mu.Unlock()
}

// SetAttachedWatchersProvider installs the callback Save consults for a session's
// attached (session-scoped) watcher configs, so they are written into the session
// index and restored with it (issue #329 Phase 3). A nil fn persists none. The
// provider is invoked while the store lock is held and must not re-enter the
// store.
func (s *SessionStore) SetAttachedWatchersProvider(fn func(sessionID string) []config.WatcherConfig) {
	s.mu.Lock()
	s.attachedWatchersFn = fn
	s.mu.Unlock()
}

// persistState records, for one session, how much of each agent's transcript is
// already on disk so Save can append only the new message lines instead of
// rewriting every agent's full transcript each turn (issue #21). The transcript
// epoch, captured at the last save, lets Save detect an in-place transcript
// replacement (compaction): when an agent's epoch advances the previously
// persisted indices no longer line up, so the next save falls back to a full
// atomic rewrite of the shard set.
type persistState struct {
	title     string                 // session title as last written to the index
	persisted map[string]int         // agentID -> count of its messages already on disk
	epoch     map[string]uint64      // agentID -> transcript epoch observed at last save
	watchers  []config.WatcherConfig // attached watcher set as last written to the index
}

// LoadedSession is a session transcript read back from disk.
type LoadedSession struct {
	ID        string
	Title     string
	CreatedAt string
	// File is the path of the session's index file (the source of truth), used
	// by Adopt to re-attach a restored session.
	File string
	// Transcripts maps each agent id to the messages restored from the current
	// shard (the root agent is keyed "root"). AgentOrder preserves the agent
	// order seen on disk.
	Transcripts map[string][]model.Message
	AgentOrder  []string
	// Model is the config name of the model the session was last using, read back
	// from the index so a restored session resumes on that model (issue #266).
	// Empty for older sessions persisted before the model was recorded.
	Model string
	// ModelLabel/ModelID are the frozen display label + provider id captured at the
	// last save (issue #389), carried for any caller wanting the session's
	// historical model presentation. Empty for index files predating the fields.
	ModelLabel string
	ModelID    string
	// Watchers are the session's attached (session-scoped) watcher definitions read
	// back from the index, re-registered with the watcher manager via
	// OnSessionRestored (issue #329 Phase 3). nil when the session has none.
	Watchers []config.WatcherConfig
}

// NewSessionStore creates (and ensures the directory for) a session store.
func NewSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	return &SessionStore{
		dir:    dir,
		base:   make(map[string]string),
		state:  make(map[string]*persistState),
		shards: make(map[string][]shardMeta),
		cache:  newShardCache(shardCacheCapacity),
		dirty:  make(map[string]struct{}),
	}, nil
}

// Adopt re-attaches a session to its existing on-disk shards (used on restore so
// continued saves append to the active shard rather than starting over). It
// loads the index and the active shard, recovering the persisted frontier so the
// next save is a delta — older shards are left untouched on disk. agents is the
// live agent tree (typically just the restored root); Adopt captures each
// restored agent's transcript epoch as the compaction-detection baseline, so a
// compaction on the first restored turn is detected and rewritten rather than
// appended. file is the index path (LoadedSession.File from ListActive); a bare
// base prefix is also accepted. It is a no-op if the session has no index yet.
func (s *SessionStore) Adopt(sessionID, file string, agents []*agent.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	base := file
	if strings.HasSuffix(file, indexFileExt) {
		base = strings.TrimSuffix(file, indexFileExt)
	}
	s.base[sessionID] = base

	idx, err := loadIndexFile(base)
	if err != nil || idx.SessionID == "" {
		return // nothing persisted yet; a later save will build the shard set
	}
	s.shards[sessionID] = idx.Shards
	transcripts, _ := s.loadTranscripts(base, idx.Shards)

	st := &persistState{title: idx.Title, persisted: make(map[string]int), epoch: make(map[string]uint64)}
	for aid, msgs := range transcripts {
		st.persisted[aid] = len(msgs) // active-shard counts
	}
	// Capture the baseline epoch of each restored agent so Save can tell a later
	// compaction (epoch advance) from append-only growth and rewrite instead of
	// appending a stale delta.
	for _, a := range agents {
		if a.ThoughtTrain == nil {
			continue
		}
		if _, seen := st.persisted[a.ID]; seen {
			st.epoch[a.ID] = a.ThoughtTrain.TranscriptEpoch()
		}
	}
	s.state[sessionID] = st
}

// activeBase returns (assigning on first use) the base prefix for a session.
func (s *SessionStore) activeBase(sessionID string, createdAt int64) string {
	if b, ok := s.base[sessionID]; ok {
		return b
	}
	ts := time.Unix(createdAt, 0).UTC().Format("2006-01-02T15-04-05")
	b := filepath.Join(s.dir, ts+"_"+sanitizeID(sessionID)+activeBaseSuffix)
	s.base[sessionID] = b
	return b
}

// sanitizeID makes a session id safe to embed in a filename.
func sanitizeID(id string) string {
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "_")
	return repl.Replace(id)
}

// Save persists the session transcript. After the first save (and after any
// compaction) it appends only the new message lines added since the previous
// save to the active shard, rather than rewriting every agent's full transcript
// each turn — the line-oriented format makes the delta an O(new messages) append
// (issue #21). When the active shard crosses the cap a fresh shard is rolled so
// no single file grows unboundedly (issue #26).
//
// A full atomic rewrite (rebuild the shard set from scratch) is reserved for two
// cases: the first save of a session, and any agent whose transcript was
// replaced in place by a compaction (its persisted indices no longer line up
// with the on-disk lines). A title change only rewrites the index (the title
// lives there, not in the shards), so renaming a session no longer rewrites its
// transcript. Any per-record marshal failure is aggregated and returned rather
// than silently dropped, so Save never reports success while a line went missing
// (issue #17).
//
// The whole operation runs under the store lock: a delta append and its frontier
// update must be atomic with respect to other saves, or two overlapping saves
// could both append the same messages.
func (s *SessionStore) Save(us *agent.UserSession, title string) error {
	if s == nil || us == nil || us.RootAgent == nil {
		return nil
	}

	// Snapshot the usage summary before taking the store lock (it takes the
	// session's own lock, not this one) so the index write carries fresh stats.
	snap := us.Snapshot()

	s.mu.Lock()
	defer s.mu.Unlock()

	base := s.activeBase(us.ID, us.CreatedAt)
	st := s.state[us.ID]
	agents := us.RootAgent.ListAllAgents()
	created := time.Unix(us.CreatedAt, 0).UTC().Format(time.RFC3339)
	// Capture the usage summary once so every index write (first save,
	// compaction rewrite, delta) carries the same fresh figures (issue #58).
	sum := sessionSummary{
		turns:     snap.Turns,
		tokensIn:  snap.TokensIn,
		tokensOut: snap.TokensOut,
		model:     us.PrimaryModel(),
	}
	// Freeze the primary model's display label + provider id as they are right now
	// (issue #389), so the Saved Sessions dialog renders the model this session
	// actually ran on rather than a key that becomes opaque after a later edit. If
	// the config can't be resolved, leave both empty — the dialog falls back to the
	// bare Name. We hold s.mu here, so the provider must not re-enter the store.
	if s.modelSnapshotFn != nil && sum.model != "" {
		if label, id, ok := s.modelSnapshotFn(sum.model); ok {
			sum.modelLabel = label
			sum.modelID = id
		}
	}
	// Capture the session's attached watcher configs once so every index write
	// (first save, compaction rewrite, delta) carries the same set (issue #329).
	var watchers []config.WatcherConfig
	if s.attachedWatchersFn != nil {
		watchers = s.attachedWatchersFn(us.ID)
	}

	// First save: build the whole shard set.
	if st == nil {
		sms, err := s.writeFullTranscript(base, us.ID, title, created, sum, watchers, agents)
		if err != nil {
			return err
		}
		s.shards[us.ID] = sms
		st := newPersistFrontier(title, agents)
		st.watchers = watchers
		s.state[us.ID] = st
		s.markDirty(append(shardFilePaths(base, sms), indexFilePath(base))...)
		return nil
	}

	// A known agent whose transcript was compacted in place forces a full rewrite.
	for _, a := range agents {
		if a.ThoughtTrain == nil {
			continue
		}
		if prev, ok := st.epoch[a.ID]; ok && prev != a.ThoughtTrain.TranscriptEpoch() {
			sms, err := s.writeFullTranscript(base, us.ID, title, created, sum, watchers, agents)
			if err != nil {
				return err
			}
			s.shards[us.ID] = sms
			st.title = title
			st.watchers = watchers
			recordFrontier(st, agents)
			s.markDirty(append(shardFilePaths(base, sms), indexFilePath(base))...)
			return nil
		}
	}

	// Delta path: encode only the messages added since the last save.
	var buf bytes.Buffer
	if _, err := encodeMessages(json.NewEncoder(&buf), agents, func(aid string) int { return st.persisted[aid] }); err != nil {
		// Drop the frontier so the next save rebuilds the shard set atomically
		// instead of appending on top of an unknown state.
		delete(s.state, us.ID)
		delete(s.shards, us.ID)
		return err
	}
	lines := splitJSONLLines(buf.Bytes())
	titleChanged := st.title != title
	// An attached watcher created/removed/updated since the last save changes only
	// the index (it lives there, not in the shards), so a watcher-set change must
	// still rewrite the index even with no new transcript lines (issue #329).
	watchersChanged := !watchersEqual(st.watchers, watchers)

	if len(lines) == 0 && !titleChanged && !watchersChanged {
		// True no-op: nothing to write. Still refresh the captured epochs so a
		// later compaction is detected.
		recordFrontier(st, agents)
		return nil
	}

	sms := s.shards[us.ID]
	var written []string
	if len(lines) > 0 {
		var err error
		sms, written, err = writeLinesToShards(base, sms, lines)
		if err != nil {
			delete(s.state, us.ID)
			delete(s.shards, us.ID)
			return err
		}
		s.shards[us.ID] = sms
		s.cache.evictPrefix(base) // the active shard(s) changed
	}
	if err := writeIndexFile(base, indexFrom(us.ID, title, created, sum, watchers, sms)); err != nil {
		delete(s.state, us.ID)
		delete(s.shards, us.ID)
		return err
	}
	st.title = title
	st.watchers = watchers
	recordFrontier(st, agents)
	s.markDirty(append(written, indexFilePath(base))...)
	return nil
}

// watchersEqual reports whether two attached-watcher sets are identical for the
// purpose of deciding whether the index needs rewriting. It compares by value so
// a created/removed/edited watcher is detected; order is treated as significant
// (the manager preserves insertion order via gogent's per-session slice).
func watchersEqual(a, b []config.WatcherConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// writeFullTranscript rebuilds the whole shard set atomically: it encodes every
// agent's full transcript, splits it into cap-sized shards (each created via a
// temp file + rename so a crash can't leave a half-written shard behind), drops
// any leftover shards from a previously larger layout, then rewrites the index
// last so the on-disk state stays consistent.
func (s *SessionStore) writeFullTranscript(base, id, title, created string, sum sessionSummary, watchers []config.WatcherConfig, agents []*agent.Agent) ([]shardMeta, error) {
	var buf bytes.Buffer
	if _, err := encodeMessages(json.NewEncoder(&buf), agents, func(string) int { return 0 }); err != nil {
		return nil, err
	}
	lines := splitJSONLLines(buf.Bytes())

	sms, _, err := writeLinesToShards(base, nil, lines)
	if err != nil {
		return nil, err
	}
	// Remove orphaned shards left over from a previously larger shard set.
	old := s.shards[id]
	for i := len(sms); i < len(old); i++ {
		_ = os.Remove(shardFilePath(base, i))
	}
	s.cache.evictPrefix(base)
	if err := writeIndexFile(base, indexFrom(id, title, created, sum, watchers, sms)); err != nil {
		return nil, err
	}
	return sms, nil
}

// indexFrom assembles the index document from its parts. Centralising it keeps
// the summary fields (issue #58) populated consistently across every write site.
func indexFrom(id, title, created string, sum sessionSummary, watchers []config.WatcherConfig, sms []shardMeta) sessionIndex {
	return sessionIndex{
		SessionID:  id,
		Title:      title,
		CreatedAt:  created,
		Turns:      sum.turns,
		TokensIn:   sum.tokensIn,
		TokensOut:  sum.tokensOut,
		Model:      sum.model,
		ModelLabel: sum.modelLabel,
		ModelID:    sum.modelID,
		Shards:     sms,
		Watchers:   watchers,
	}
}

// writeLinesToShards appends a batch of pre-encoded JSONL lines to the active
// shard, rolling to a fresh shard whenever the active one is at the event cap or
// would cross the byte cap. New shards are created via temp file + rename (an
// atomic replace); appends to an existing shard are plain appends, like a delta.
// It returns the updated shard table and the paths actually written (for the
// durability flush).
func writeLinesToShards(base string, sms []shardMeta, lines [][]byte) ([]shardMeta, []string, error) {
	var written []string
	for _, line := range lines {
		recBytes := int64(len(line) + 1) // line + newline
		last := len(sms) - 1
		roll := last < 0 || sms[last].Events >= shardMaxEvents || sms[last].Bytes+recBytes > shardMaxBytes
		if roll {
			idx := len(sms)
			path := shardFilePath(base, idx)
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, nil, 0o600); err != nil {
				return sms, written, fmt.Errorf("create shard file: %w", err)
			}
			if err := os.Rename(tmp, path); err != nil {
				return sms, written, fmt.Errorf("rename shard file: %w", err)
			}
			sms = append(sms, shardMeta{Index: idx})
			last = idx
		}
		path := shardFilePath(base, sms[last].Index)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is a session shard file built from the store dir, not user input
		if err != nil {
			return sms, written, fmt.Errorf("open shard file: %w", err)
		}
		n, err := f.Write(append(line, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return sms, written, err
		}
		sms[last].Events++
		sms[last].Bytes += int64(n)
		written = append(written, path)
	}
	return sms, written, nil
}

// encodeMessages writes each new message (transcript[from(aid):] per agent) as a
// JSONL "message" record to enc. It joins (does not swallow) any encode error so
// Save never reports success while a transcript line went missing (issue #17),
// and returns the count of records written.
func encodeMessages(enc *json.Encoder, agents []*agent.Agent, from func(aid string) int) (int, error) {
	n := 0
	var errs error
	for _, a := range agents {
		if a.ThoughtTrain == nil {
			continue
		}
		off := from(a.ID)
		tr := a.ThoughtTrain.GetTranscript()
		if off >= len(tr) {
			continue
		}
		for _, m := range tr[off:] {
			m := m
			if err := enc.Encode(jsonlRecord{Kind: "message", AgentID: a.ID, Message: &m}); err != nil {
				errs = errors.Join(errs, fmt.Errorf("encode message for agent %s: %w", a.ID, err))
				continue
			}
			n++
		}
	}
	return n, errs
}

// splitJSONLLines splits a buffer of newline-terminated JSON records into
// individual line byte slices (copying each, so the buffer can be reclaimed).
func splitJSONLLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' {
			if i > start {
				lines = append(lines, append([]byte(nil), b[start:i]...))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, append([]byte(nil), b[start:]...))
	}
	return lines
}

// recordFrontier snapshots how much of each agent's transcript is now on disk
// and the transcript epoch observed at this save, so the next save can compute a
// delta and detect a later compaction. Agents whose epoch was previously unknown
// (e.g. a freshly restored or newly spawned agent) get their baseline captured
// here without forcing a rewrite.
func recordFrontier(st *persistState, agents []*agent.Agent) {
	for _, a := range agents {
		if a.ThoughtTrain == nil {
			continue
		}
		if st.persisted == nil {
			st.persisted = make(map[string]int)
		}
		if st.epoch == nil {
			st.epoch = make(map[string]uint64)
		}
		st.persisted[a.ID] = a.ThoughtTrain.TranscriptLen()
		st.epoch[a.ID] = a.ThoughtTrain.TranscriptEpoch()
	}
}

func newPersistFrontier(title string, agents []*agent.Agent) *persistState {
	st := &persistState{title: title, persisted: make(map[string]int), epoch: make(map[string]uint64)}
	recordFrontier(st, agents)
	return st
}

// Archive renames a session's base from "_session" to "_session_archived" across
// all of its files (index + shards) so it is not restored on the next startup.
// It is a no-op if the session has no index yet.
func (s *SessionStore) Archive(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	base, ok := s.base[sessionID]
	if !ok {
		return nil
	}
	if _, err := os.Stat(indexFilePath(base)); err != nil {
		return nil // nothing written yet
	}
	archivedBase := base + archivedTag

	// Best-effort rename of the index and every shard. A missing source (e.g. a
	// shard that was never created) is silently skipped.
	renameFile := func(from, to string) error {
		err := os.Rename(from, to)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("rename session file: %w", err)
	}
	if err := renameFile(indexFilePath(base), indexFilePath(archivedBase)); err != nil {
		return err
	}
	for i := 0; i < len(s.shards[sessionID]); i++ {
		if err := renameFile(shardFilePath(base, i), shardFilePath(archivedBase, i)); err != nil {
			return err
		}
	}

	delete(s.base, sessionID)
	delete(s.state, sessionID)
	delete(s.shards, sessionID)
	s.cache.evictPrefix(base)
	return nil
}

// ListActive reads every live index file and returns the restored sessions
// (oldest first), populating each transcript from its current shard only. It
// reads just the small index plus the active shard per session, never replaying
// the whole history, so it stays O(active shards) rather than O(total events).
func (s *SessionStore) ListActive() ([]LoadedSession, error) {
	bases, err := s.activeBases()
	if err != nil {
		return nil, err
	}
	var sessions []LoadedSession
	for _, base := range bases {
		idx, err := loadIndexFile(base)
		if err != nil || idx.SessionID == "" {
			continue
		}
		transcripts, order := s.loadTranscripts(base, idx.Shards)
		sessions = append(sessions, LoadedSession{
			ID:          idx.SessionID,
			Title:       idx.Title,
			CreatedAt:   idx.CreatedAt,
			File:        indexFilePath(base),
			Transcripts: transcripts,
			AgentOrder:  order,
			Model:       idx.Model,
			ModelLabel:  idx.ModelLabel,
			ModelID:     idx.ModelID,
			Watchers:    idx.Watchers,
		})
	}
	return sessions, nil
}

// activeBases lists the base prefixes of every live (non-archived) session,
// oldest first. The ISO timestamp prefixing each base makes the lexical sort
// chronological. Shared by ListActive (which also loads transcripts) and
// ListSessions (which does not).
func (s *SessionStore) activeBases() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}
	var bases []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, indexFileExt) {
			continue
		}
		baseName := strings.TrimSuffix(name, indexFileExt)
		if !strings.HasSuffix(baseName, activeBaseSuffix) { // skip "_session_archived"
			continue
		}
		bases = append(bases, filepath.Join(s.dir, baseName))
	}
	sort.Strings(bases)
	return bases, nil
}

// allBases lists the base prefixes of every session on disk — active (_session)
// AND archived (_session_archived) — oldest first. It is the counterpart to
// activeBases used by ListAllSessions so the Saved Sessions browser can show
// closed (archived) sessions alongside open ones (issue #325). The ISO timestamp
// prefixing each base makes the lexical sort chronological.
func (s *SessionStore) allBases() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}
	var bases []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, indexFileExt) {
			continue
		}
		baseName := strings.TrimSuffix(name, indexFileExt)
		// Keep both "_session" and "_session_archived" bases (the only difference
		// from activeBases, which skips the archived ones).
		bases = append(bases, filepath.Join(s.dir, baseName))
	}
	sort.Strings(bases)
	return bases, nil
}

// SessionMeta is the lightweight, index-only view of one persisted session used
// by the Sessions browser (issue #58): enough metadata to list, search and pick
// a session without replaying its transcript. Messages is the total message
// record count (the sum of every shard's events); File is the index path that
// LoadSession re-opens.
type SessionMeta struct {
	ID        string
	Title     string
	CreatedAt string
	Turns     int
	Messages  int
	TokensIn  int
	TokensOut int
	Model     string // stable config Name (lookup key)
	// ModelLabel/ModelID are the frozen display label + provider id snapshotted at
	// save time (issue #389), shown by the Saved Sessions dialog. Empty for index
	// files predating the fields — the dialog falls back to the bare Model key.
	ModelLabel string
	ModelID    string
	File       string
	// Archived is true when this session's on-disk base is "_session_archived"
	// (its window was closed). Only ListAllSessions sets it; the active-only
	// ListSessions always leaves it false. The browser uses it to mark closed
	// sessions (issue #325).
	Archived bool
}

// ListSessions returns the metadata of every live session straight from the
// index files (oldest first) — the O(sessions) listing the Sessions browser
// needs (issue #58). It never reads a shard, so listing stays cheap regardless
// of transcript size; total message counts come from the index's shard table.
func (s *SessionStore) ListSessions() ([]SessionMeta, error) {
	bases, err := s.activeBases()
	if err != nil {
		return nil, err
	}
	out := make([]SessionMeta, 0, len(bases))
	for _, base := range bases {
		idx, err := loadIndexFile(base)
		if err != nil || idx.SessionID == "" {
			continue
		}
		messages := 0
		for _, sm := range idx.Shards {
			messages += sm.Events
		}
		out = append(out, SessionMeta{
			ID:         idx.SessionID,
			Title:      idx.Title,
			CreatedAt:  idx.CreatedAt,
			Turns:      idx.Turns,
			Messages:   messages,
			TokensIn:   idx.TokensIn,
			TokensOut:  idx.TokensOut,
			Model:      idx.Model,
			ModelLabel: idx.ModelLabel,
			ModelID:    idx.ModelID,
			File:       indexFilePath(base),
		})
	}
	return out, nil
}

// ListAllSessions returns the metadata of every persisted session on disk —
// active (open) AND archived (closed) — straight from the index files (oldest
// first), tagging each with Archived so the Saved Sessions browser can show and
// mark closed sessions (issue #325). It mirrors ListSessions but iterates
// allBases() instead of activeBases(); the index read is identical for either
// base (an archived session's .index is the same JSON at a "_archived" path). It
// never reads a shard, so listing stays O(sessions) regardless of transcript
// size. The active-only ListSessions is deliberately left unchanged so startup
// restore keeps excluding archived sessions.
func (s *SessionStore) ListAllSessions() ([]SessionMeta, error) {
	bases, err := s.allBases()
	if err != nil {
		return nil, err
	}
	out := make([]SessionMeta, 0, len(bases))
	for _, base := range bases {
		idx, err := loadIndexFile(base)
		if err != nil || idx.SessionID == "" {
			continue
		}
		messages := 0
		for _, sm := range idx.Shards {
			messages += sm.Events
		}
		out = append(out, SessionMeta{
			ID:         idx.SessionID,
			Title:      idx.Title,
			CreatedAt:  idx.CreatedAt,
			Turns:      idx.Turns,
			Messages:   messages,
			TokensIn:   idx.TokensIn,
			TokensOut:  idx.TokensOut,
			Model:      idx.Model,
			ModelLabel: idx.ModelLabel,
			ModelID:    idx.ModelID,
			File:       indexFilePath(base),
			Archived:   strings.HasSuffix(base, archivedTag),
		})
	}
	return out, nil
}

// UnarchiveBase reverses Archive for a single session addressed by its index file
// path (or bare base): it renames the base back from "_session_archived" to
// "_session" across the index and every shard, so subsequent saves append to the
// active base and the session re-enters the active/restore set. It is the
// counterpart the codebase lacked, used when a user CONTINUES a closed (archived)
// session from the browser (issue #325); the read-only Open path must NOT call it.
// A base that is already active is returned unchanged (no-op). It returns the
// index file path of the now-active base for the caller to load from.
func (s *SessionStore) UnarchiveBase(file string) (string, error) {
	base := strings.TrimSuffix(file, indexFileExt)
	if !strings.HasSuffix(base, archivedTag) {
		return indexFilePath(base), nil // already active: nothing to do
	}
	activeBase := strings.TrimSuffix(base, archivedTag)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Refuse to clobber a live active base. If an active index already exists for
	// this prefix (a same-second/same-id active session coexisting with the
	// archived one), os.Rename would silently overwrite it and destroy the open
	// session's data, so bail out and let the caller load the active base as-is.
	if _, err := os.Stat(indexFilePath(activeBase)); err == nil {
		return indexFilePath(activeBase), fmt.Errorf("unarchive %s: an active session already exists at %s", base, activeBase)
	}

	// The shard table lives in the (archived) index, so read it to learn which
	// shard files to rename alongside the index.
	idx, err := loadIndexFile(base)
	if err != nil {
		return indexFilePath(base), err
	}

	// Best-effort rename of the index and every shard. A missing source (e.g. a
	// shard that was never created) is silently skipped, mirroring Archive.
	renameFile := func(from, to string) error {
		err := os.Rename(from, to)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("rename session file: %w", err)
	}
	if err := renameFile(indexFilePath(base), indexFilePath(activeBase)); err != nil {
		return indexFilePath(base), err
	}
	for _, sm := range idx.Shards {
		if err := renameFile(shardFilePath(base, sm.Index), shardFilePath(activeBase, sm.Index)); err != nil {
			return indexFilePath(activeBase), err
		}
	}
	s.cache.evictPrefix(base)
	return indexFilePath(activeBase), nil
}

// LoadSession reads one session's transcript (current shard only) on demand,
// addressed by its index file path (SessionMeta.File) or the bare base prefix.
// It is the on-demand counterpart of ListActive's per-session load, used to open
// a single saved session into a window for analysis or continuation (issue #58).
func (s *SessionStore) LoadSession(file string) (LoadedSession, error) {
	base := file
	if strings.HasSuffix(file, indexFileExt) {
		base = strings.TrimSuffix(file, indexFileExt)
	}
	idx, err := loadIndexFile(base)
	if err != nil {
		return LoadedSession{}, err
	}
	transcripts, order := s.loadTranscripts(base, idx.Shards)
	return LoadedSession{
		ID:          idx.SessionID,
		Title:       idx.Title,
		CreatedAt:   idx.CreatedAt,
		File:        indexFilePath(base),
		Transcripts: transcripts,
		AgentOrder:  order,
		Model:       idx.Model,
		ModelLabel:  idx.ModelLabel,
		ModelID:     idx.ModelID,
		Watchers:    idx.Watchers,
	}, nil
}

// loadTranscripts reads the current (latest) shard and returns its per-agent
// transcripts and the agent order seen on disk. The current-shard-only restore
// (issue #26) bounds restore cost for long sessions; older shards stay on disk.
func (s *SessionStore) loadTranscripts(base string, sms []shardMeta) (map[string][]model.Message, []string) {
	out := make(map[string][]model.Message)
	if len(sms) == 0 {
		return out, nil
	}
	active := sms[len(sms)-1]
	records, err := s.loadShard(shardFilePath(base, active.Index))
	if err != nil {
		return out, nil
	}
	var order []string
	for _, r := range records {
		if _, seen := out[r.agentID]; !seen {
			order = append(order, r.agentID)
		}
		out[r.agentID] = append(out[r.agentID], r.msg)
	}
	return out, order
}

// loadShard parses one shard file into ordered records, served from the LRU
// cache when the shard is hot (e.g. the ListActive -> Adopt sequence on restore).
func (s *SessionStore) loadShard(path string) ([]shardRecord, error) {
	if records, ok := s.cache.get(path); ok {
		return records, nil
	}
	f, err := os.Open(path) //nolint:gosec // path is a caller-controlled session shard file under the store dir
	if err != nil {
		return nil, fmt.Errorf("open shard: %w", err)
	}
	defer func() { _ = f.Close() }()

	var records []shardRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec jsonlRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Kind != "message" || rec.Message == nil {
			continue // meta/legacy records are not part of a shard's transcript
		}
		aid := rec.AgentID
		if aid == "" {
			aid = "root"
		}
		records = append(records, shardRecord{agentID: aid, msg: *rec.Message})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan shard: %w", err)
	}
	s.cache.put(path, records)
	return records, nil
}

// shardRecord is one parsed message keyed by its owning agent.
type shardRecord struct {
	agentID string
	msg     model.Message
}

// Sync flushes any pending durability writes (fsync) for all dirty session files
// synchronously. Call on graceful shutdown, or from tests that need the data on
// disk to survive a simulated crash.
func (s *SessionStore) Sync() {
	s.flushDirty()
}

// Close stops the background flusher and performs one final synchronous flush.
func (s *SessionStore) Close() error {
	s.flushMu.Lock()
	s.stopped = true
	t := s.timer
	s.timer = nil
	paths := s.takeDirtyLocked()
	s.flushMu.Unlock()
	if t != nil {
		t.Stop()
	}
	flushPaths(paths)
	fsyncDir(s.dir)
	return nil
}

// markDirty records paths whose data was written this turn and schedules a batched
// fsync shortly (debounced: many saves within flushInterval coalesce into one).
func (s *SessionStore) markDirty(paths ...string) {
	s.flushMu.Lock()
	for _, p := range paths {
		if p != "" {
			s.dirty[p] = struct{}{}
		}
	}
	if !s.stopped && s.timer == nil && len(s.dirty) > 0 {
		s.timer = time.AfterFunc(flushInterval, s.flushDirty)
	}
	s.flushMu.Unlock()
}

// flushDirty fsyncs the current dirty set plus the sessions directory. Any paths
// dirtied while the flush is in flight land in a fresh dirty set and are picked
// up by the next markDirty-scheduled flush, so no fsync is lost.
func (s *SessionStore) flushDirty() {
	s.flushMu.Lock()
	if s.stopped {
		s.flushMu.Unlock()
		return
	}
	s.timer = nil
	paths := s.takeDirtyLocked()
	s.flushMu.Unlock()
	flushPaths(paths)
	fsyncDir(s.dir)
}

// takeDirtyLocked swaps out and returns the current dirty set. Caller holds flushMu.
func (s *SessionStore) takeDirtyLocked() []string {
	if len(s.dirty) == 0 {
		return nil
	}
	paths := make([]string, 0, len(s.dirty))
	for p := range s.dirty {
		paths = append(paths, p)
	}
	s.dirty = make(map[string]struct{})
	return paths
}

// flushPaths fsyncs each path (best-effort: missing files are skipped).
func flushPaths(paths []string) {
	for _, p := range paths {
		fsyncFile(p)
	}
}

func fsyncFile(path string) {
	f, err := os.Open(path) //nolint:gosec // path is a caller-controlled session file under the store dir
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

func fsyncDir(dir string) {
	f, err := os.Open(dir) //nolint:gosec // dir is a caller-controlled session store directory
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

// --- path helpers ---

func indexFilePath(base string) string { return base + indexFileExt }

func shardFilePath(base string, idx int) string {
	return fmt.Sprintf("%s.%0*d%s", base, shardNumberWidth, idx, shardFileExt)
}

func shardFilePaths(base string, sms []shardMeta) []string {
	out := make([]string, 0, len(sms))
	for _, sm := range sms {
		out = append(out, shardFilePath(base, sm.Index))
	}
	return out
}

// writeIndexFile atomically writes the index (temp file + rename).
func writeIndexFile(base string, idx sessionIndex) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	path := indexFilePath(base)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write index file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename index file: %w", err)
	}
	return nil
}

// loadIndexFile reads and decodes a session index.
func loadIndexFile(base string) (sessionIndex, error) {
	data, err := os.ReadFile(indexFilePath(base))
	if err != nil {
		return sessionIndex{}, fmt.Errorf("read index file: %w", err)
	}
	var idx sessionIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return sessionIndex{}, fmt.Errorf("unmarshal index: %w", err)
	}
	return idx, nil
}

// --- shard LRU cache ---

// shardCache is a small LRU of parsed shard files, keeping hot turns in memory so
// repeated reads (e.g. list then restore) do not re-parse the same shard.
type shardCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	key     string
	records []shardRecord
}

func newShardCache(cap int) *shardCache {
	return &shardCache{cap: cap, ll: list.New(), items: make(map[string]*list.Element)}
}

func (c *shardCache) get(key string) ([]shardRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*cacheEntry).records, true
	}
	return nil, false
}

func (c *shardCache) put(key string, records []shardRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = &cacheEntry{key: key, records: records}
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&cacheEntry{key: key, records: records})
	for c.ll.Len() > c.cap {
		if back := c.ll.Back(); back != nil {
			e := c.ll.Remove(back).(*cacheEntry)
			delete(c.items, e.key)
		}
	}
}

// evictPrefix drops every cached shard whose path begins with prefix (used when a
// session's shards are rewritten, archived, or otherwise invalidated).
func (c *shardCache) evictPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var toRemove []*list.Element
	for el := c.ll.Front(); el != nil; el = el.Next() {
		if strings.HasPrefix(el.Value.(*cacheEntry).key, prefix) {
			toRemove = append(toRemove, el)
		}
	}
	for _, el := range toRemove {
		e := c.ll.Remove(el).(*cacheEntry)
		delete(c.items, e.key)
	}
}
