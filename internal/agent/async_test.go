package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

// funcProvider answers each request with script(request number, request).
type funcProvider struct {
	mu     sync.Mutex
	n      int
	reqs   []llm.Request
	script func(n int, req llm.Request) []llm.Event
}

func (p *funcProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.Event, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	evs := p.script(n, req)
	ch := make(chan llm.Event, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func call(id, name, args string) llm.Event {
	return llm.Event{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: id, Name: name, Args: args}}
}
func say(s string) llm.Event { return llm.Event{Type: llm.EventTextDelta, TextDelta: s} }

func spawnArgs(tasks ...string) string {
	var ts []Task
	for _, t := range tasks {
		ts = append(ts, Task{Task: t})
	}
	b, _ := json.Marshal(map[string]any{"tasks": ts})
	return string(b)
}

// gatedChildren finishes child "x" only when release[x] is closed.
type gatedChildren struct {
	release map[string]chan struct{}
	started chan string
}

func (g *gatedChildren) start(ctx context.Context, _ *Agent, t Task) ChildResult {
	g.started <- t.Task
	select {
	case <-g.release[t.Task]:
	case <-ctx.Done():
		return ChildResult{ID: "root/" + t.Task, Status: "cancelled"}
	}
	return ChildResult{ID: "root/" + t.Task, Status: "ok", Report: "report from " + t.Task}
}

func newAsyncRoot(t *testing.T, p llm.Provider, g *gatedChildren, maxTurns int) *Agent {
	return New(Config{Provider: p, Model: "m", MaxTurns: maxTurns, Cwd: t.TempDir(), Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), Spawn: func(context.Context, *Agent, []Task) string { return "sync" },
		StartChild: g.start})
}

func hasText(msgs []llm.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Text, sub) {
			return true
		}
	}
	return false
}

func TestAsyncSpawnParentKeepsWorkingAndGetsResults(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"fast": make(chan struct{}), "slow": make(chan struct{})}, started: make(chan string, 4)}
	close(g.release["fast"])
	p := &funcProvider{}
	var releaseSlow sync.Once
	p.script = func(n int, req llm.Request) []llm.Event {
		switch {
		case n == 1:
			return []llm.Event{call("s", "spawn", spawnArgs("fast", "slow"))}
		case n == 2:
			// The parent is not blocked: it keeps working with its own tools.
			if !hasText(req.Messages, "Started 2 subagent(s) in the background") {
				t.Errorf("spawn did not return at once")
			}
			return []llm.Event{call("l", "ls", "{}")}
		default:
			if hasText(req.Messages, "report from slow") {
				return []llm.Event{say("all done")}
			}
			if hasText(req.Messages, "report from fast") {
				releaseSlow.Do(func() { close(g.release["slow"]) })
			}
			return []llm.Event{say("waiting for the rest")}
		}
	}
	a := newAsyncRoot(t, p, g, 20)
	done := make(chan struct{})
	var final string
	var err error
	go func() { final, err = a.Run(context.Background(), "go"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parent never finished")
	}
	if err != nil || final != "all done" {
		t.Fatalf("final=%q err=%v", final, err)
	}
	if !hasText(a.History(), "[Subagent results: 1 finished") {
		t.Error("results were not injected as a subagent-results message")
	}
	if a.pendingKids() != 0 {
		t.Errorf("pending = %d", a.pendingKids())
	}
}

