// Package session records every LLM call Genie makes during one
// process lifetime as a JSONL file under
// $XDG_DATA_HOME/genie/sessions/<session-id>.jsonl. One file per
// `genie serve` (or one-shot `genie query`) invocation; each line is
// one normalize or generate call with its system + user prompt, the
// model's response, token usage, and timing.
//
// Pattern lifted from Claude Code's session log
// (~/.claude/projects/<project>/<session>.jsonl), without the
// project-scoping — Genie doesn't have a "project" concept.
//
// JSONL is append-only, one record per line; readers don't need to
// load the whole file. Each record is small (kilobytes), so even a
// busy serve session stays manageable.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// EnvDir overrides the sessions directory.
	EnvDir = "GENIE_SESSIONS_DIR"
)

// Record is one LLM call. Marshalled to a single JSONL line.
type Record struct {
	Timestamp  time.Time `json:"timestamp"`
	SessionID  string    `json:"session_id"`
	Call       string    `json:"call"` // "normalize" | "generate"
	Provider   string    `json:"provider,omitempty"`
	Field      string    `json:"field,omitempty"` // GraphQL field name being resolved
	Backend    string    `json:"backend,omitempty"`
	SystemText string    `json:"system,omitempty"` // concatenated SystemBlocks
	UserText   string    `json:"user,omitempty"`
	Response   string    `json:"response,omitempty"`
	Err        string    `json:"error,omitempty"`
	Usage      *Usage    `json:"usage,omitempty"`
	DurationMS int64     `json:"duration_ms"`
}

// Usage mirrors llm.Usage but is duplicated here so the session
// package doesn't depend on internal/llm (and vice versa — internal/
// llm imports session in the recording wrapper).
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
}

// Session owns the JSONL file for one process lifetime. Append is
// safe for concurrent use.
type Session struct {
	id   string
	path string

	mu sync.Mutex
	f  *os.File
}

// New creates a session backed by a fresh JSONL file under the
// resolved sessions directory. The session ID is a UUID; the file
// path is logged so the caller can surface it.
//
// Returns a no-op Session (writes go to /dev/null) if the directory
// can't be created — sessions are debug aid, not load-bearing, and
// should never crash the process.
func New() *Session {
	id := uuid.NewString()
	dir, err := resolveDir()
	if err != nil {
		return &Session{id: id}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return &Session{id: id}
	}
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return &Session{id: id}
	}
	return &Session{id: id, path: path, f: f}
}

// ID returns the session's UUID.
func (s *Session) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// Path returns the JSONL file path, or "" if the session is no-op.
func (s *Session) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Append writes one record. Errors are swallowed — a write failure
// shouldn't abort the LLM call that triggered it.
func (s *Session) Append(rec Record) {
	if s == nil || s.f == nil {
		return
	}
	rec.SessionID = s.id
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.f.Write(append(buf, '\n'))
}

// Close flushes and closes the underlying file.
func (s *Session) Close() error {
	if s == nil || s.f == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.f.Close()
	s.f = nil
	return err
}

func resolveDir() (string, error) {
	if v := os.Getenv(EnvDir); v != "" {
		return v, nil
	}
	dir, err := userDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "genie", "sessions"), nil
}

// userDataDir returns $XDG_DATA_HOME (Linux), ~/Library/Application
// Support (macOS), or %APPDATA% (Windows). Go's stdlib doesn't
// expose this directly (UserConfigDir is config, UserCacheDir is
// cache); roll our own.
func userDataDir() (string, error) {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	switch detectOS() {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support"), nil
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return v, nil
		}
		return filepath.Join(home, "AppData", "Roaming"), nil
	default: // linux + unknown
		return filepath.Join(home, ".local", "share"), nil
	}
}
