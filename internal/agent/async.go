package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/unix/ugh/internal/llm"
)

// Background subagents: spawn returns at once and the parent keeps working.
// Finished children's results are injected into the parent's next turn; a
// parent that gives its final answer while children still run waits for
// them (or for a user message) instead of ending, so no result is lost.

// childRun is one background child of an agent.
type childRun struct {
	label string
	done  chan struct{}
	res   ChildResult
}

func (c *childRun) finished() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// startKids launches tasks in the background through the runtime.
func (a *Agent) startKids(ctx context.Context, tasks []Task) []*childRun {
	var out []*childRun
	for i, t := range tasks {
		t := t
		out = append(out, a.launch(ctx, fmt.Sprintf("task %d: %s", len(a.kidsSnapshot())+i+1, summarize(t.Task, 80)),
			func(ctx context.Context) ChildResult { return a.cfg.StartChild(ctx, a, t) }))
	}
	return out
}

// launch runs one background child job and tracks it until delivered.
func (a *Agent) launch(ctx context.Context, label string, run func(context.Context) ChildResult) *childRun {
	c := &childRun{label: label, done: make(chan struct{})}
	a.kidsMu.Lock()
	if a.kidsCancel == nil {
		a.kidsCtx, a.kidsCancel = context.WithCancel(ctx)
	}
	ctx = a.kidsCtx
	a.kids = append(a.kids, c)
	a.kidsMu.Unlock()
	go func() {
		c.res = run(ctx)
		close(c.done)
		a.signal()
	}()
	return c
}

type resumeTool struct{ agent *Agent }

func (resumeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "resume_agent", Description: "Continue one of your finished subagents (typically a turn-out) " +
		"in the background with its full history and a fresh turn budget, instead of spawning a new one that re-reads " +
		"everything. Its result arrives like a spawn result.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"id":        map[string]any{"type": "string", "description": "the subagent's tree id, e.g. root/worker-1/scout-7"},
			"task":      map[string]any{"type": "string", "description": "what to do next (default: continue where it stopped)"},
			"max_turns": map[string]any{"type": "integer", "description": "turn budget for this continuation"},
		}, "required": []string{"id"}}}
}