func TestAsyncFinalAnswerWaitsAndUserMessageWakesParent(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"job": make(chan struct{})}, started: make(chan string, 4)}
	p := &funcProvider{}
	answered := make(chan struct{}, 4)
	p.script = func(n int, req llm.Request) []llm.Event {
		switch {
		case n == 1:
			return []llm.Event{call("s", "spawn", spawnArgs("job"))}
		case hasText(req.Messages, "report from job"):
			return []llm.Event{say("job finished")}
		case hasText(req.Messages[len(req.Messages)-1:], "how is it going"):
			answered <- struct{}{}
			return []llm.Event{say("job still running")}
		default:
			return []llm.Event{say("started the job")}
		}
	}
	a := newAsyncRoot(t, p, g, 20)
	done := make(chan string)
	go func() { f, _ := a.Run(context.Background(), "go"); done <- f }()
	<-g.started
	time.Sleep(100 * time.Millisecond) // the parent has answered and is now waiting
	select {
	case f := <-done:
		t.Fatalf("returned %q while a child was still running", f)
	default:
	}
	a.Steer("how is it going")
	select {
	case <-answered:
	case <-time.After(3 * time.Second):
		t.Fatal("user message did not wake the waiting parent")
	}
	close(g.release["job"])
	select {
	case f := <-done:
		if f != "job finished" {
			t.Fatalf("final = %q", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parent did not finish after the child")
	}
}

func TestWaitAgentsTool(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}, started: make(chan string, 4)}
	close(g.release["a"])
	a := newAsyncRoot(t, &funcProvider{}, g, 5)
	a.startKids(context.Background(), []Task{{Task: "a"}, {Task: "b"}})
	w := waitTool{agent: a}
	out, _ := w.Run(context.Background(), "", json.RawMessage(`{"timeout_s":1}`))
	if !strings.Contains(out, "report from a") || !strings.Contains(out, "1 subagent(s) still running") {
		t.Fatalf("timed wait = %q", out)
	}
	close(g.release["b"])
	out, _ = w.Run(context.Background(), "", json.RawMessage(`{}`))
	if !strings.Contains(out, "report from b") || strings.Contains(out, "still running") {
		t.Fatalf("full wait = %q", out)
	}
}

func TestTurnOutStillCollectsChildren(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"late": make(chan struct{})}, started: make(chan string, 4)}
	p := &funcProvider{}
	p.script = func(n int, req llm.Request) []llm.Event {
		if n == 1 {
			return []llm.Event{call("s", "spawn", spawnArgs("late"))}
		}
		return []llm.Event{call("l"+strings.Repeat("x", n), "ls", "{}")}
	}
	a := newAsyncRoot(t, p, g, 3)
	go func() { <-g.started; time.Sleep(50 * time.Millisecond); close(g.release["late"]) }()
	final, err := a.Run(context.Background(), "go")
	if err == nil || !strings.Contains(final, "[TURN-OUT]") || !strings.Contains(final, "report from late") {
		t.Fatalf("final=%q err=%v", final, err)
	}
}

func TestWaitAgentsReturnsEarlyForUserMessage(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"long": make(chan struct{})}, started: make(chan string, 4)}
	a := newAsyncRoot(t, &funcProvider{}, g, 5)
	a.startKids(context.Background(), []Task{{Task: "long"}})
	go func() { <-g.started; time.Sleep(50 * time.Millisecond); a.Steer("status?") }()
	start := time.Now()
	out, _ := waitTool{agent: a}.Run(context.Background(), "", json.RawMessage(`{"timeout_s":600}`))
	if time.Since(start) > 2*time.Second || !strings.Contains(out, "the user sent you a message") || !strings.Contains(out, "1 subagent(s) still running") {
		t.Fatalf("after %s: %q", time.Since(start), out)
	}
	close(g.release["long"])
}

func TestBlockingSpawnReturnsEarlyForUserMessage(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"long": make(chan struct{})}, started: make(chan string, 4)}
	a := newAsyncRoot(t, &funcProvider{}, g, 5)
	go func() { <-g.started; time.Sleep(50 * time.Millisecond); a.Steer("status?") }()
	out, _ := spawnTool{agent: a}.Run(context.Background(), "", json.RawMessage(`{"tasks":[{"task":"long"}],"wait":true}`))
	if !strings.Contains(out, "the user sent you a message") {
		t.Fatalf("out = %q", out)
	}
	close(g.release["long"])
}

func TestInboxMessagesReachRunningAndWaitingAgents(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	inbox, _ := session.InboxPath("run1", "root")
	g := &gatedChildren{release: map[string]chan struct{}{"job": make(chan struct{})}, started: make(chan string, 4)}
	answered := make(chan struct{}, 4)
	p := &funcProvider{}
	p.script = func(n int, req llm.Request) []llm.Event {
		last := req.Messages[len(req.Messages)-1].Text
		switch {
		case n == 1:
			return []llm.Event{call("s", "spawn", spawnArgs("job"))}
		case strings.Contains(last, "fix the presentation"):
			answered <- struct{}{}
			return []llm.Event{say("will fix")}
		case hasText(req.Messages, "report from job"):
			return []llm.Event{say("done")}
		default:
			return []llm.Event{say("waiting")}
		}
	}
	a := New(Config{Provider: p, Model: "m", MaxTurns: 20, Cwd: t.TempDir(), Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), Spawn: func(context.Context, *Agent, []Task) string { return "" },
		StartChild: g.start, Inbox: inbox})
	done := make(chan string)
	go func() { f, _ := a.Run(context.Background(), "go"); done <- f }()
	<-g.started
	time.Sleep(100 * time.Millisecond) // the parent is now waiting on its child
	if err := session.SendInbox("run1", "root", "fix the presentation of the blocks"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("inbox message never reached the waiting agent")
	}
	close(g.release["job"])
	if f := <-done; f != "done" {
		t.Fatalf("final = %q", f)
	}
	if !hasText(a.History(), "[Message from the user, sent while you were working]\nfix the presentation") {
		t.Error("message not recorded in history")
	}
}

