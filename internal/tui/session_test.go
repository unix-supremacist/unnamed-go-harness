package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
)

type nopProvider struct{}

func (nopProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, error) {
	ch := make(chan llm.Event)
	close(ch)
	return ch, nil
}

func sessionState(t *testing.T) *State {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := testState(t)
	s.cwd = t.TempDir()
	s.modelRef, s.thinking, s.system = "fake/m", "off", "sys"
	s.providers = map[string]llm.Provider{"fake": nopProvider{}}
	s.maxTurns = 4
	if err := s.switchSession(s.freshSession()); err != nil {
		t.Fatal(err)
	}
	return s
}

func writeSession(t *testing.T, s *State, parent string) string {
	t.Helper()
	st, err := session.Create(s.cwd, session.Record{Model: "fake/m", Cwd: s.cwd, Parent: parent, System: "sys", Thinking: "off"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(m llm.Message) { _ = st.Append(session.Record{Type: "message", Message: &m}) }
	add(llm.Message{Role: llm.RoleUser, Text: "first prompt"})
	add(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "a", Name: "ls"}}})
	add(llm.Message{Role: llm.RoleTool, ToolCallID: "a", ToolName: "ls", Text: "files"})
	_ = st.Append(session.Record{Type: "compaction", Seed: []llm.Message{{Role: llm.RoleUser, Text: "SUMMARY-TEXT"}}})
	add(llm.Message{Role: llm.RoleUser, Text: "second prompt"})
	add(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "b", Name: "read"}}}) // never answered
	p := st.Path()
	st.Close()
	return p
}

func TestResumeReplaysTranscriptWithMarkers(t *testing.T) {
	s := sessionState(t)
	path := writeSession(t, s, "")
	if err := s.switchSession(s.resumeSession(path)); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range s.entries {
		kinds = append(kinds, e.kind)
	}
	// user, tool(ls), compaction marker, user, tool(read, unanswered), resumed notice
	if strings.Join(kinds, ",") != "user,tool,info,user,tool,info" {
		t.Fatalf("entries = %v", kinds)
	}
	if !strings.Contains(s.entries[2].text, "SUMMARY-TEXT") || s.entries[1].tool.status != "ok" {
		t.Fatalf("marker/tool wrong: %+v", s.entries[2])
	}
	if st := s.entries[4].tool.status; st != "error" {
		t.Fatalf("unanswered call must not spin forever, status %q", st)
	}
	if len(s.history) != 2 || s.history[1] != "second prompt" || s.histIdx != -1 {
		t.Fatalf("up-arrow history = %v", s.history)
	}
	if s.sessCloser().Path() != path {
		t.Fatal("store not swapped")
	}
}

func TestFailedSwitchKeepsOldSession(t *testing.T) {
	s := sessionState(t)
	oldPath, oldAgent, gen := s.sessCloser().Path(), s.agent, s.gen
	s.addEntry(entry{kind: "user", text: "keep me"})
	if err := s.switchSession(s.resumeSession(s.cwd + "/missing.jsonl")); err == nil {
		t.Fatal("expected error")
	}
	// A subagent session is not a resume target.
	child := writeSession(t, s, "root")
	if err := s.switchSession(s.resumeSession(child)); err == nil {
		t.Fatal("subagent session must be refused")
	}
	// A build failure removes the file the switch created.
	s.modelRef = "nonsense"
	files, _ := os.ReadDir(strings.TrimSuffix(oldPath, "/"+baseName(oldPath)))
	before := len(files)
	if err := s.switchSession(s.freshSession()); err == nil {
		t.Fatal("expected build error")
	}
	files, _ = os.ReadDir(strings.TrimSuffix(oldPath, "/"+baseName(oldPath)))
	if len(files) != before {
		t.Fatalf("failed switch leaked a session file (%d → %d)", before, len(files))
	}
	s.modelRef = "fake/m"
	if s.sessCloser().Path() != oldPath || s.agent != oldAgent || s.gen != gen || s.entries[len(s.entries)-1].text != "keep me" {
		t.Fatal("old session was disturbed")
	}
}

