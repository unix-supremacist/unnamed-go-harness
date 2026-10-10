// /compact: summarize the old turns with the current model, start a new
// session JSONL seeded with that summary, and continue from there — recall
// of pre-compaction context comes from the summary, new messages append to
// the fresh file (PLAN.md §8).
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/llm"
)

// compact kicks off the background summary run for the live history; the
// result lands as compactedMsg and is applied by applyCompact. The summary is
// written by the compaction model ([compaction] model, /compact-model), else
// the conversation model.
func (s *State) compact() tea.Cmd {
	if s.running {
		s.addEntry(entry{kind: "info", text: "· still running — esc cancels"})
		s.refresh()
		return nil
	}
	if s.compacting {
		s.addEntry(entry{kind: "info", text: "· a compaction is already in progress"})
		s.refresh()
		return nil
	}
	hist := s.agentHistory()
	if len(hist) == 0 {
		s.addEntry(entry{kind: "info", text: "· nothing to compact yet"})
		s.refresh()
		return nil
	}
	s.addEntry(entry{kind: "info", text: fmt.Sprintf("· compacting %d messages with %s…", len(hist), s.summarizerLabel())})
	s.refresh()
	s.compacting = true
	gen, ag := s.gen, s.agent
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		text, usage, err := ag.Summarize(ctx, hist)
		if err != nil {
			return compactedMsg{n: len(hist), err: err, gen: gen, usage: usage}
		}
		return compactedMsg{summary: strings.TrimSpace(text), n: len(hist), gen: gen, usage: usage}
	}
}

// summarizerLabel names the model /compact will use.
func (s *State) summarizerLabel() string {
	if m := agent.ContextConfigFrom(s.runtime.Config).CompactModel; m != "" {
		return m
	}
	return s.modelRef
}

// applyCompact swaps the live history for the summary in place: the same
// session file gets a compaction record, so lineage, the tool-result archive
// and replay all stay in one file.
func (s *State) applyCompact(m compactedMsg) {
	if m.err != nil {
		s.addEntry(entry{kind: "error", text: "/compact: " + m.err.Error()})
		s.refresh()
		return
	}
	seed := []llm.Message{{Role: llm.RoleUser, Text: "Summary of the earlier conversation (the session was compacted; this " +
		"summary replaces the summarized turns):\n\n" + m.summary}}
	s.agent.ApplyCompaction(seed, m.usage)
	s.usage = s.agent.Usage
	s.addEntry(entry{kind: "info", text: fmt.Sprintf("· compacted %d messages in place (%s)\n%s",
		m.n, filepath.Base(s.sessCloser().Path()), m.summary)})
	s.refresh()
}