func TestResumeTurnedOutChild(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r := NewRuntime()
	dir := t.TempDir()
	parent := New(Config{Provider: &funcProvider{}, Model: "m", Cwd: dir, Tree: r.Tree})
	node := r.Tree.Child(parent.Node, "scout", "x/m", dir)
	p := &funcProvider{}
	p.script = func(n int, req llm.Request) []llm.Event {
		if hasText(req.Messages[len(req.Messages)-1:], "now finish") {
			return []llm.Event{say("finished with full context")}
		}
		return []llm.Event{call("c"+strings.Repeat("x", n), "ls", "{}")}
	}
	sess, err := session.Create(dir, session.Record{Run: r.RunID, Agent: node.ID, Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	sw := &sessWriter{s: sess}
	child := New(Config{Provider: p, Model: "m", Cwd: dir, Tree: r.Tree, Node: node, Agent: node.ID, MaxTurns: 3,
		Tools: tools.NewRegistry(tools.All()...), Trace: &strings.Builder{},
		OnMessage: func(m llm.Message) { sw.append(session.Record{Type: "message", Message: &m}) }})
	first := r.execChild(context.Background(), parent, child, sw, "research it")
	if first.Status != "turn-out" || !strings.Contains(first.Report, "resume_agent can continue "+node.ID) {
		t.Fatalf("first = %+v", first)
	}
	histBefore := len(child.History())
	if bad := r.ResumeChild(context.Background(), &Agent{cfg: Config{Node: &Node{ID: "root/other"}}}, node.ID, "x", 0); bad.Status != "error" {
		t.Fatalf("a non-parent resumed it: %+v", bad)
	}
	second := r.ResumeChild(context.Background(), parent, node.ID, "now finish", 5)
	if second.Status != "ok" || second.Report != "finished with full context" || second.Turns != 1 || second.MaxTurns != 5 {
		t.Fatalf("second = %+v", second)
	}
	if !hasText(child.History()[:histBefore], "research it") {
		t.Error("history was not kept")
	}
	loaded, err := session.Load(sess.Path())
	if err != nil || !hasText(loaded.Messages, "research it") || !hasText(loaded.Messages, "finished with full context") {
		t.Fatalf("session file lacks one of the runs: %v", err)
	}
}

func TestTimeBudgetBoundsWaitingOnStuckChild(t *testing.T) {
	g := &gatedChildren{release: map[string]chan struct{}{"stuck": make(chan struct{})}, started: make(chan string, 4)}
	var statuses []string
	var mu sync.Mutex
	p := &funcProvider{}
	p.script = func(n int, req llm.Request) []llm.Event {
		if n == 1 {
			return []llm.Event{call("s", "spawn", spawnArgs("stuck"))}
		}
		return []llm.Event{say("report: the scout never came back")}
	}
	a := New(Config{Provider: p, Model: "m", MaxTurns: 20, Cwd: t.TempDir(), Trace: &strings.Builder{},
		Tools: tools.NewRegistry(tools.All()...), Spawn: func(context.Context, *Agent, []Task) string { return "" },
		StartChild: g.start, Deadline: time.Now().Add(300 * time.Millisecond),
		OnStatus: func(s string) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() }})
	done := make(chan string)
	go func() { f, _ := a.Run(context.Background(), "go"); done <- f }()
	select {
	case f := <-done:
		if !strings.Contains(f, "the scout never came back") || !strings.Contains(f, "status: cancelled") {
			t.Fatalf("final = %q", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parent waited forever on a stuck child")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) == 0 || !strings.HasPrefix(statuses[0], "waiting on ") {
		t.Errorf("statuses = %q", statuses)
	}
}
