package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/unix/ugh/internal/llm"
)

// CompactSystem is the summarizer system prompt shared by /compact and
// automatic compaction.
const CompactSystem = "You summarize coding-agent conversations. Write a compact but complete " +
	"summary of the conversation: the user's requests and their state, key decisions, files " +
	"created or changed and what is in them, important tool results and errors, and what " +
	"remains to be done. Plain text. A later model must be able to continue the work from " +
	"your summary alone without access to the original messages."

// compactKeep is how many trailing messages automatic compaction keeps
// verbatim.
const compactKeep = 6

// replyHeadroom is the part of the window kept free for the model's answer;
// the prompt is never allowed past window − replyHeadroom.
const replyHeadroom = 16000

// defaultTokPerByte is the prompt-size estimate before any usage report
// calibrates it (about 3.5 bytes per token).
const defaultTokPerByte = 1.0 / 3.5

// limitFor is the prompt size (tokens) at which this agent compacts when
// talking to model ref: the minimum of the limits that are defined.
//   - an absolute limit: [compaction.thresholds] for ref, else the model card's
//     compact_at, else [compaction] threshold (0 = none);
//   - auto_compact × window, when the window is known;
//   - window − replyHeadroom as a hard safety limit, when the window is known.
//
// auto_compact = 0 disables automatic compaction entirely. With no limit
// defined (unknown window, no absolute limit) nothing compacts.
func (a *Agent) limitFor(ref string) (int, bool) {
	if a.cfg.OneShot || a.cfg.AutoCompact <= 0 {
		return 0, false
	}
	a.limitMu.Lock()
	defer a.limitMu.Unlock()
	if r, ok := a.limitCache[ref]; ok {
		return r.limit, r.ok
	}
	cc := a.cfg.Context
	limit, have := 0, false
	take := func(n int) {
		if n > 0 && (!have || n < limit) {
			limit, have = n, true
		}
	}
	abs, found := cc.CompactThresholds[ref]
	if !found {
		if n, ok := CompactAt(ref); ok {
			abs, found = n, true
		}
	}
	if !found {
		abs = cc.CompactThreshold
	}
	take(abs)
	if w, ok := a.windowFor(ref); ok && w > 0 {
		take(int(a.cfg.AutoCompact * float64(w)))
		take(w - replyHeadroom)
	}
	if a.limitCache == nil {
		a.limitCache = map[string]limitResult{}
	}
	a.limitCache[ref] = limitResult{limit, have}
	return limit, have
}

// windowFor is the context window of ref; an explicit Config.ContextWindow
// overrides the model's own.
func (a *Agent) windowFor(ref string) (int, bool) {
	if a.cfg.ContextWindow > 0 {
		return a.cfg.ContextWindow, true
	}
	if w, ok := ContextWindow(ref); ok {
		return w, true
	}
	if ref == a.activeModel() { // agents may know only the bare model id
		return ContextWindow(a.cfg.Model)
	}
	return 0, false
}

type limitResult struct {
	limit int
	ok    bool
}

// activeModel is the ref the conversation currently talks to (after failover).
func (a *Agent) activeModel() string {
	if m := a.cfg.Node.modelRef(); m != "" {
		return m
	}
	return a.cfg.Model
}

// requestBytes sizes a request: system prompt, messages and tool declarations.
func requestBytes(system string, msgs []llm.Message, specs []llm.ToolSpec) int {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Text) + len(m.Reasoning) + len(m.Native) + len(m.ToolName) + len(m.ToolCallID)
		for _, c := range m.ToolCalls {
			n += len(c.Name) + len(c.Args) + len(c.ID)
		}
	}
	for _, sp := range specs {
		n += len(sp.Name) + len(sp.Description)
		if b, err := json.Marshal(sp.Parameters); err == nil {
			n += len(b)
		}
	}
	return n
}

// estimateTokens sizes the request that would be sent now, as assembled for
// the model, and converts it to tokens with the ratio calibrated from the last
// reported usage.
func (a *Agent) estimateTokens(msgs []llm.Message, specs []llm.ToolSpec) int {
	ratio := a.tokPerByte
	if ratio <= 0 {
		ratio = defaultTokPerByte
	}
	return int(float64(requestBytes(a.cfg.System, a.assemble(msgs), specs)) * ratio)
}

// ContextUsage reports the latest prompt size and the effective compaction
// limit for the active model; limit is 0 when none is defined. Safe to call
// from the UI goroutine.
func (a *Agent) ContextUsage() (used, limit int) {
	l, _ := a.limitFor(a.activeModel())
	return int(a.lastInPub.Load()), l
}

