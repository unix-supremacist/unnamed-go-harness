package perm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logEntry is one line of ~/.local/state/ugh/decisions.jsonl. Every string
// field is scrubbed before it is written, so no secret ever lands in the log.
type logEntry struct {
	TS       string    `json:"ts"`
	Mode     string    `json:"mode"`
	Agent    string    `json:"agent,omitempty"`
	Cwd      string    `json:"cwd,omitempty"`
	Tool     string    `json:"tool"`
	Tier     string    `json:"tier,omitempty"`
	TierWhy  string    `json:"tier_reason,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Verdicts []Verdict `json:"verdicts"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason,omitempty"`
	Answer   string    `json:"answer,omitempty"` // prompt answer: y/a/n/s
}

// DecisionLog appends JSONL decision records. A nil *DecisionLog is valid
// (logging disabled).
type DecisionLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	err  error
}

// DefaultLogPath returns $XDG_STATE_HOME/ugh/decisions.jsonl (or
// ~/.local/state/ugh/decisions.jsonl).
func DefaultLogPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home := os.Getenv("HOME")
		if !filepath.IsAbs(home) {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ugh", "decisions.jsonl")
}

// NewLog opens (creating, 0700 dir / 0600 file) the decision log at path.
func NewLog(path string) (*DecisionLog, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &DecisionLog{path: path, f: f}, nil
}

// Close releases the log file.
func (l *DecisionLog) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// Add appends one record. Logging failures never block a permission decision.
func (l *DecisionLog) Add(e logEntry) {
	if l == nil {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	e.Tool = Scrub(e.Tool)
	e.Detail = Scrub(truncate(e.Detail, 500))
	e.Reason = Scrub(e.Reason)
	e.Agent, e.Cwd = Scrub(e.Agent), Scrub(e.Cwd)
	for i := range e.Verdicts {
		// Errors are not JSON-serializable; keep them visible in the log line.
		reason := e.Verdicts[i].Reason
		if reason == "" && e.Verdicts[i].Err != nil {
			reason = "error: " + e.Verdicts[i].Err.Error()
		}
		e.Verdicts[i].Reason = Scrub(truncate(reason, 300))
	}
	if e.Verdicts == nil {
		e.Verdicts = []Verdict{}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil && l.err == nil {
		l.err = err
	}
}

// LogError reports the first write error (for tests/diagnostics).
func (l *DecisionLog) LogError() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// stringArg pulls a top-level string field out of raw tool arguments.
func stringArg(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil {
		return ""
	}
	return s
}
