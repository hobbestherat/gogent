package fileops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// maxCheckpoints bounds how many turns of undo history a session keeps in memory,
// so a long-running session cannot grow the checkpoint store without limit. It is
// generous enough for repeated undo use; the oldest turns fall off the back
// (FIFO) and are no longer rewindable.
const maxCheckpoints = 100

// FileSnapshot captures the pre-mutation state of a single file: its content
// immediately before a turn first touched it, or — when Existed is false — the
// fact that it did not yet exist (so an undo must delete rather than restore it).
type FileSnapshot struct {
	// Path is the resolved absolute path the file operation acted on.
	Path string
	// Existed reports whether the file was present before the turn mutated it.
	Existed bool
	// Content is the file's bytes at the turn's start. It is empty when the file
	// did not yet exist, and nil-but-Existed when the file was present but could
	// not be read (an undo then cannot restore it and skips it).
	Content []byte
	// Mode is the file's mode at the turn's start, so a restored executable keeps
	// its bits. Zero when the file did not exist.
	Mode os.FileMode
}

// Checkpoint is the set of file snapshots recorded during one turn. Each file is
// snapshotted at most once per turn — the first mutation wins — so the checkpoint
// always reflects the workspace's state at the turn's start, no matter how many
// times a file was edited within it (issue #41).
type Checkpoint struct {
	Files map[string]FileSnapshot // keyed by resolved absolute path
}

// sessionCheckpoints holds one session's accumulating (in-flight) checkpoint and
// its committed turn history (newest last).
type sessionCheckpoints struct {
	active  *Checkpoint
	history []*Checkpoint
}

// Checkpointer records pre-mutation file snapshots, grouped by turn and session,
// so a botched multi-file edit can be rolled back with UndoLastTurn / Rewind
// without the user resorting to their own VCS (issue #41). Snapshots live in
// memory for the running process; restarting loses them (the transcript is still
// recovered from disk via the JSONL store). It is safe for concurrent use:
// sessions may overlap and sub-agents mutate files from parallel goroutines.
type Checkpointer struct {
	fs       *FileSystem
	mu       sync.Mutex
	sessions map[string]*sessionCheckpoints
}

// NewCheckpointer creates a Checkpointer that resolves and reads files through
// fs. A nil fs yields a nil Checkpointer (checkpointing disabled).
func NewCheckpointer(fs *FileSystem) *Checkpointer {
	if fs == nil {
		return nil
	}
	return &Checkpointer{fs: fs, sessions: make(map[string]*sessionCheckpoints)}
}

// state returns (creating on demand) the per-session checkpoint state. The caller
// must hold c.mu.
func (c *Checkpointer) state(sessionID string) *sessionCheckpoints {
	st := c.sessions[sessionID]
	if st == nil {
		st = &sessionCheckpoints{}
		c.sessions[sessionID] = st
	}
	return st
}

// BeginTurn starts accumulating snapshots for a new turn, discarding any previous
// in-flight (uncommitted) checkpoint for the session. Call it at the start of a
// user-driven turn; pair it with CommitTurn (or AbortTurn) when the turn ends.
func (c *Checkpointer) BeginTurn(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state(sessionID).active = &Checkpoint{Files: make(map[string]FileSnapshot)}
}

// Snapshot records path's current state into the session's active checkpoint. It
// is a no-op when no turn is in progress. The first snapshot of a path within a
// turn wins, so later edits to the same file do not overwrite the turn's initial
// snapshot. It is best-effort: resolve/read failures are swallowed (an unreadable
// file is left without restorable content) so checkpointing can never block or
// fail a write — it is a safety net, not a gate.
func (c *Checkpointer) Snapshot(sessionID, path string, auth Authorization) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state(sessionID)
	if st.active == nil {
		return
	}
	resolved, err := c.fs.Abs(path)
	if err != nil {
		return
	}
	if _, seen := st.active.Files[resolved]; seen {
		return // first mutation in this turn already captured the pre-turn state
	}
	snap := FileSnapshot{Path: resolved}
	if info, err := os.Stat(resolved); err == nil {
		snap.Existed = true
		snap.Mode = info.Mode()
		if content, err := c.fs.Read(path, auth); err == nil {
			snap.Content = content
		}
		// A present-but-unreadable file keeps Existed with nil Content; restore
		// skips it rather than clobbering it with empty bytes.
	}
	st.active.Files[resolved] = snap
}

