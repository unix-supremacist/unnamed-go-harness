package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/unix/ugh/internal/llm"
)

// Conditional instructions: AGENTS.md files that apply to the files an agent
// touches, shown at the moment they become relevant instead of all up front.
//
//   - The cwd's AGENTS.md (fallback CLAUDE.md) joins the system prompt at build;
//     it is stable, so it is cache-safe.
//   - Before read/edit/write runs, the AGENTS.md files between the cwd and the
//     target's directory are collected. A mutation whose instructions have not
//     been shown yet does not execute: its own tool result carries them, so
//     result batches stay contiguous. A read executes and carries them too.
//   - bash is not covered: shell file access triggers no path hooks.

const (
	instrFile      = "AGENTS.md"
	instrFallback  = "CLAUDE.md"
	instrMaxBytes  = 16 << 10
	instrPrecedent = "(More specific files appear later and take precedence over earlier ones where they conflict.)"
)

// seenInstr is one instruction file already shown to the model.
type seenInstr struct {
	rel   string
	mtime int64
	text  string
}

type instrFileInfo struct {
	abs, rel string
	mtime    int64
	text     string
}

// rootInstructions reads the cwd's AGENTS.md (or CLAUDE.md) for the system prompt.
func rootInstructions(cwd string) string {
	for _, name := range []string{instrFile, instrFallback} {
		if b, err := os.ReadFile(filepath.Join(cwd, name)); err == nil {
			text := strings.TrimSpace(clipInstr(string(b)))
			if text == "" {
				continue
			}
			return "\n\nProject instructions (" + name + "):\n" + text
		}
	}
	return ""
}

func clipInstr(s string) string {
	if len(s) > instrMaxBytes {
		return strings.ToValidUTF8(s[:instrMaxBytes], "") + "\n…[truncated]"
	}
	return s
}

// realPath resolves symlinks even when the path itself does not exist yet (a
// file about to be written): the deepest existing ancestor is resolved and the
// rest re-attached.
func realPath(p string) (string, bool) {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), true
		}
		if cur == filepath.Dir(cur) {
			return "", false
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}

// instructionFilesFor lists the AGENTS.md files that apply to path, shallowest
// first. Paths outside the cwd (absolute, or escaping through a symlink) get
// none.
func instructionFilesFor(cwd, path string) []instrFileInfo {
	root, ok := realPath(cwd)
	if !ok {
		return nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	target, ok := realPath(path)
	if !ok {
		return nil
	}
	rel, err := filepath.Rel(root, filepath.Dir(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	var out []instrFileInfo
	dir := root
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			dir = filepath.Join(dir, part)
			cand := filepath.Join(dir, instrFile)
			real, err := filepath.EvalSymlinks(cand)
			if err != nil {
				continue
			}
			// The file itself must not lead out of the tree either.
			if r, err := filepath.Rel(root, real); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				continue
			}
			st, err := os.Stat(real)
			if err != nil || st.IsDir() {
				continue
			}
			b, err := os.ReadFile(real)
			if err != nil {
				continue
			}
			text := strings.TrimSpace(clipInstr(string(b)))
			if text == "" {
				continue
			}
			rp, _ := filepath.Rel(root, cand)
			out = append(out, instrFileInfo{abs: real, rel: filepath.ToSlash(rp), mtime: st.ModTime().UnixNano(), text: text})
		}
	}
	return out
}