// maybeCompact summarizes older turns in place when the request about to be
// sent (after the ladder, estimated, not lagging a turn behind) would pass
// the effective limit. The task and the last few messages stay verbatim;
// failures are traced and retried at most twice.
func (a *Agent) maybeCompact(ctx context.Context, specs []llm.ToolSpec) {
	if a.compactFails >= 2 || len(a.hist) < a.compactHold {
		return
	}
	limit, ok := a.limitFor(a.activeModel())
	if !ok {
		return
	}
	if est := a.estimateTokens(a.hist, specs); est >= limit {
		a.compactFor(ctx, a.activeModel(), limit, est, specs)
	}
}

// compactFor compacts the history; ref/limit/est only label the trace.
func (a *Agent) compactFor(ctx context.Context, ref string, limit, est int, specs []llm.ToolSpec) bool {
	cut := len(a.hist) - compactKeep
	for cut > 0 && cut < len(a.hist) && a.hist[cut].Role == llm.RoleTool {
		cut++ // never start the kept tail with an orphaned tool result
	}
	if cut <= 1 {
		return false
	}
	old := a.hist[:cut]
	text, usage, err := a.oneShot(ctx, a.cfg.Context.CompactModel, CompactSystem, CompactPrompt(old))
	a.addUsage(usage)
	if err != nil || strings.TrimSpace(text) == "" {
		a.compactFails++
		fmt.Fprintf(a.cfg.Trace, "🗜 %s: auto-compaction failed: %v\n", a.cfg.Agent, err)
		return false
	}
	seed := llm.Message{Role: llm.RoleUser, Text: "Original task:\n" + a.task +
		"\n\nSummary of earlier work (older turns were compacted automatically; this summary replaces them):\n" +
		strings.TrimSpace(text) + a.instructionTail()}
	fmt.Fprintf(a.cfg.Trace, "🗜 %s: compacted %d messages (~%d of %d tokens on %s)\n", a.cfg.Agent, len(old), est, limit, ref)
	a.hist = append([]llm.Message{seed}, a.hist[cut:]...)
	a.lastIn = 0
	a.Compactions++
	if a.cfg.OnCompact != nil {
		a.cfg.OnCompact(a.hist)
	}
	// A kept tail that is itself over the limit cannot be fixed by compacting
	// again next turn; wait for the history to grow before trying.
	if a.estimateTokens(a.hist, specs) >= limit {
		a.compactHold = len(a.hist) + compactKeep
	} else {
		a.compactHold = 0
	}
	return true
}

// ApplyCompaction replaces the live history with seed (manual /compact) and
// records it as a compaction marker in the same session file. The archive is
// untouched, so recall still reaches everything the summary replaced. Call it
// only while the agent is idle. summarizer is the usage of the call that
// produced the seed, accounted once here.
func (a *Agent) ApplyCompaction(seed []llm.Message, summarizer llm.Usage) {
	a.addUsage(summarizer)
	a.hist = append([]llm.Message(nil), seed...)
	if tail := a.instructionTail(); tail != "" && len(a.hist) > 0 && a.hist[0].Role == llm.RoleUser {
		a.hist[0].Text += tail
	}
	a.lastIn = 0
	a.Compactions++
	if a.cfg.OnCompact != nil {
		a.cfg.OnCompact(a.hist)
	}
}

// CompactPrompt serializes the history for the summarizer: role-tagged text
// with tool calls collapsed to one line each (tool results keep their text,
// truncated).
func CompactPrompt(hist []llm.Message) string {
	var b strings.Builder
	b.WriteString("Summarize this conversation:\n\n")
	for _, m := range hist {
		switch m.Role {
		case llm.RoleUser:
			b.WriteString("User: " + oneFocus(m.Text) + "\n")
		case llm.RoleAssistant:
			b.WriteString("Assistant: " + oneFocus(m.Text) + "\n")
			for _, c := range m.ToolCalls {
				b.WriteString("  [tool call " + c.Name + ": " + oneFocus(c.Args) + "]\n")
			}
		case llm.RoleTool:
			b.WriteString("  [result " + m.ToolName + ": " + oneFocus(m.Text) + "]\n")
		}
	}
	return b.String()
}

// oneFocus trims each message for the summary prompt while keeping enough
// substance for recall after compaction.
func oneFocus(t string) string {
	t = strings.TrimSpace(t)
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > 800 {
		t = t[:800] + "…"
	}
	return t
}
