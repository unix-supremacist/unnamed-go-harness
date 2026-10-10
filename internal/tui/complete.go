// Tab completion: slash-command autocomplete for inputs starting with "/" and
// @filepath completion for the text after the last "@"; repeated tabs cycle
// the candidates while a hint line above the editor shows them.
package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clearComp drops the current completion state (any key that edits the text).
func (s *State) clearComp() {
	s.compCands, s.compIdx, s.compSeed, s.compHint = nil, 0, "", ""
}

// compToken classifies the completion point of v: "cmd" (the whole input is
// a slash command), "file" (at = index of the active "@"), or "" (nothing to
// complete).
func compToken(v string) (kind string, at int) {
	if strings.HasPrefix(v, "/") && !strings.Contains(v, " ") {
		return "cmd", -1
	}
	if at = strings.LastIndex(v, "@"); at < 0 {
		return "", -1
	}
	if strings.ContainsAny(v[at+1:], " \t") {
		return "", -1
	}
	return "file", at
}

// tabComplete completes or cycles the token at the completion point.
func (s *State) tabComplete() {
	v := s.ta.Value()
	kind, at := compToken(v)
	if kind == "" {
		s.clearComp()
		s.layout()
		s.refresh()
		return
	}
	cands := s.compCands
	if v != s.compSeed || strings.HasSuffix(v, "/") || len(s.compCands) == 0 {
		cands = candidatesFor(kind, s.cwd, v, at)
	}
	if len(cands) == 0 {
		// Nothing matches (e.g. @nosuchfile): leave the input alone.
		s.clearComp()
		s.compHint = "↹ no matches"
		s.layout()
		s.refresh()
		return
	}
	s.compCands = cands
	if v != s.compSeed || strings.HasSuffix(v, "/") {
		s.compIdx = 0
	} else if s.compIdx >= len(cands) {
		s.compIdx = 0
	}
	pick := cands[s.compIdx]
	next := pick
	if kind == "file" {
		next = v[:at+1] + pick
	}
	s.ta.SetValue(next)
	s.ta.MoveToEnd()
	s.compIdx = (s.compIdx + 1) % len(cands) // next tab cycles
	s.compSeed = next
	if len(cands) == 1 {
		s.compHint = ""
		s.compCands, s.compSeed = nil, ""
	} else {
		s.compHint = "↹ " + strings.Join(head(cands, s.compIdx, 8), "  ")
	}
	s.layout()
	s.refresh()
}

// candidatesFor lists the completions for one token: slash commands by
// prefix, or cwd-relative directory entries for the text after the "@".
// Directories carry a trailing "/" so deeper paths complete on the next tab.
func candidatesFor(kind, cwd, v string, at int) []string {
	if kind == "cmd" {
		return slashMatches(v)
	}
	token := v[at+1:]
	dir, prefix := "", token
	if i := strings.LastIndex(token, "/"); i >= 0 {
		dir, prefix = token[:i+1], token[i+1:]
	}
	entries, err := os.ReadDir(filepath.Join(cwd, dir))
	if err != nil {
		return nil
	}
	var cands []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || (strings.HasPrefix(name, ".") && prefix == "") {
			continue
		}
		cand := dir + name
		if e.IsDir() {
			cand += "/"
		}
		cands = append(cands, cand)
	}
	sort.Strings(cands)
	if len(cands) > 200 {
		cands = cands[:200]
	}
	return cands
}

// head takes a window of the candidate list around the cursor for the hint.
func head(cands []string, idx, max int) []string {
	lo := idx - max/2
	if lo < 0 {
		lo = 0
	}
	hi := lo + max
	if hi > len(cands) {
		hi = len(cands)
		lo = hi - max
		if lo < 0 {
			lo = 0
		}
	}
	out := make([]string, 0, hi-lo)
	for i := lo; i < hi; i++ {
		if i == idx {
			out = append(out, "["+cands[i]+"]")
		} else {
			out = append(out, cands[i])
		}
	}
	return out
}
