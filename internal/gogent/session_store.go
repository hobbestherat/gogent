package gogent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gogent/internal/agent"
	"gogent/internal/model"
)

// Session transcripts are persisted as JSON-lines files in the sessions
// directory. A live session is "<iso>_<id>_session.jsonl"; when its window is
// closed it is renamed to "<iso>_<id>_session_archived.jsonl". On startup any
// remaining (non-archived) files are restored for continuation — this is how a
// crash leaves a recoverable transcript behind. A user can re-load an archived
// session simply by renaming the file back to the "_session.jsonl" suffix.
const (
	sessionFileSuffix  = "_session.jsonl"
	archivedFileSuffix = "_session_archived.jsonl"
)

// jsonlRecord is one line of a session file: either a "meta" header or a
// per-agent "message".
type jsonlRecord struct {
	Kind      string         `json:"kind"`
	SessionID string         `json:"session_id,omitempty"`
	Title     string         `json:"title,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	Message   *model.Message `json:"message,omitempty"`
}

// SessionStore manages the JSONL session files on disk.
type SessionStore struct {
	dir   string
	mu    sync.Mutex
	files map[string]string        // sessionID -> active file path
	state map[string]*persistState // sessionID -> per-agent persisted frontier
}

// persistState records, for one session, how much of each agent's transcript is
// already on disk so Save can append only the new message lines instead of
// rewriting every agent's full transcript each turn (issue #21). The transcript
// epoch, captured at the last save, lets Save detect an in-place transcript
// replacement (compaction): when an agent's epoch advances the previously
// persisted indices no longer line up, so the next save falls back to a full
// atomic rewrite of the file.
type persistState struct {
	title     string            // session title as last written to the meta line
	persisted map[string]int    // agentID -> count of its messages already on disk
	epoch     map[string]uint64 // agentID -> transcript epoch observed at last save
}

// LoadedSession is a session transcript read back from disk.
type LoadedSession struct {
	ID        string
	Title     string
	CreatedAt string
	File      string
	// Transcripts maps each agent id to its restored message list (the root
	// agent is keyed "root"). order preserves the agent order seen on disk.
	Transcripts map[string][]model.Message
	AgentOrder  []string
}

// NewSessionStore creates (and ensures the directory for) a session store.
func NewSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}
	return &SessionStore{
		dir:   dir,
		files: make(map[string]string),
		state: make(map[string]*persistState),
	}, nil
}

// Adopt records that a session is backed by an existing file (used on restore so
// continued saves append to the same file rather than starting a new one).
func (s *SessionStore) Adopt(sessionID, file string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[sessionID] = file
}

// activePath returns (assigning on first use) the live file path for a session.
func (s *SessionStore) activePath(sessionID string, createdAt int64) string {
	if p, ok := s.files[sessionID]; ok {
		return p
	}
	ts := time.Unix(createdAt, 0).UTC().Format("2006-01-02T15-04-05")
	name := fmt.Sprintf("%s_%s%s", ts, sanitizeID(sessionID), sessionFileSuffix)
	p := filepath.Join(s.dir, name)
	s.files[sessionID] = p
	return p
}

// sanitizeID makes a session id safe to embed in a filename.
func sanitizeID(id string) string {
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "_")
	return repl.Replace(id)
}

// Save persists the session transcript. After the first save (and after any
// compaction) it appends only the new message lines added since the previous
// save, rather than rewriting every agent's full transcript each turn — the
// line-oriented format makes the delta an O(new messages) append (issue #21).
//
// A full atomic rewrite (temp file + rename, which a crash can't leave
// half-written behind) is reserved for three cases: the first save of a
// session, a change to the session title (held in the meta header), and any
// agent whose transcript was replaced in place by a compaction (its persisted
// indices no longer line up with the on-disk lines). Any per-record marshal
// failure is aggregated and returned rather than silently dropped, so Save
// never reports success while a transcript line went missing (issue #17).
//
// The whole operation runs under the store lock: a delta append and its
// frontier update must be atomic with respect to other saves, or two
// overlapping saves could both append the same messages.
func (s *SessionStore) Save(us *agent.UserSession, title string) error {
	if s == nil || us == nil || us.RootAgent == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.activePath(us.ID, us.CreatedAt)
	st := s.state[us.ID]
	agents := us.RootAgent.ListAllAgents()
	created := time.Unix(us.CreatedAt, 0).UTC().Format(time.RFC3339)

	// A full rewrite is needed when there is no recorded frontier yet, when the
	// title (held in the meta header) changed, or when any known agent's
	// transcript was replaced in place since the last save. A brand-new agent
	// (unknown epoch) does not force a rewrite — its messages simply append.
	fullRewrite := st == nil || st.title != title
	if !fullRewrite {
		for _, a := range agents {
			if a.ThoughtTrain == nil {
				continue
			}
			if prev, ok := st.epoch[a.ID]; ok && prev != a.ThoughtTrain.TranscriptEpoch() {
				fullRewrite = true
				break
			}
		}
	}

	var err error
	if fullRewrite {
		err = saveFull(us, path, title, created)
	} else {
		err = saveDelta(agents, st, path)
	}
	if err != nil {
		// A failed delta append may have left a partial tail on disk; drop the
		// recorded frontier so the next save rebuilds the file atomically
		// (overwriting any corrupt tail via temp + rename) instead of appending
		// on top of it.
		if !fullRewrite {
			delete(s.state, us.ID)
		}
		return err
	}

	// Record the new persisted frontier so the next save resumes from here.
	if st == nil {
		st = &persistState{
			persisted: make(map[string]int),
			epoch:     make(map[string]uint64),
		}
		s.state[us.ID] = st
	}
	st.title = title
	for _, a := range agents {
		if a.ThoughtTrain == nil {
			continue
		}
		st.persisted[a.ID] = a.ThoughtTrain.TranscriptLen()
		st.epoch[a.ID] = a.ThoughtTrain.TranscriptEpoch()
	}
	return nil
}

// saveFull rebuilds the whole file atomically: it encodes the meta header plus
// every agent's full transcript to a buffer, then writes a temp file and renames
// it into place so a crash can't leave a half-written file behind.
func saveFull(us *agent.UserSession, path, title, created string) error {
	var buf strings.Builder
	if err := encodeTranscript(json.NewEncoder(&buf), us, title, created); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveDelta appends only the message records added since the previous save. The
// whole delta is encoded into memory first so a marshal failure can't leave a
// half-written line on disk; only then is it appended in a single write. st is
// read for the per-agent "already on disk" offset and is left untouched — Save
// commits the new frontier once this returns without error.
func saveDelta(agents []*agent.Agent, st *persistState, path string) error {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	var errs error
	for _, a := range agents {
		if a.ThoughtTrain == nil {
			continue
		}
		// Skip agents with nothing new without copying their transcript — the
		// common case where only one agent grew this turn.
		from := st.persisted[a.ID]
		if from >= a.ThoughtTrain.TranscriptLen() {
			continue
		}
		for _, m := range a.ThoughtTrain.GetTranscript()[from:] {
			m := m
			if err := enc.Encode(jsonlRecord{Kind: "message", AgentID: a.ID, Message: &m}); err != nil {
				errs = errors.Join(errs, fmt.Errorf("encode message for agent %s: %w", a.ID, err))
			}
		}
	}
	if errs != nil {
		return errs
	}
	if buf.Len() == 0 {
		return nil // every agent already up to date
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(buf.String())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// encodeTranscript writes the session meta header followed by every agent's
// transcript as JSONL records into enc. It joins any per-record marshal error
// (instead of swallowing it) so a single unencodable message can't silently
// vanish from the persisted transcript.
func encodeTranscript(enc *json.Encoder, us *agent.UserSession, title, created string) error {
	var errs error
	if err := enc.Encode(jsonlRecord{Kind: "meta", SessionID: us.ID, Title: title, CreatedAt: created}); err != nil {
		errs = errors.Join(errs, fmt.Errorf("encode session meta: %w", err))
	}
	for _, a := range us.RootAgent.ListAllAgents() {
		if a.ThoughtTrain == nil {
			continue
		}
		for _, m := range a.ThoughtTrain.GetTranscript() {
			m := m
			if err := enc.Encode(jsonlRecord{Kind: "message", AgentID: a.ID, Message: &m}); err != nil {
				errs = errors.Join(errs, fmt.Errorf("encode message for agent %s: %w", a.ID, err))
			}
		}
	}
	return errs
}

// Archive renames a session's live file to the archived suffix so it is not
// restored on the next startup. It is a no-op if the session has no file.
func (s *SessionStore) Archive(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.files[sessionID]
	if !ok {
		return nil
	}
	delete(s.files, sessionID)
	delete(s.state, sessionID)
	archived := strings.TrimSuffix(path, sessionFileSuffix) + archivedFileSuffix
	if _, err := os.Stat(path); err != nil {
		return nil // nothing written yet
	}
	return os.Rename(path, archived)
}

// ListActive reads every live ("_session.jsonl", not archived) file and returns
// the restored sessions, oldest first.
func (s *SessionStore) ListActive() ([]LoadedSession, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, sessionFileSuffix) || strings.HasSuffix(name, archivedFileSuffix) {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files) // ISO-prefixed names sort chronologically

	var sessions []LoadedSession
	for _, name := range files {
		full := filepath.Join(s.dir, name)
		ls, err := loadSessionFile(full)
		if err != nil || ls.ID == "" {
			continue
		}
		sessions = append(sessions, ls)
	}
	return sessions, nil
}

// loadSessionFile parses a single JSONL session file.
func loadSessionFile(path string) (LoadedSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return LoadedSession{}, err
	}
	defer func() { _ = f.Close() }()

	ls := LoadedSession{File: path, Transcripts: make(map[string][]model.Message)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec jsonlRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		switch rec.Kind {
		case "meta":
			ls.ID = rec.SessionID
			ls.Title = rec.Title
			ls.CreatedAt = rec.CreatedAt
		case "message":
			if rec.Message == nil {
				continue
			}
			aid := rec.AgentID
			if aid == "" {
				aid = "root"
			}
			if _, seen := ls.Transcripts[aid]; !seen {
				ls.AgentOrder = append(ls.AgentOrder, aid)
			}
			ls.Transcripts[aid] = append(ls.Transcripts[aid], *rec.Message)
		}
	}
	return ls, sc.Err()
}