// CommitTurn finalizes the in-flight checkpoint, pushing it onto the session's
// history. A turn that mutated nothing is dropped so undo never offers an empty
// no-op. When no turn is in progress it is a no-op.
func (c *Checkpointer) CommitTurn(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state(sessionID)
	if st.active == nil {
		return
	}
	if len(st.active.Files) > 0 {
		st.history = append(st.history, st.active)
		// Bound retained history so a long session cannot grow it without limit.
		if len(st.history) > maxCheckpoints {
			st.history = st.history[len(st.history)-maxCheckpoints:]
		}
	}
	st.active = nil
}

// AbortTurn discards the in-flight checkpoint without committing it.
func (c *Checkpointer) AbortTurn(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.sessions[sessionID]; st != nil {
		st.active = nil
	}
}

// Count reports the number of committed (undoable) turns recorded for a session.
func (c *Checkpointer) Count(sessionID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st := c.sessions[sessionID]; st != nil {
		return len(st.history)
	}
	return 0
}

// ErrNoCheckpoint is returned by UndoLastTurn / Rewind when a session has no
// committed checkpoint to revert.
var ErrNoCheckpoint = errors.New("no checkpoint to undo")

// UndoLastTurn reverts the most recently committed turn, restoring every file it
// touched to its pre-turn state, and drops it from history. It returns the number
// of files restored and ErrNoCheckpoint when there is nothing to undo.
func (c *Checkpointer) UndoLastTurn(sessionID string) (int, error) {
	c.mu.Lock()
	st := c.sessions[sessionID]
	if st == nil || len(st.history) == 0 {
		c.mu.Unlock()
		return 0, ErrNoCheckpoint
	}
	last := st.history[len(st.history)-1]
	st.history = st.history[:len(st.history)-1]
	c.mu.Unlock()
	return restoreCheckpoint(last)
}

// Rewind reverts the last turns turns — turns <= 0 reverts every recorded turn —
// merging their snapshots so that, per file, the earliest reverted turn's
// pre-turn state wins. It drops the reverted turns from history and returns the
// number of files restored and turns actually reverted.
func (c *Checkpointer) Rewind(sessionID string, turns int) (files, reverted int, err error) {
	c.mu.Lock()
	st := c.sessions[sessionID]
	if st == nil || len(st.history) == 0 {
		c.mu.Unlock()
		return 0, 0, ErrNoCheckpoint
	}
	if turns <= 0 || turns > len(st.history) {
		turns = len(st.history)
	}
	// Pop the last `turns` checkpoints (newest last) and merge oldest-wins: a
	// file touched in several of them is restored to its state before the
	// earliest of those turns.
	popped := st.history[len(st.history)-turns:]
	st.history = st.history[:len(st.history)-turns]
	c.mu.Unlock()

	merged := make(map[string]FileSnapshot)
	for _, cp := range popped { // oldest -> newest; first write wins = earliest turn
		for k, v := range cp.Files {
			if _, ok := merged[k]; !ok {
				merged[k] = v
			}
		}
	}
	n, rerr := restoreCheckpoint(&Checkpoint{Files: merged})
	return n, turns, rerr
}

// restoreCheckpoint writes each snapshot back to its pre-turn state: existing
// files get their original content (and mode), files the turn created are
// removed, and present-but-unreadable files are skipped. Per-file errors are
// joined rather than aborting the whole restore, so one locked file does not
// strand the rest of the rollback.
func restoreCheckpoint(cp *Checkpoint) (int, error) {
	var errs error
	restored := 0
	for _, snap := range cp.Files {
		if err := restoreSnapshot(snap); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		restored++
	}
	return restored, errs
}

// restoreSnapshot applies a single file's pre-turn state to disk.
func restoreSnapshot(snap FileSnapshot) error {
	if !snap.Existed {
		// The turn created this file; remove it. "already gone" is a success.
		if err := os.Remove(snap.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove snapshot file: %w", err)
		}
		return nil
	}
	if snap.Content == nil {
		// The file existed but its content could not be captured; leave it.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(snap.Path), 0o755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	if err := os.WriteFile(snap.Path, snap.Content, snap.Mode); err != nil {
		return fmt.Errorf("restore snapshot file: %w", err)
	}
	return nil
}
