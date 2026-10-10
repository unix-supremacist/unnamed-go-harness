// Package session persists conversations as append-only JSONL (one meta
// record, then one record per message) for TUI and headless resume.
package session

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/llm"
)

type Record struct {
	Type     string       `json:"type"`
	TS       string       `json:"ts"`
	Model    string       `json:"model,omitempty"`
	Thinking string       `json:"thinking,omitempty"`
	System   string       `json:"system,omitempty"`
	Cwd      string       `json:"cwd,omitempty"`
	Parent   string       `json:"parent,omitempty"`
	Agent    string       `json:"agent,omitempty"`
	Message  *llm.Message `json:"message,omitempty"`
	// Run links every session file of one agent tree (root and children).
	Run string `json:"run,omitempty"`
	// Result records (type "result") close a child or root run.
	Status string     `json:"status,omitempty"` // ok | turn-out | error | cancelled
	Turns  int        `json:"turns,omitempty"`
	Usage  *llm.Usage `json:"usage,omitempty"`
	Error  string     `json:"error,omitempty"`
	// Seed is the full replacement history of a "compaction" record.
	Seed []llm.Message `json:"seed,omitempty"`
	// Meta carries structured harness metadata (e.g. a "route" record: router,
	// tier, thinking, route chain, fallback reason).
	Meta map[string]any `json:"meta,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func StateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home := os.Getenv("HOME")
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("session: absolute HOME required")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ugh", "sessions"), nil
}

func Dir(cwd string) (string, error) {
	base, err := StateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(cwd))
	d := filepath.Join(base, fmt.Sprintf("%x", sum[:8]))
	return d, os.MkdirAll(d, 0o700)
}

// BlobDir is where a session keeps the full text of trimmed tool output. It
// lives and dies with the session file.
func BlobDir(sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	return sessionPath + ".blobs"
}

func Create(cwd string, meta Record) (*Store, error) {
	d, err := Dir(cwd)
	if err != nil {
		return nil, err
	}
	// Concurrent creators (council seats) can land on the same clock tick:
	// a taken name gets a numeric suffix instead of failing the agent.
	stamp := time.Now().UTC().Format("20060102-150405.000000000")
	var path string
	var f *os.File
	for i := 0; ; i++ {
		path = filepath.Join(d, stamp+".jsonl")
		if i > 0 {
			path = filepath.Join(d, fmt.Sprintf("%s-%d.jsonl", stamp, i))
		}
		f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			break
		}
		if !os.IsExist(err) || i >= 1000 {
			return nil, err
		}
	}
	s := &Store{path: path, f: f}
	meta.Type = "meta"
	if err := s.Append(meta); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, f: f}, nil
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) Append(r Record) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.f.Write(append(b, '\n'))
	return err
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

type Loaded struct {
	Path     string
	Meta     Record
	Messages []llm.Message
	// Display is the chronological archive: every message record and every
	// compaction marker (Seed kept), in file order. Compaction never removes
	// anything from it, so replay and recall can read the whole conversation.
	Display []Record
	// Councils are the "council" outcome records, replayed on resume.
	Councils []Record
}

func Load(path string) (*Loaded, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := &Loaded{Path: path}
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			if i == 0 {
				return nil, fmt.Errorf("session: %s: bad record: %w", filepath.Base(path), err)
			}
			continue // tolerate torn tail
		}
		if r.Type == "meta" && out.Meta.Model == "" {
			out.Meta = r
		}
		if r.Type == "council" {
			out.Councils = append(out.Councils, r)
			continue
		}
		if r.Type == "compaction" {
			out.Messages = append([]llm.Message(nil), r.Seed...)
			out.Display = append(out.Display, r)
			continue
		}
		if r.Message != nil {
			out.Messages = append(out.Messages, *r.Message)
			out.Display = append(out.Display, r)
		}
	}
	return out, nil
}

// Latest is the most recently active root session of cwd; child and council
// sessions are never a resume target.
func Latest(cwd string) (string, error) {
	roots, err := ListRoots(cwd, 1)
	if err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("session: no sessions for %s", cwd)
	}
	return roots[0].Path, nil
}

// Info is the cheap listing summary of one session file.
type Info struct {
	Path  string
	Meta  Record
	Title string // first user prompt, one line
	Msgs  int    // message records
	MTime time.Time
	Err   error // unreadable file
}

type infoKey struct {
	path  string
	size  int64
	mtime int64
}

var (
	infoMu    sync.Mutex
	infoCache = map[infoKey]Info{}
)

// Summary streams a session file once and returns its listing info; results
// are cached by path, size and mtime.
func Summary(path string) Info {
	st, err := os.Stat(path)
	if err != nil {
		return Info{Path: path, Err: err}
	}
	key := infoKey{path, st.Size(), st.ModTime().UnixNano()}
	infoMu.Lock()
	if in, ok := infoCache[key]; ok {
		infoMu.Unlock()
		return in
	}
	infoMu.Unlock()
	in := Info{Path: path, MTime: st.ModTime()}
	f, err := os.Open(path)
	if err != nil {
		in.Err = err
		return in
	}
	defer f.Close()
	rd := bufio.NewReader(f)
	first := true
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' { // a torn tail is ignored
			var r Record
			if json.Unmarshal(line, &r) != nil {
				if first {
					in.Err = fmt.Errorf("bad record")
					break
				}
			} else {
				if first && r.Type == "meta" {
					in.Meta = r
				}
				if r.Message != nil && r.Type != "compaction" {
					in.Msgs++
					if in.Title == "" && r.Message.Role == llm.RoleUser {
						in.Title = oneLine(r.Message.Text, 120)
					}
				}
			}
			first = false
		}
		if rerr != nil {
			break
		}
	}
	infoMu.Lock()
	infoCache[key] = in
	infoMu.Unlock()
	return in
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// firstMeta reads only the first record, cached with the same key as Summary.
func firstMeta(path string, st os.FileInfo) (Record, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(line, &r) != nil {
		return Record{}, false
	}
	return r, true
}

// ListRoots returns cwd's root sessions (no Parent) newest-activity first.
// Children are filtered out before the cap; limit <= 0 means all. Unreadable
// files stay in the list with Err set.
func ListRoots(cwd string, limit int) ([]Info, error) {
	d, err := Dir(cwd)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	type cand struct {
		path  string
		mtime time.Time
		bad   bool
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(d, e.Name())
		st, err := e.Info()
		if err != nil {
			continue
		}
		meta, ok := firstMeta(p, st)
		if ok && meta.Parent != "" {
			continue
		}
		cands = append(cands, cand{p, st.ModTime(), !ok})
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].mtime.Equal(cands[j].mtime) {
			return cands[i].mtime.After(cands[j].mtime)
		}
		return cands[i].path > cands[j].path
	})
	if limit > 0 && len(cands) > limit {
		cands = cands[:limit]
	}
	out := make([]Info, len(cands))
	for i, c := range cands {
		out[i] = Summary(c.path)
	}
	return out, nil
}

// InboxPath is where `ugh send` queues messages for one agent of a run.
func InboxPath(run, agent string) (string, error) {
	base, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(base), "inbox", run, strings.ReplaceAll(agent, "/", "__")+".jsonl"), nil
}

// InboxMessage is one queued message.
type InboxMessage struct {
	TS   string `json:"ts"`
	Text string `json:"text"`
}

// SendInbox appends a message to an agent's inbox.
func SendInbox(run, agent, text string) error {
	p, err := InboxPath(run, agent)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(InboxMessage{TS: time.Now().UTC().Format(time.RFC3339Nano), Text: text})
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadInbox returns messages appended after offset and the new offset;
// a partial last line is left for the next read.
func ReadInbox(path string, offset int64) ([]InboxMessage, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() <= offset {
		return nil, offset
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset
	}
	b, _ := io.ReadAll(f)
	end := strings.LastIndexByte(string(b), '\n')
	if end < 0 {
		return nil, offset
	}
	var out []InboxMessage
	for _, line := range strings.Split(string(b[:end]), "\n") {
		var m InboxMessage
		if json.Unmarshal([]byte(line), &m) == nil && m.Text != "" {
			out = append(out, m)
		}
	}
	return out, offset + int64(end) + 1
}

// ScratchPath is one agent's private scratch directory ($UGH_SCRATCH).
func ScratchPath(run, agent string) string {
	base, err := StateDir()
	if err != nil || run == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(base), "scratch", run, strings.ReplaceAll(agent, "/", "__"))
}
