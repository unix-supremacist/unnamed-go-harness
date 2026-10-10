package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/perm"
)

func qs(names ...string) agent.AskRequest {
	r := agent.AskRequest{Agent: "root/1"}
	for _, n := range names {
		r.Questions = append(r.Questions, agent.AskQuestion{Question: n + "?", Header: n, Options: []agent.AskOption{
			{Label: "Alpha", Description: "first"}, {Label: "Beta", Description: "second"}, {Label: "Gamma", Description: "third"}}})
	}
	return r
}

type askResult struct {
	ans agent.AskAnswer
	err error
}

func startAsk(s *State, ctx context.Context, req agent.AskRequest) chan askResult {
	out := make(chan askResult, 1)
	want := s.asks.len() + 1
	go func() {
		a, err := s.askUser(ctx, req)
		out <- askResult{a, err}
	}()
	// wait until it is queued
	for i := 0; i < 2000 && s.asks.len() < want; i++ {
		time.Sleep(time.Millisecond)
	}
	return out
}

func key(s *State, k string) {
	switch k {
	case "enter":
		s.keyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "down":
		s.keyPress(tea.KeyPressMsg{Code: tea.KeyDown})
	case "up":
		s.keyPress(tea.KeyPressMsg{Code: tea.KeyUp})
	case "esc":
		s.keyPress(tea.KeyPressMsg{Code: tea.KeyEscape})
	case "space":
		s.keyPress(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	case "shift+tab":
		s.keyPress(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	default:
		for _, r := range k {
			s.keyPress(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
}

func wait(t *testing.T, ch chan askResult) askResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("ask never completed")
		return askResult{}
	}
}

func TestAskSingleSelectAndWriteIn(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	ch := startAsk(s, context.Background(), qs("One", "Two"))
	key(s, "down")  // Beta
	key(s, "enter") // choose Beta, advance
	// second question: write-in
	key(s, "4") // jump to the Other row (4th)
	key(s, "enter")
	key(s, "my idea")
	key(s, "enter")
	r := wait(t, ch)
	if r.err != nil || r.ans.Declined || len(r.ans.Replies) != 2 {
		t.Fatalf("%+v", r)
	}
	if r.ans.Replies[0].Labels[0] != "Beta" || r.ans.Replies[1].Other != "my idea" || len(r.ans.Replies[1].Labels) != 0 {
		t.Fatalf("replies = %+v", r.ans.Replies)
	}
	if s.asks.len() != 0 {
		t.Fatal("answered item still queued")
	}
	var lines []string
	for _, e := range s.entries {
		lines = append(lines, e.text)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "One? → Beta") || !strings.Contains(got, `other: "my idea"`) {
		t.Fatalf("transcript = %q", got)
	}
}

func TestAskMultiSelectAndBack(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	req := qs("One", "Two")
	req.Questions[0].MultiSelect = true
	ch := startAsk(s, context.Background(), req)
	key(s, "space") // Alpha
	key(s, "down")
	key(s, "down")
	key(s, "space") // Gamma
	key(s, "enter") // confirm → page 2
	key(s, "shift+tab")
	if it := s.asks.head(); it.page != 0 || !it.picked[0][0] || !it.picked[0][2] {
		t.Fatalf("back lost the selection: page=%d picked=%v", it.page, it.picked[0])
	}
	key(s, "enter")
	key(s, "enter") // second question: highlighted row (Alpha)
	r := wait(t, ch)
	if got := r.ans.Replies[0].Labels; len(got) != 2 || got[0] != "Alpha" || got[1] != "Gamma" {
		t.Fatalf("multi = %v", got)
	}
	if r.ans.Replies[1].Labels[0] != "Alpha" {
		t.Fatalf("second = %+v", r.ans.Replies[1])
	}
}

func TestAskFIFOAcrossAgents(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	a := qs("First")
	a.Agent = "root/1"
	b := qs("Second")
	b.Agent = "root/2"
	chA := startAsk(s, context.Background(), a)
	chB := startAsk(s, context.Background(), b)
	if s.asks.head().req.Agent != "root/1" || s.asks.len() != 2 {
		t.Fatalf("queue head = %v", s.asks.head().req.Agent)
	}
	if !strings.Contains(plain(func() string { v, _ := s.askSheet(); return v }()), "root/1 asks (1/1) · 1 more waiting") {
		t.Fatal("header does not show the queue")
	}
	key(s, "enter")
	if r := wait(t, chA); r.ans.Replies[0].Labels[0] != "Alpha" {
		t.Fatalf("A = %+v", r)
	}
	if s.asks.head() == nil || s.asks.head().req.Agent != "root/2" {
		t.Fatal("second request did not come up")
	}
	key(s, "esc")
	if r := wait(t, chB); !r.ans.Declined {
		t.Fatalf("B = %+v", r)
	}
}

func TestAskCancelQueuedAndDisplayed(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	chA := startAsk(s, ctxA, qs("A"))
	chB := startAsk(s, ctxB, qs("B"))
	cancelB() // queued, not displayed
	if r := wait(t, chB); r.err == nil || !r.ans.Declined {
		t.Fatalf("B = %+v", r)
	}
	if s.asks.len() != 1 {
		t.Fatalf("queue = %d", s.asks.len())
	}
	cancelA() // displayed
	if r := wait(t, chA); r.err == nil {
		t.Fatalf("A = %+v", r)
	}
	if s.asks.len() != 0 || s.askActive() != nil {
		t.Fatal("modal still open after cancellation")
	}
	// A late key press after cancellation is harmless.
	key(s, "enter")
}

func TestAskPermissionPromptPreemptsAndRestores(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	ch := startAsk(s, context.Background(), qs("One"))
	key(s, "4")
	key(s, "enter")
	key(s, "half typed")
	it := s.asks.head()
	if !it.typing || it.typed != "half typed" {
		t.Fatalf("setup: %+v", it)
	}
	// A permission request arrives mid write-in.
	s.modal = &perm.UIRequest{Call: perm.Call{Tool: "bash"}, Reply: make(chan perm.Answer, 1)}
	if s.askActive() != nil {
		t.Fatal("ask modal must yield to the permission modal")
	}
	key(s, "y") // goes to the permission modal, never into the write-in
	if it.typed != "half typed" {
		t.Fatalf("keys leaked into the write-in: %q", it.typed)
	}
	if s.modal != nil {
		t.Fatal("permission modal did not take the key")
	}
	if s.askActive() == nil || !it.typing || it.typed != "half typed" {
		t.Fatal("ask modal state was not restored")
	}
	key(s, " and more")
	key(s, "enter")
	if r := wait(t, ch); r.ans.Replies[0].Other != "half typed and more" {
		t.Fatalf("%+v", r)
	}
}

func TestAskMainEditorDraftIsUntouched(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	s.ta.SetValue("my draft")
	ch := startAsk(s, context.Background(), qs("One"))
	key(s, "4")
	key(s, "enter")
	key(s, "typed")
	key(s, "enter")
	wait(t, ch)
	if s.ta.Value() != "my draft" {
		t.Fatalf("draft = %q", s.ta.Value())
	}
}

func TestAskDeclinedOnQuitSwitchAndStaleReplyIgnored(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	ch := startAsk(s, context.Background(), qs("A"))
	it := s.asks.head()
	s.declineAsks("session changed")
	if r := wait(t, ch); !r.ans.Declined {
		t.Fatalf("%+v", r)
	}
	// A stale answer after the switch changes nothing.
	it.complete(agent.AskAnswer{Replies: []agent.AskReply{{Labels: []string{"Alpha"}}}})
	if len(it.reply) != 0 {
		t.Fatal("stale reply was delivered")
	}
	// Quitting declines too.
	ch = startAsk(s, context.Background(), qs("B"))
	s.command("/quit")
	if r := wait(t, ch); !r.ans.Declined {
		t.Fatalf("quit: %+v", r)
	}
	// And so does the UI shutting down.
	ch = startAsk(s, context.Background(), qs("C"))
	close(s.done)
	if r := wait(t, ch); !r.ans.Declined {
		t.Fatalf("shutdown: %+v", r)
	}
}

func TestAskWaitsPastToolTimeoutUnlessConfigured(t *testing.T) {
	s := testState(t)
	s.done = make(chan struct{})
	s.askTimeout = 30 * time.Millisecond
	ch := startAsk(s, context.Background(), qs("Slow"))
	r := wait(t, ch)
	if !r.ans.Declined || r.err != nil {
		t.Fatalf("a timed-out question must be a plain decline: %+v", r)
	}
	if s.asks.len() != 0 {
		t.Fatal("timed-out question still queued")
	}
}

// Program.Send blocks on an unbuffered channel that the event loop itself
// drains, so Update-side code (quit, session switches) must never call it.
// An unstarted program never drains, which makes any such Send hang here.
func TestDeclineAsksNeverBlocksTheEventLoop(t *testing.T) {
	run := func(name string, act func(s *State)) {
		s := sessionState(t)
		s.done = make(chan struct{})
		ch := startAsk(s, context.Background(), qs("A"))
		// Attach the (never-draining) program only now: the tool goroutine's own
		// notification is allowed to block in this test, the UI goroutine's is not.
		s.prog.Store(tea.NewProgram(s))
		fin := make(chan struct{})
		go func() { act(s); close(fin) }()
		select {
		case <-fin:
		case <-time.After(time.Second):
			t.Fatalf("%s hung: the UI goroutine called Program.Send", name)
		}
		if r := wait(t, ch); !r.ans.Declined {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	run("/quit", func(s *State) { s.command("/quit") })
	run("session switch", func(s *State) {
		s.asks.head().complete(agent.AskAnswer{Declined: true}) // answered elsewhere; the switch must still not hang
		s.asks.declineAll()
		_ = s.commitSessionForTest()
	})
	run("ctrl+c twice", func(s *State) {
		s.keyPress(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		s.keyPress(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	})
}

func TestModelThinkingAndCompactModelRefusedWhileRunning(t *testing.T) {
	s := sessionState(t)
	agentBefore, thinking := s.agent, s.thinking
	s.running = true
	s.setThinking("high")
	s.setCompactModel("openrouter/x")
	s.setModel("fake/other")
	if s.agent != agentBefore || s.thinking != thinking || s.modelRef != "fake/m" {
		t.Fatal("agent was replaced under a live run")
	}
}
