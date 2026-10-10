package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/perm/model"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

// fakeJev answers ladder questions from a script and records what it was sent.
type fakeJev struct {
	mu     sync.Mutex
	states []string // JSON of each request's state
	asked  [][]string
	choose func(call int, alias string) string // "" = leave the question unanswered
	cost   model.Cost
	err    error
	calls  int
}

func (f *fakeJev) AskCosted(_ context.Context, state any, qs map[string]model.Question) (map[string]model.Result, model.Cost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	b, _ := json.Marshal(state)
	f.states = append(f.states, string(b))
	var names []string
	for n := range qs {
		names = append(names, n)
	}
	f.asked = append(f.asked, names)
	if f.err != nil {
		return nil, f.cost, f.err
	}
	out := map[string]model.Result{}
	for n := range qs {
		c := "full"
		if f.choose != nil {
			c = f.choose(f.calls, n)
		}
		if c == "" {
			continue
		}
		out[n] = model.Result{Answer: c, Confidence: 1}
	}
	return out, f.cost, nil
}

func bigText(tag string, lines int) string {
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "%s line %d of the output\n", tag, i)
	}
	return b.String()
}

// ladderHistory builds n big tool results (c1..cn) followed by a few small
// messages, with the matching archive records.
func ladderHistory(n int) ([]llm.Message, []session.Record) {
	var hist []llm.Message
	hist = append(hist, llm.Message{Role: llm.RoleUser, Text: "first task"})
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("call_%d", i) // providers reuse ids freely; ordinals are what count
		hist = append(hist,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "bash", Args: fmt.Sprintf(`{"command":"run step %d"}`, i)}}},
			llm.Message{Role: llm.RoleTool, ToolCallID: id, ToolName: "bash", Ord: i, Text: bigText(fmt.Sprintf("OUT%d", i), 120)})
	}
	hist = append(hist, llm.Message{Role: llm.RoleAssistant, Text: "ok"})
	var disp []session.Record
	for i := range hist {
		disp = append(disp, session.Record{Type: "message", Message: &hist[i]})
	}
	return hist, disp
}

func ladderAgent(t *testing.T, fp llm.Provider, jev model.CostAsker, n int, mut func(*Config)) *Agent {
	t.Helper()
	hist, disp := ladderHistory(n)
	cfg := Config{Provider: fp, Model: "m", Cwd: t.TempDir(), History: hist, Archive: disp, Trace: &strings.Builder{},
		Tools:   tools.NewRegistry(tools.All()...),
		Context: ContextConfig{Ladder: "jev", KeepRecent: 2, MinChunkBytes: 2048, MinSavings: 1000, Jev: jev}}
	if mut != nil {
		mut(&cfg)
	}
	return New(cfg)
}

func answer(text string) []llm.Event {
	return []llm.Event{{Type: llm.EventTextDelta, TextDelta: text}, {Type: llm.EventDone, Stop: "stop"}}
}

func toolMsgs(req llm.Request) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role == llm.RoleTool {
			out = append(out, m.Text)
		}
	}
	return out
}

