package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/tools"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSpawnParallelResultsAndDepth(t *testing.T) {
	r := NewRuntime()
	dir := t.TempDir()
	root := New(Config{Provider: &fakeProvider{turns: [][]llm.Event{{{Type: llm.EventTextDelta, TextDelta: "ok"}}}}, Model: "fake", Cwd: dir, Tools: tools.NewRegistry(tools.All()...), Tree: r.Tree, Spawn: func(_ context.Context, a *Agent, tasks []Task) string {
		var wg sync.WaitGroup
		out := make([]string, len(tasks))
		for i, task := range tasks {
			wg.Add(1)
			go func(i int, t Task) { defer wg.Done(); out[i] = t.Task }(i, task)
		}
		wg.Wait()
		return strings.Join(out, ",")
	}})
	if root.ID != "root" || root.Depth != 0 || root.Role != "root" {
		t.Fatalf("root metadata: %s %d %s", root.ID, root.Depth, root.Role)
	}
	tool, ok := root.cfg.Tools.Get("spawn")
	if !ok {
		t.Fatal("root missing spawn")
	}
	raw, _ := json.Marshal(map[string]any{"tasks": []Task{{Task: "first"}, {Task: "second"}}})
	got, e := tool.Run(context.Background(), dir, raw)
	if e != nil || got != "first,second" {
		t.Fatalf("spawn=%q %v", got, e)
	}
	parent := root.cfg.Node
	for i := 0; i < 4; i++ {
		parent = r.Tree.Child(parent, "worker", "m", dir)
	}
	deep := New(Config{Provider: root.cfg.Provider, Model: "m", Cwd: dir, Tools: tools.NewRegistry(), Tree: r.Tree, Node: parent, Spawn: r.Spawn})
	if _, ok := deep.cfg.Tools.Get("spawn"); ok {
		t.Fatal("depth 4 offered spawn")
	}
	if len(r.Tree.Snapshot().Children) != 1 {
		t.Fatal("tree missing child")
	}
}
func TestProviderFallbackOnlyTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{{"HTTP 429 quota exceeded", true}, {"connection reset by peer", true}, {"unauthorized token", true}, {`chat: HTTP 400: {"error":{"param":"reasoning","type":"invalid_request_error","message":"Upstream request failed: [unknown_parameter] invalid request body: json: unknown field \"reasoning\""}}`, true}, {"permission denied", false}, {"poor answer", false}, {"stopped by permission policy", false}} {
		if got := providerFailure(fmt.Errorf("%s", tc.msg)); got != tc.want {
			t.Errorf("providerFailure(%q)=%v", tc.msg, got)
		}
	}
}
func TestSessionPermissionSeparatesChildCWD(t *testing.T) {
	a := perm.NewCall("/tmp/repo-A", "bash", json.RawMessage(`{"command":"echo hello"}`))
	b := perm.NewCall("/tmp/repo-B", "bash", json.RawMessage(`{"command":"echo hello"}`))
	if perm.SessionKey(a) == perm.SessionKey(b) {
		t.Fatal("allow-session permission escaped its cwd")
	}
}
func TestCardsRoutesAndRoleTools(t *testing.T) {
	c, e := readCard("roles", "scout")
	if e != nil {
		t.Fatal(e)
	}
	if contains(c.Tools, "write") || contains(c.Tools, "edit") || c.Prompt == "" {
		t.Fatalf("scout=%+v", c)
	}
	m, e := readCard("models", "gpt-luna")
	if e != nil {
		t.Fatal(e)
	}
	routes := usableRoutes(m.Routes)
	if len(routes) < 1 || routeRef(routes[0]) != "codex/gpt-6-luna" || strings.Contains(strings.Join(routes, ","), "opencode-go:") {
		t.Fatalf("luna routes=%v", routes)
	}
	if routeRef("openrouter:xiaomi/mimo-v2.6-pro") != "openrouter/xiaomi/mimo-v2.6-pro" {
		t.Fatal("slash conversion")
	}
}

