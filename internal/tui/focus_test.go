package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/tools"
)

func testState(t *testing.T) *State {
	t.Helper()
	s := &State{w: 100, h: 40, histIdx: -1}
	s.initTheme("dark")
	s.ta = textarea.New()
	s.vp = viewport.New()
	s.vp.SetWidth(100)
	s.vp.SetHeight(30)
	s.runtime = &agent.Runtime{Tree: &agent.Tree{}}
	return s
}

// naive renders the active transcript without the prefix cache.
func naive(s *State) string {
	var lines []string
	for _, e := range *s.active() {
		lines = append(lines, s.render(&e)...)
	}
	return strings.Join(lines, "\n")
}

func plain(s string) string { return strings.TrimSpace(ansi.Strip(s)) }

func TestRefreshCacheMatchesFullRender(t *testing.T) {
	s := testState(t)
	s.addEntry(entry{kind: "user", text: "hello"})
	s.applyEvent(agent.Event{Kind: "thinking", Text: "let me think"})
	s.applyEvent(agent.Event{Kind: "tool_start", Call: llm.ToolCall{ID: "1", Name: "bash", Args: `{"command":"ls"}`}})
	for _, d := range []string{"Here ", "is ", "the ", "answer."} {
		s.applyEvent(agent.Event{Kind: "text", Text: d})
		if !s.dirty {
			t.Fatal("events must only mark the transcript dirty")
		}
		s.refresh()
		if got, want := plain(s.vp.GetContent()), plain(naive(s)); got != want {
			t.Fatalf("cached render diverged:\n%s\n---\n%s", got, want)
		}
	}
	// An earlier entry changes (the tool finishes): the cache must notice.
	s.applyEvent(agent.Event{Kind: "tool_done", Call: llm.ToolCall{ID: "1", Name: "bash"}, Output: "a.go", Elapsed: time.Second})
	s.refresh()
	if got, want := plain(s.vp.GetContent()), plain(naive(s)); got != want || !strings.Contains(got, "✓") {
		t.Fatalf("after tool_done:\n%s\n---\n%s", got, want)
	}
	if !strings.Contains(plain(s.vp.GetContent()), "thought for 1 line") {
		t.Errorf("thinking block missing:\n%s", plain(s.vp.GetContent()))
	}
}

// stepProvider streams a scripted child turn; turn 2 blocks until release.
type stepProvider struct {
	n       int
	release chan struct{}
}

func (p *stepProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.n++
	ch := make(chan llm.Event, 4)
	switch p.n {
	case 1:
		ch <- llm.Event{Type: llm.EventThinkingDelta, Thinking: "I should list files"}
		ch <- llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "t1", Name: "ls", Args: "{}"}}
		close(ch)
	case 2:
		go func() {
			ch <- llm.Event{Type: llm.EventThinkingDelta, Thinking: "now summarizing"}
			ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "partial ans"}
			<-p.release
			ch <- llm.Event{Type: llm.EventTextDelta, TextDelta: "wer"}
			close(ch)
		}()
	}
	return ch, nil
}

func TestFocusShowsSubagentHistoryLiveAndSteers(t *testing.T) {
	s := testState(t)
	dir := t.TempDir()
	root := agent.New(agent.Config{Provider: &stepProvider{}, Model: "m", Cwd: dir, Tree: s.runtime.Tree})
	s.agent = root
	child := s.runtime.Tree.Child(root.Node, "worker", "opencode-go/glm-5.3", dir)
	p := &stepProvider{release: make(chan struct{})}
	ca := agent.New(agent.Config{Provider: p, Model: "m", Cwd: dir, Tree: s.runtime.Tree, Node: child,
		Tools: tools.NewRegistry(tools.All()...), Agent: child.ID})
	done := make(chan struct{})
	go func() { _, _ = ca.Run(context.Background(), "list and summarize"); close(done) }()

	s.setFocus(child.ID)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(plain(s.vp.GetContent()), "partial ans") {
		if time.Now().After(deadline) {
			t.Fatalf("live partial never shown:\n%s", plain(s.vp.GetContent()))
		}
		time.Sleep(10 * time.Millisecond)
		s.onFrame()
	}
	out := plain(s.vp.GetContent())
	for _, want := range []string{"viewing " + child.ID, "❯ list and summarize", "thought for 1 line", "● ls ✓", "now summarizing"} {
		if !strings.Contains(out, want) {
			t.Errorf("focus view missing %q:\n%s", want, out)
		}
	}
	// Typing while focused goes to the child.
	s.ta.SetValue("also check tests")
	s.keyPress(keyEnter())
	if got := ca.TakeSteered(); len(got) != 1 || got[0] != "also check tests" {
		t.Errorf("steered = %v", got)
	}
	close(p.release)
	<-done
	s.onFrame()
	if out := plain(s.vp.GetContent()); !strings.Contains(out, "partial answer") {
		t.Errorf("final text missing:\n%s", out)
	}
	// esc returns to root.
	s.keyPress(keyEsc())
	if s.focus != nil {
		t.Error("esc did not return to root")
	}
}

func keyEnter() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }
func keyEsc() tea.KeyPressMsg   { return tea.KeyPressMsg{Code: tea.KeyEscape} }

func TestToolProgressShowsLiveTailOnlyWhileRunning(t *testing.T) {
	s := testState(t)
	s.applyEvent(agent.Event{Kind: "tool_start", Call: llm.ToolCall{ID: "1", Name: "bash", Args: `{"command":"make"}`}})
	s.applyEvent(agent.Event{Kind: "tool_progress", Call: llm.ToolCall{ID: "1"}, Output: "compiling a.o\ncompiling b.o"})
	if out := plain(naive(s)); !strings.Contains(out, "compiling b.o") {
		t.Fatalf("tail not shown while running:\n%s", out)
	}
	s.applyEvent(agent.Event{Kind: "tool_done", Call: llm.ToolCall{ID: "1"}, Output: "exit 0\nstdout:\nbuilt"})
	if out := plain(naive(s)); strings.Contains(out, "compiling b.o") {
		t.Fatalf("tail lingers after completion:\n%s", out)
	}
}