func baseName(p string) string { return p[strings.LastIndex(p, "/")+1:] }

func TestSwitchRefusedWhileBusy(t *testing.T) {
	s := sessionState(t)
	path := writeSession(t, s, "")
	for name, set := range map[string]func(){
		"running":    func() { s.running = true },
		"compacting": func() { s.compacting = true },
	} {
		set()
		if err := s.switchSession(s.resumeSession(path)); err == nil {
			t.Fatalf("%s: switch must be refused", name)
		}
		s.running, s.compacting = false, false
	}
	// The command table refuses idle-only commands while running.
	s.running = true
	before := s.sessCloser().Path()
	s.command("/resume 1")
	s.command("/clear")
	if s.sessCloser().Path() != before {
		t.Fatal("idle-only command ran while running")
	}
	last := s.entries[len(s.entries)-1]
	if !strings.Contains(last.text, "unavailable while a run is in progress") {
		t.Fatalf("no refusal message: %q", last.text)
	}
}

func TestStaleResultsAfterSwitchAreDropped(t *testing.T) {
	s := sessionState(t)
	staleGen := s.gen
	if err := s.switchSession(s.freshSession()); err != nil {
		t.Fatal(err)
	}
	n := len(s.entries)
	s.compacting = true
	s.Update(compactedMsg{summary: "late", n: 3, gen: staleGen})
	s.Update(askMsg{text: "late answer", gen: staleGen})
	s.running = true
	s.Update(runDoneMsg{final: "late", gen: staleGen})
	if len(s.entries) != n || !s.running || s.compacting {
		t.Fatalf("stale messages leaked: entries %d→%d running=%v compacting=%v", n, len(s.entries), s.running, s.compacting)
	}
}

func TestPickerListsRootsAndResumeNMatchesOrder(t *testing.T) {
	s := sessionState(t)
	root := writeSession(t, s, "")
	writeSession(t, s, "root") // a child must not appear
	cmd := s.openResume()
	msg := cmd().(resumeScanMsg)
	s.Update(msg)
	var paths []string
	for _, i := range s.resume.items {
		paths = append(paths, s.resume.all[i].Path)
	}
	// The fresh session from setup plus the written root; never the child.
	if len(paths) != 2 {
		t.Fatalf("picker rows = %v", paths)
	}
	// A superseded scan is dropped.
	s.resumeSeq++
	s.resume = &resumeState{seq: s.resumeSeq}
	s.onResumeScan(resumeScanMsg{seq: s.resumeSeq - 1, infos: msg.infos})
	if len(s.resume.all) != 0 {
		t.Fatal("stale scan applied")
	}
	// Typing filters; enter resumes.
	s.resume = nil
	s.Update(msg2(s.openResume()))
	for _, r := range "first" {
		s.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if s.resume != nil || s.sessCloser().Path() != root {
		t.Fatalf("enter did not open the filtered session: %v", s.sessCloser().Path())
	}
}

func msg2(c tea.Cmd) tea.Msg { return c() }

func TestCopyAndInitCommands(t *testing.T) {
	s := sessionState(t)
	if cmd := s.command("/copy"); cmd != nil {
		t.Fatal("nothing to copy yet, expected no command")
	}
	if err := s.rebuildAgent([]llm.Message{{Role: llm.RoleUser, Text: "q"}, {Role: llm.RoleAssistant, Text: "the answer"}}); err != nil {
		t.Fatal(err)
	}
	if cmd := s.command("/copy"); cmd == nil {
		t.Fatal("expected an OSC 52 clipboard command")
	}
	// /init is a normal user turn with the canned prompt, and is refused mid-run.
	s.running = true
	if cmd := s.command("/init"); cmd != nil {
		t.Fatal("/init ran while a run was in progress")
	}
	s.running = false
	if cmd := s.command("/init"); cmd == nil || !s.running {
		t.Fatal("/init did not start a run")
	}
	if !strings.Contains(initPrompt, "AGENTS.md") {
		t.Fatal("canned prompt lost")
	}
}