func TestChildStatusAndResultFormat(t *testing.T) {
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cases := []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{ctx, nil, "ok"},
		{ctx, &TurnOutError{Turns: 3}, "turn-out"},
		{cancelled, context.Canceled, "cancelled"},
		{ctx, perm.ErrStopped, "error"},
		{ctx, fmt.Errorf("HTTP 500"), "error"},
	}
	for _, c := range cases {
		if got := childStatus(c.ctx, c.err); got != c.want {
			t.Errorf("childStatus(%v) = %s, want %s", c.err, got, c.want)
		}
	}
	out := ChildResult{ID: "root/worker-2", Model: "opencode-go/glm-5.3", Cwd: "/w", Session: "/s/x.jsonl",
		Status: "turn-out", Turns: 80, MaxTurns: 80, Wall: 23*time.Minute + 4*time.Second, Tools: 41, Usage: llm.Usage{In: 9, Out: 2}, Report: "[TURN-OUT] partial"}.String()
	want := "### root/worker-2 · status: turn-out · model: opencode-go/glm-5.3 · turns: 80/80 · tools: 41 · tokens: 9 in/2 out · wall: 23m4s\ncwd: /w\nsession: /s/x.jsonl\n\n[TURN-OUT] partial"
	if out != want {
		t.Errorf("result =\n%s\nwant\n%s", out, want)
	}
	if got := (ChildResult{Status: "error", Report: "spawn error: empty task"}).String(); !strings.Contains(got, "### - · status: error") || !strings.Contains(got, "session: -") {
		t.Errorf("pre-run failure = %q", got)
	}
}

func TestChildResultListsDenials(t *testing.T) {
	out := ChildResult{ID: "root/scout-1", Status: "ok", Report: "done", Denied: []string{"bash {\"command\":\"curl x\"}: no human attached"}}.String()
	if !strings.Contains(out, "denied: 1\n- bash {\"command\":\"curl x\"}: no human attached\n\ndone") {
		t.Errorf("result = %q", out)
	}
}

func TestLiveCapQueuesAndParentsReleaseSlots(t *testing.T) {
	r := NewRuntime()
	r.live = make(chan struct{}, 2)
	var mu sync.Mutex
	running, peak := 0, 0
	// Each fake child takes a slot like startChild does; depth-1 children
	// spawn two grandchildren of their own while holding theirs.
	r.runChild = func(ctx context.Context, parent *Agent, task Task) ChildResult {
		if err := r.acquireLive(ctx); err != nil {
			return ChildResult{Status: "cancelled"}
		}
		active := func(d int) {
			mu.Lock()
			running += d
			if running > peak {
				peak = running
			}
			mu.Unlock()
		}
		active(1)
		child := &Agent{cfg: Config{Node: &Node{Depth: parent.cfg.Node.Depth + 1}}, liveSlot: true}
		report := task.Task
		if task.Task == "lead" {
			active(-1) // Spawn releases the slot while the lead waits…
			report += ":" + r.Spawn(ctx, child, []Task{{Task: "leaf"}, {Task: "leaf"}})
			active(1) // …and takes one back before returning
		}
		active(-1)
		r.releaseLive()
		return ChildResult{Status: "ok", Report: report}
	}
	root := &Agent{cfg: Config{Node: &Node{}}}
	done := make(chan string)
	go func() {
		done <- r.Spawn(context.Background(), root, []Task{{Task: "lead"}, {Task: "lead"}, {Task: "lead"}})
	}()
	select {
	case out := <-done:
		if strings.Count(out, "leaf") != 6 {
			t.Fatalf("out = %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: parents holding slots starved their children")
	}
	if peak > 2 {
		t.Errorf("peak live = %d, cap 2", peak)
	}
	if len(r.live) != 0 {
		t.Errorf("slots leaked: %d", len(r.live))
	}
}

func TestQueuedChildCancelled(t *testing.T) {
	r := NewRuntime()
	r.live = make(chan struct{}, 1)
	r.live <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.acquireLive(ctx); err == nil {
		t.Fatal("acquired a slot from a full tree")
	}
}