func (rt resumeTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var req struct {
		ID       string `json:"id"`
		Task     string `json:"task"`
		MaxTurns int    `json:"max_turns"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", err
	}
	a := rt.agent
	a.launch(ctx, "resume "+req.ID, func(ctx context.Context) ChildResult {
		return a.cfg.Resume(ctx, a, req.ID, req.Task, req.MaxTurns)
	})
	return fmt.Sprintf("Resuming %s in the background; its result arrives automatically (agent_status shows progress).", req.ID), nil
}

func (a *Agent) kidsSnapshot() []*childRun {
	a.kidsMu.Lock()
	defer a.kidsMu.Unlock()
	return append([]*childRun(nil), a.kids...)
}

// pendingKids counts children whose results the parent has not seen yet.
func (a *Agent) pendingKids() int {
	a.kidsMu.Lock()
	defer a.kidsMu.Unlock()
	return len(a.kids)
}

// collectFinished removes finished children and returns their results.
func (a *Agent) collectFinished() []ChildResult {
	a.kidsMu.Lock()
	defer a.kidsMu.Unlock()
	var done []ChildResult
	keep := a.kids[:0]
	for _, c := range a.kids {
		if c.finished() {
			done = append(done, c.res)
		} else {
			keep = append(keep, c)
		}
	}
	a.kids = keep
	return done
}

// signal wakes a parent blocked waiting on its children or the user.
func (a *Agent) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// block waits for a child to finish, a user message, timeout or ctx. The
// agent gives up its live slot meanwhile so its own children can run.
func (a *Agent) block(ctx context.Context, timeout time.Duration) {
	prev := a.cfg.Node.Status
	a.cfg.Node.setStatus(fmt.Sprintf("waiting on %d subagent(s)", a.pendingKids()))
	defer a.cfg.Node.setStatus(prev)
	if a.cfg.OnStatus != nil {
		ids := a.cfg.Tree.RunningChildren(a.cfg.Node)
		a.cfg.OnStatus("waiting on " + strings.Join(ids, ", "))
		defer a.cfg.OnStatus("")
	}
	// A wall-clock budget also bounds waiting on children.
	if !a.cfg.Deadline.IsZero() {
		if left := time.Until(a.cfg.Deadline); timeout <= 0 || left < timeout {
			timeout = max(left, time.Millisecond)
		}
	}
	if a.liveSlot && a.rt != nil {
		a.rt.releaseLive()
		defer func() { _ = a.rt.acquireLive(context.WithoutCancel(ctx)) }()
	}
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	// `ugh send` writes a file, not a channel: poll it while waiting.
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.wake:
			return
		case <-ctx.Done():
			return
		case <-timer:
			return
		case <-tick.C:
			if a.pollInbox() {
				return
			}
		}
	}
}

// formatResults renders delivered child results for the parent.
func formatResults(rs []ChildResult) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}
	return strings.Join(parts, "\n\n")
}

// deliverKids pushes finished children's results into the history.
func (a *Agent) deliverKids() {
	if rs := a.collectFinished(); len(rs) > 0 {
		left := a.pendingKids()
		a.push(llm.Message{Role: llm.RoleUser, Text: fmt.Sprintf(
			"[Subagent results: %d finished, %d still running]\n\n%s", len(rs), left, formatResults(rs))})
	}
}

// drainKids waits for every remaining child and returns their results; used
// when a run ends early (turn-out, error) so children are never orphaned.
func (a *Agent) drainKids(ctx context.Context) string {
	var all []ChildResult
	for a.pendingKids() > 0 {
		all = append(all, a.collectFinished()...)
		if a.pendingKids() > 0 {
			a.block(ctx, time.Second)
		}
		if ctx.Err() != nil {
			// Children share ctx and finish as cancelled promptly.
			for _, c := range a.kidsSnapshot() {
				<-c.done
			}
		}
	}
	return formatResults(all)
}

// ---- tools ----

type waitTool struct{ agent *Agent }

func (waitTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "wait_agents", Description: "Block until your background subagents finish (or timeout_s passes, default 600) and return their results. Returns early when the user sends you a message. Only use it when you have nothing else useful to do; finished results also arrive automatically.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"timeout_s": map[string]any{"type": "integer", "description": "max seconds to wait"},
			"all":       map[string]any{"type": "boolean", "description": "wait for all subagents (default) instead of the first to finish"},
		}}}
}

func (w waitTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var req struct {
		TimeoutS int   `json:"timeout_s"`
		All      *bool `json:"all"`
	}
	_ = json.Unmarshal(raw, &req)
	if req.TimeoutS <= 0 {
		req.TimeoutS = 600
	}
	all := req.All == nil || *req.All
	a := w.agent
	if a.pendingKids() == 0 {
		return "no subagents are running", nil
	}
	deadline := time.Now().Add(time.Duration(req.TimeoutS) * time.Second)
	var got []ChildResult
	for {
		got = append(got, a.collectFinished()...)
		pastBudget := !a.cfg.Deadline.IsZero() && time.Now().After(a.cfg.Deadline)
		if a.pendingKids() == 0 || (!all && len(got) > 0) || time.Now().After(deadline) || ctx.Err() != nil || a.hasSteered() || pastBudget {
			break
		}
		a.block(ctx, time.Until(deadline))
	}
	out := formatResults(got)
	if a.hasSteered() {
		out += "\n\nStopped waiting: the user sent you a message (it follows). Answer it, then call wait_agents again if you still need to wait."
	}
	if n := a.pendingKids(); n > 0 {
		out += fmt.Sprintf("\n\n%d subagent(s) still running; their results will arrive automatically.", n)
	}
	if len(got) == 0 {
		return strings.TrimSpace(out) + " (none finished yet)", nil
	}
	return out, nil
}

type statusTool struct{ agent *Agent }

func (statusTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "agent_status", Description: "Show the live status of your subagents (and their subagents) without waiting: model, status, tokens and latest step. Also returns any results that have finished.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}

func (s statusTool) Run(context.Context, string, json.RawMessage) (string, error) {
	a := s.agent
	var b strings.Builder
	b.WriteString(a.cfg.Tree.SubtreeReport(a.cfg.Node))
	if rs := a.collectFinished(); len(rs) > 0 {
		b.WriteString("\nFinished results:\n\n" + formatResults(rs))
	}
	fmt.Fprintf(&b, "\n%d subagent(s) still running.", a.pendingKids())
	return b.String(), nil
}

// cancelKids stops every outstanding child (they finish as cancelled).
func (a *Agent) cancelKids() {
	a.kidsMu.Lock()
	cancel := a.kidsCancel
	a.kidsCtx, a.kidsCancel = nil, nil
	a.kidsMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