func TestShortViewIsBoundedAndNeverOverlaps(t *testing.T) {
	e := archived{Ord: 7, Tool: "grep", Args: `{"pattern":"foo"}`}
	// Under 30 lines: shown once, no overlap, no omission note.
	text := bigText("S", 29)
	got := renderShort(e, text)
	if strings.Count(got, "S line 15 of") != 1 || strings.Contains(got, "omitted") {
		t.Fatalf("short view of 29 lines:\n%s", got)
	}
	// Long output: first 15, last 15, the gap counted.
	got = renderShort(e, bigText("L", 800))
	for _, want := range []string{"L line 1 of", "L line 15 of", "L line 786 of", "L line 800 of", "770 lines omitted", `recall("c7"`, `grep "foo"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "L line 16 of") || strings.Contains(got, "L line 785 of") {
		t.Error("short view leaked the middle")
	}
	// Enormous lines are capped and the total is bounded.
	huge := strings.Repeat(strings.Repeat("x", 5000)+"\n", 100)
	if got = renderShort(e, huge); len(got) > shortTotal+600 {
		t.Fatalf("short view is %d bytes", len(got))
	}
	if h := renderHidden(e, 24<<10); h != `[c7: grep "foo" · 24 KiB · hidden as not relevant to this request · recall("c7")]` {
		t.Fatalf("hidden placeholder = %q", h)
	}
}

func TestLadderHidesAndShortensForTheRequestOnly(t *testing.T) {
	jev := &fakeJev{choose: func(_ int, alias string) string {
		return map[string]string{"c1": "hide", "c2": "short", "c3": "full", "c4": "full"}[alias]
	}}
	fp := &fakeProvider{turns: [][]llm.Event{answer("done")}}
	a := ladderAgent(t, fp, jev, 4, nil)
	if _, err := a.Run(context.Background(), "now do the second task"); err != nil {
		t.Fatal(err)
	}
	got := toolMsgs(fp.requests[0])
	if len(got) != 4 {
		t.Fatalf("tool results in request: %d", len(got))
	}
	if !strings.HasPrefix(got[0], "[c1: bash run step 1") || !strings.Contains(got[0], "hidden as not relevant") {
		t.Errorf("c1 = %q", got[0])
	}
	if !strings.Contains(got[1], "short view") || !strings.Contains(got[1], "OUT2 line 1 of") || strings.Contains(got[1], "OUT2 line 60 of") {
		t.Errorf("c2 = %q", got[1])
	}
	// c3 is inside... c3 and c4 stay full by the model's answer.
	if got[2] != bigText("OUT3", 120) {
		t.Error("c3 changed")
	}
	// The stored history is never rewritten, and the archive has the originals.
	if a.hist[2].Text != bigText("OUT1", 120) {
		t.Error("history was rewritten")
	}
	if e, _ := a.arch.get(1); e.Text != bigText("OUT1", 120) {
		t.Error("archive lost the original")
	}
	// The recall tool is offered because the ladder is on.
	have := map[string]bool{}
	for _, s := range fp.requests[0].Tools {
		have[s.Name] = true
	}
	if !have["recall"] {
		t.Error("recall not offered")
	}
	// Every tool message kept its slot: pairing is intact.
	if r := reconcileHistory(a.History()); len(r) != len(a.History()) {
		t.Error("history no longer pairs")
	}
}

// A second query that needs the evidence hidden for the first sees it in full.
func TestEachQueryStartsAllFull(t *testing.T) {
	jev := &fakeJev{choose: func(call int, alias string) string {
		if call == 1 {
			return "hide"
		}
		return "full"
	}}
	fp := &fakeProvider{turns: [][]llm.Event{answer("one"), answer("two")}}
	a := ladderAgent(t, fp, jev, 3, nil)
	for _, q := range []string{"first query", "second query"} {
		if _, err := a.Run(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	first, second := toolMsgs(fp.requests[0]), toolMsgs(fp.requests[1])
	if !strings.Contains(first[0], "hidden") {
		t.Fatalf("first query: %q", first[0])
	}
	if second[0] != bigText("OUT1", 120) {
		t.Fatalf("second query still sees the old view: %q", second[0])
	}
	// The view is a function of the query: the second Jev state carries it.
	if !strings.Contains(jev.states[1], "second query") {
		t.Fatalf("second state lacks the new query: %s", jev.states[1])
	}
}

// Within one query the view is frozen across tool-loop turns, a recall leaves
// the earlier assembled prefix byte-identical, and a mid-loop steer re-decides.
func TestViewFrozenWithinQueryRecallKeepsPrefixAndSteerRedecides(t *testing.T) {
	jev := &fakeJev{choose: func(call int, alias string) string {
		if call == 1 && alias == "c1" {
			return "hide"
		}
		return "full"
	}}
	var ag *Agent
	fp := &fakeProvider{turns: [][]llm.Event{
		{{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "r1", Name: "recall", Args: `{"id":"c1","start_line":1}`}}, {Type: llm.EventDone, Stop: "tool_calls"}},
		{{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "r2", Name: "recall", Args: `{"id":"c1","grep":"line 7 "}`}}, {Type: llm.EventDone, Stop: "tool_calls"}},
		answer("done"),
	}}
	a := ladderAgent(t, fp, jev, 3, nil)
	ag = a
	_ = ag
	if _, err := a.Run(context.Background(), "find the thing"); err != nil {
		t.Fatal(err)
	}
	if jev.calls != 1 {
		t.Fatalf("Jev was asked %d times within one query", jev.calls)
	}
	r0, r1, r2 := fp.requests[0].Messages, fp.requests[1].Messages, fp.requests[2].Messages
	for i, prev := range [][]llm.Message{r0, r1} {
		next := [][]llm.Message{r1, r2}[i]
		for j := range prev {
			a, _ := json.Marshal(prev[j])
			b, _ := json.Marshal(next[j])
			if string(a) != string(b) {
				t.Fatalf("request %d→%d changed message %d:\n%s\n%s", i, i+1, j, a, b)
			}
		}
	}
	// The recall result is a new tool result carrying the exact original text.
	var recalled string
	for _, m := range r1 {
		if m.ToolCallID == "r1" {
			recalled = m.Text
		}
	}
	if !strings.Contains(recalled, "OUT1 line 1 of the output") || !strings.Contains(recalled, "OUT1 line 120 of the output") {
		t.Fatalf("recall result = %.200q", recalled)
	}
	// A steer is a new query.
	fp2 := &fakeProvider{turns: [][]llm.Event{answer("a")}}
	jev2 := &fakeJev{}
	b := ladderAgent(t, fp2, jev2, 3, nil)
	b.Steer("an extra message") // read at the first turn: pushed, query changes
	if _, err := b.Run(context.Background(), "original"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jev2.states[0], "an extra message") {
		t.Fatalf("steer did not become the query: %s", jev2.states[0])
	}
}

func TestLadderFailsOpen(t *testing.T) {
	cases := map[string]*fakeJev{
		"error":     {err: errors.New("boom")},
		"malformed": {choose: func(int, string) string { return "" }}, // no answers at all
		"garbage":   {choose: func(int, string) string { return "obliterate" }},
	}
	for name, jev := range cases {
		fp := &fakeProvider{turns: [][]llm.Event{answer("ok")}}
		a := ladderAgent(t, fp, jev, 3, nil)
		if _, err := a.Run(context.Background(), "q"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, got := range toolMsgs(fp.requests[0]) {
			if got != bigText(fmt.Sprintf("OUT%d", i+1), 120) {
				t.Errorf("%s: chunk %d not full", name, i+1)
			}
		}
	}
	// Below min_savings there is no call at all.
	jev := &fakeJev{}
	fp := &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	a := ladderAgent(t, fp, jev, 3, func(c *Config) { c.Context.MinSavings = 1 << 30 })
	a.Run(context.Background(), "q")
	if jev.calls != 0 {
		t.Fatalf("asked %d times despite nothing to save", jev.calls)
	}
	// Small sessions make no call either; ladder=off never does.
	jev = &fakeJev{}
	fp = &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	a = ladderAgent(t, fp, jev, 3, func(c *Config) { c.Context.Ladder = "off" })
	a.Run(context.Background(), "q")
	if _, ok := a.cfg.Tools.Get("recall"); ok || jev.calls != 0 {
		t.Fatal("ladder off still active")
	}
}

func TestLadderBudgetsAndCost(t *testing.T) {
	// 20 chunks: at most 8 questions per request, largest first, nothing over the cap asked.
	jev := &fakeJev{cost: model.Cost{USD: 0.0002, Known: true}, choose: func(int, string) string { return "short" }}
	fp := &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	a := ladderAgent(t, fp, jev, 30, nil)
	if _, err := a.Run(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if jev.calls != 3 {
		t.Fatalf("requests = %d, want 3 (24 questions max)", jev.calls)
	}
	for _, names := range jev.asked {
		if len(names) > 8 {
			t.Fatalf("%d questions in one request", len(names))
		}
	}
	for _, s := range jev.states {
		if len(s) > 16<<10 {
			t.Fatalf("state is %d bytes", len(s))
		}
	}
	// 3 requests at $0.0002 are billed once each, once.
	if got := a.Usage.Cost; got < 0.00059 || got > 0.00061 {
		t.Fatalf("cost = %v", got)
	}
	if !a.lastLadder.Cost.Known {
		t.Fatal("cost should be known")
	}
	// Chunks beyond the budget stay full: 29 eligible, 24 asked.
	short := 0
	for _, m := range toolMsgs(fp.requests[0]) {
		if strings.Contains(m, "short view") {
			short++
		}
	}
	if short != 24 {
		t.Fatalf("shortened %d chunks, want 24", short)
	}
	// An endpoint that does not report cost shows unknown.
	jev = &fakeJev{cost: model.Cost{}, choose: func(int, string) string { return "hide" }}
	fp = &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	var tr strings.Builder
	b := ladderAgent(t, fp, jev, 3, func(c *Config) { c.Trace = &tr })
	b.Run(context.Background(), "q")
	if b.lastLadder.Cost.Known || !strings.Contains(tr.String(), "$?") {
		t.Fatalf("unknown cost not shown as unknown: %q", tr.String())
	}
	// Retries and failures add up once.
	jev = &fakeJev{cost: model.Cost{USD: 0.001, Known: true}, err: errors.New("down")}
	fp = &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	c := ladderAgent(t, fp, jev, 3, nil)
	c.Run(context.Background(), "q")
	if got := c.Usage.Cost; got < 0.00099 || got > 0.00101 {
		t.Fatalf("failed call cost = %v", got)
	}
}

func TestLadderScrubsSecretsBeforeSending(t *testing.T) {
	const secret = "sk-live-TOP-SECRET-VALUE-123"
	perm.RegisterSecret(secret)
	hist, disp := ladderHistory(3)
	hist[2].Text = "token=" + secret + "\n" + hist[2].Text
	hist[1].ToolCalls[0].Args = `{"command":"curl -H 'Authorization: Bearer ` + secret + `'"}`
	disp[1] = session.Record{Type: "message", Message: &hist[1]}
	disp[2] = session.Record{Type: "message", Message: &hist[2]}
	jev := &fakeJev{}
	fp := &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	a := New(Config{Provider: fp, Model: "m", Cwd: t.TempDir(), History: hist, Archive: disp, Trace: &strings.Builder{},
		Context: ContextConfig{Ladder: "jev", KeepRecent: 2, MinChunkBytes: 2048, MinSavings: 1000, Jev: jev}})
	if _, err := a.Run(context.Background(), "use "+secret+" to deploy"); err != nil {
		t.Fatal(err)
	}
	if jev.calls == 0 {
		t.Fatal("no Jev call to inspect")
	}
	for _, s := range jev.states {
		if strings.Contains(s, secret) {
			t.Fatalf("secret reached the decisions endpoint: %s", s)
		}
	}
}

// Hide a chunk → auto-compact → /compact → restart → recall returns the exact original bytes.
func TestRecallSurvivesCompactionAndRestart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	sess, err := session.Create(dir, session.Record{Model: "m", Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	original := bigText("PRECIOUS", 200)
	blobs := session.BlobDir(sess.Path())
	rec := func(m llm.Message) { _ = sess.Append(session.Record{Type: "message", Message: &m}) }
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	jev := &fakeJev{choose: func(int, string) string { return "hide" }}
	script := [][]llm.Event{
		{{Type: llm.EventToolCall, Call: &llm.ToolCall{ID: "x", Name: "bash", Args: `{"command":"cat big.txt"}`}}, {Type: llm.EventDone, Stop: "tool_calls"}},
		answer("done reading"),
	}
	fp := &fakeProvider{turns: script}
	a := New(Config{Provider: fp, Model: "m", Cwd: dir, Trace: &strings.Builder{}, OnMessage: rec, BlobDir: blobs,
		Tools:     tools.NewRegistry(tools.All()...),
		OnCompact: func(h []llm.Message) { _ = sess.Append(session.Record{Type: "compaction", Seed: h}) },
		Context:   ContextConfig{Ladder: "jev", KeepRecent: 1, MinChunkBytes: 2048, MinSavings: 1, Jev: jev}})
	if _, err := a.Run(context.Background(), "produce output"); err != nil {
		t.Fatal(err)
	}
	// Manual compaction in place, then more work, then "restart".
	a.ApplyCompaction([]llm.Message{{Role: llm.RoleUser, Text: "SUMMARY of everything"}}, llm.Usage{})
	sess.Close()
	loaded, err := session.Load(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || len(loaded.Display) < 4 {
		t.Fatalf("messages=%d display=%d", len(loaded.Messages), len(loaded.Display))
	}
	b := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: dir, History: loaded.Messages, Archive: loaded.Display, BlobDir: blobs,
		Tools: tools.NewRegistry(tools.All()...), Context: ContextConfig{Ladder: "jev"}})
	out, err := b.recall("c1", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PRECIOUS line 1 of the output") || !strings.Contains(out, "PRECIOUS line 200 of the output") {
		t.Fatalf("recall after compaction+restart lost bytes: %.300q", out)
	}
	if _, err := os.Stat(filepath.Join(blobs)); err == nil {
		// the output was small enough not to need a blob; nothing to assert beyond recall
	}
	if _, err := b.recall("c99", 1, ""); err == nil || !strings.Contains(err.Error(), "c1") {
		t.Fatalf("unknown id error = %v", err)
	}
}

func TestRecallPagingGrepAndEnormousLines(t *testing.T) {
	a := New(Config{Provider: &fakeProvider{}, Model: "m", Cwd: t.TempDir(), Context: ContextConfig{Ladder: "jev"}})
	long := bigText("P", 3000)
	a.arch.add(llm.Message{Role: llm.RoleTool, ToolCallID: "i", ToolName: "grep", Text: long}, "")
	page, err := a.recall("c1", 1, "")
	if err != nil || len(page) > recallCap+200 || !strings.Contains(page, `recall("c1", `) {
		t.Fatalf("page: %v len=%d tail=%.120q", err, len(page), page[max(0, len(page)-120):])
	}
	// Paging continues exactly where it stopped.
	var next int
	fmt.Sscanf(page[strings.LastIndex(page, `recall("c1", `):], `recall("c1", %d)`, &next)
	p2, err := a.recall("c1", next, "")
	if err != nil || !strings.Contains(p2, fmt.Sprintf("P line %d of", next)) {
		t.Fatalf("page 2 should start at line %d: %.120q", next, p2)
	}
	g, err := a.recall("c1", 0, "line 2999 ")
	if err != nil || !strings.Contains(g, "2999: P line 2999 of") || strings.Contains(g, "line 3000") {
		t.Fatalf("grep: %v %q", err, g)
	}
	if _, err := a.recall("c1", 9999, ""); err == nil {
		t.Fatal("start past the end should fail")
	}
	a.arch.add(llm.Message{Role: llm.RoleTool, ToolCallID: "j", ToolName: "bash", Text: strings.Repeat("z", 100<<10)}, "")
	big, err := a.recall("c2", 1, "")
	if err != nil || len(big) > recallCap+300 || !strings.Contains(big, "only its first part is shown") {
		t.Fatalf("one huge line: %v len=%d", err, len(big))
	}
}