// unseenInstructions returns the applicable files not yet shown (or edited
// since), and does not mark them.
func (a *Agent) unseenInstructions(path string) []instrFileInfo {
	if !a.cfg.Context.Instructions || a.cfg.OneShot || path == "" {
		return nil
	}
	var out []instrFileInfo
	for _, f := range instructionFilesFor(a.cfg.Cwd, path) {
		if s, ok := a.instr[f.abs]; ok && s.mtime == f.mtime {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (a *Agent) markSeen(fs []instrFileInfo) {
	if a.instr == nil {
		a.instr = map[string]seenInstr{}
	}
	for _, f := range fs {
		a.instr[f.abs] = seenInstr{rel: f.rel, mtime: f.mtime, text: f.text}
	}
}

func instrEnd(rel string) string { return "[End of " + rel + "]" }

// instructionBlock renders files for a tool result.
func instructionBlock(fs []instrFileInfo) string {
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "[Instructions from %s]\n%s\n%s\n", f.rel, f.text, instrEnd(f.rel))
	}
	if len(fs) > 1 {
		b.WriteString(instrPrecedent + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// instructionPath extracts the path a read/edit/write call targets.
func instructionPath(tool, args string) (path string, mutates, ok bool) {
	switch tool {
	case "read":
	case "edit", "write":
		mutates = true
	default:
		return "", false, false
	}
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || strings.TrimSpace(a.Path) == "" {
		return "", false, false
	}
	return a.Path, mutates, true
}

// instructionTail lists the instruction files shown so far, for a compaction
// seed: the summary replaces the tool results that carried them.
func (a *Agent) instructionTail() string {
	if len(a.instr) == 0 {
		return ""
	}
	seen := make([]seenInstr, 0, len(a.instr))
	for _, s := range a.instr {
		seen = append(seen, s)
	}
	sort.Slice(seen, func(i, j int) bool {
		di, dj := strings.Count(seen[i].rel, "/"), strings.Count(seen[j].rel, "/")
		if di != dj {
			return di < dj
		}
		return seen[i].rel < seen[j].rel
	})
	var b strings.Builder
	b.WriteString("\n\nInstructions still in effect (AGENTS.md files shown earlier in this conversation):\n")
	for _, s := range seen {
		fmt.Fprintf(&b, "[Instructions from %s]\n%s\n%s\n", s.rel, s.text, instrEnd(s.rel))
	}
	if len(seen) > 1 {
		b.WriteString(instrPrecedent + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

var instrMarker = regexp.MustCompile(`(?s)\[Instructions from ([^\]\n]+)\]\n(.*?)\n\[End of ([^\]\n]+)\]`)

// restoreInstructions rebuilds the seen set from the instruction blocks that
// earlier tool results (and compaction seeds) carried, so a resumed session
// does not re-announce files the model already has.
func (a *Agent) restoreInstructions() {
	if !a.cfg.Context.Instructions || a.cfg.OneShot {
		return
	}
	scan := func(text string) {
		for _, m := range instrMarker.FindAllStringSubmatch(text, -1) {
			rel := m[1]
			if rel != m[3] || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
				continue
			}
			abs, ok := realPath(filepath.Join(a.cfg.Cwd, rel))
			if !ok {
				continue
			}
			st, err := os.Stat(abs)
			if err != nil {
				continue
			}
			if a.instr == nil {
				a.instr = map[string]seenInstr{}
			}
			a.instr[abs] = seenInstr{rel: rel, mtime: st.ModTime().UnixNano(), text: m[2]}
		}
	}
	a.arch.mu.Lock()
	ents := make([]archived, 0, len(a.arch.ents))
	for _, e := range a.arch.ents {
		ents = append(ents, e)
	}
	a.arch.mu.Unlock()
	sort.Slice(ents, func(i, j int) bool { return ents[i].Ord < ents[j].Ord })
	for _, e := range ents {
		scan(e.Text)
	}
	for _, m := range a.hist {
		if m.Role == llm.RoleTool || m.Role == llm.RoleUser {
			scan(m.Text)
		}
	}
}

// instructionGate runs before a read/edit/write executes. It returns a result
// when the call must not run (a mutation with unseen instructions). For a read
// it returns the block to append after execution.
func (a *Agent) instructionGate(tc llm.ToolCall) (blocked string, appendAfter string, files []instrFileInfo) {
	path, mutates, ok := instructionPath(tc.Name, tc.Args)
	if !ok {
		return "", "", nil
	}
	fs := a.unseenInstructions(path)
	if len(fs) == 0 {
		return "", "", nil
	}
	if mutates {
		a.markSeen(fs)
		names := make([]string, len(fs))
		for i, f := range fs {
			names[i] = f.rel
		}
		return "not executed — " + strings.Join(names, ", ") + " applies to this path and had not been shown yet. " +
			"Re-issue the call if it still complies.\n\n" + instructionBlock(fs), "", nil
	}
	return "", instructionBlock(fs), fs
}
