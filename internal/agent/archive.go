package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
)

// The archive indexes every tool result this conversation has produced. It is
// the in-memory face of the session file: compaction replaces a.hist but never
// the archive, so recall and replay still reach the original bytes.
//
// A result's ordinal is its count among tool-result messages pushed in this
// conversation (1-based) and its alias is "c<ordinal>". Messages that are not
// in the file never take one: compaction seeds and the stubs reconcileHistory
// synthesizes. That keeps aliases stable across compaction and resume.

type archived struct {
	Ord    int
	CallID string
	Tool   string
	Args   string
	Text   string // the result as the model first saw it
	Blob   string // full-output file, "" when the result was not trimmed
}

func (e archived) alias() string { return aliasOf(e.Ord) }

func aliasOf(ord int) string { return fmt.Sprintf("c%d", ord) }

type archive struct {
	mu    sync.Mutex
	ord   int // tool results so far
	ents  map[int]archived
	calls map[string]llm.ToolCall // latest assistant call per id, to label results
}

func (r *archive) noteCalls(m llm.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]llm.ToolCall{}
	}
	for _, c := range m.ToolCalls {
		r.calls[c.ID] = c
	}
}

// add records one tool result and returns its entry.
func (r *archive) add(m llm.Message, blobDir string) archived {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ents == nil {
		r.ents = map[int]archived{}
	}
	r.ord++
	call := r.calls[m.ToolCallID]
	e := archived{Ord: r.ord, CallID: m.ToolCallID, Tool: m.ToolName, Args: call.Args, Text: m.Text}
	if e.Tool == "" {
		e.Tool = call.Name
	}
	if blobDir != "" {
		p := filepath.Join(blobDir, aliasOf(e.Ord)+".txt")
		if _, err := os.Stat(p); err == nil {
			e.Blob = p
		}
	}
	r.ents[e.Ord] = e
	return e
}

func (r *archive) get(ord int) (archived, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.ents[ord]
	return e, ok
}

// next is the ordinal the next pushed tool result will take.
func (r *archive) next() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ord + 1
}

func (r *archive) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ord
}

// seed rebuilds the archive from a session's chronological records. Without
// records (no Display available) it only advances the counter past the tool
// results already in history, so new aliases never collide with old ones.
func (r *archive) seed(display []session.Record, hist []llm.Message, blobDir string) {
	if len(display) == 0 {
		n := 0
		for _, m := range hist {
			if m.Role == llm.RoleTool {
				n++
			}
		}
		r.mu.Lock()
		r.ord = n
		r.mu.Unlock()
		return
	}
	for _, rec := range display {
		if rec.Message == nil {
			continue
		}
		switch rec.Message.Role {
		case llm.RoleAssistant:
			r.noteCalls(*rec.Message)
		case llm.RoleTool:
			r.add(*rec.Message, blobDir)
		}
	}
}

// uniqueCallIDs makes the ids of one assistant turn usable as pairing keys:
// an empty or repeated id becomes ugh_<turn>_<i>. Reports whether any changed.
func uniqueCallIDs(calls []llm.ToolCall, turn int) bool {
	seen := map[string]bool{}
	changed := false
	for i := range calls {
		id := calls[i].ID
		if id == "" || seen[id] {
			for n := i; ; n++ {
				id = fmt.Sprintf("ugh_%d_%d", turn, n)
				if !seen[id] {
					break
				}
			}
			calls[i].ID = id
			changed = true
		}
		seen[id] = true
	}
	return changed
}

// InheritFrom carries conversation-scoped state (the tool-result archive)
// from the agent this one replaces within the same session, so changing the
// model or thinking level does not make earlier results unrecallable.
func (a *Agent) InheritFrom(old *Agent) {
	if old == nil {
		return
	}
	old.arch.mu.Lock()
	defer old.arch.mu.Unlock()
	a.arch.mu.Lock()
	defer a.arch.mu.Unlock()
	a.arch.ord = old.arch.ord
	a.arch.ents = make(map[int]archived, len(old.arch.ents))
	for k, v := range old.arch.ents {
		a.arch.ents[k] = v
	}
	a.arch.calls = make(map[string]llm.ToolCall, len(old.arch.calls))
	for k, v := range old.arch.calls {
		a.arch.calls[k] = v
	}
	a.instr = make(map[string]seenInstr, len(old.instr))
	for k, v := range old.instr {
		a.instr[k] = v
	}
}
