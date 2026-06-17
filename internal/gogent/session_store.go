package gogent

import (
	"bufio"
	"encoding/json"
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
	files map[string]string // sessionID -> active file path
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
	return &SessionStore{dir: dir, files: make(map[string]string)}, nil
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

// Save (re)writes the full transcript of every agent in the session. It writes
// to a temp file and renames it into place so a crash can't leave a half-written
// file behind.
func (s *SessionStore) Save(us *agent.UserSession, title string) error {
	if s == nil || us == nil || us.RootAgent == nil {
		return nil
	}
	s.mu.Lock()
	path := s.activePath(us.ID, us.CreatedAt)
	s.mu.Unlock()

	created := time.Unix(us.CreatedAt, 0).UTC().Format(time.RFC3339)
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	_ = enc.Encode(jsonlRecord{Kind: "meta", SessionID: us.ID, Title: title, CreatedAt: created})
	for _, a := range us.RootAgent.ListAllAgents() {
		if a.ThoughtTrain == nil {
			continue
		}
		for _, m := range a.ThoughtTrain.GetTranscript() {
			m := m
			_ = enc.Encode(jsonlRecord{Kind: "message", AgentID: a.ID, Message: &m})
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
