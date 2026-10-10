package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/unix/ugh/internal/llm"
)

// oneShot runs a single tool-less model call for the harness itself (the
// compaction summarizer, digests). The child agent is OneShot by construction:
// no tools, no ladder, no instructions, no compaction, one turn.
//
// ref picks the model; "" means this agent's conversation model. A ref that
// cannot be resolved falls back to the conversation model (traced); a runtime
// failure of the chosen model is returned, never silently retried on a pricier
// one. The call goes through the same stream limit and admission as the
// conversation, and its usage (including cost) is returned for the caller to
// account once.
func (a *Agent) oneShot(ctx context.Context, ref, system, prompt string) (string, llm.Usage, error) {
	cfg := Config{
		Provider: a.cfg.Provider, Model: a.cfg.Model, System: system, Thinking: a.cfg.Thinking,
		Cwd: a.cfg.Cwd, MaxTurns: 1, ProviderName: a.cfg.ProviderName,
		Admission: a.cfg.Admission, StreamLimit: a.cfg.StreamLimit, OneShot: true,
		Agent: a.cfg.Agent + "/oneshot", Trace: a.cfg.Trace,
	}
	if ref != "" {
		resolved, err := CardRef(ref)
		var prov llm.Provider
		var model string
		if err == nil {
			prov, model, err = resolveRoute(resolved)
		}
		if err != nil {
			fmt.Fprintf(a.cfg.Trace, "🗜 %s: summarizer %s unavailable (%v); using the conversation model\n", a.cfg.Agent, ref, err)
		} else {
			provName, _, _ := strings.Cut(resolved, "/")
			cfg.Provider, cfg.Model, cfg.ProviderName = prov, model, provName
			cfg.Thinking = ClampThinking(resolved, a.thinkingWant)
		}
	}
	one := New(cfg)
	text, err := one.Run(ctx, prompt)
	return text, one.Usage, err
}

// addUsage folds a helper call's usage into this agent's total.
func (a *Agent) addUsage(u llm.Usage) {
	a.Usage.In += u.In
	a.Usage.Out += u.Out
	a.Usage.Cached += u.Cached
	a.Usage.Cost += u.Cost
	a.cfg.Node.setUsage(a.Usage)
}

// Summarize asks the compaction model for a summary of hist. It does not touch
// the agent's own usage (the caller may be on another goroutine); pass the
// returned usage to ApplyCompaction.
func (a *Agent) Summarize(ctx context.Context, hist []llm.Message) (string, llm.Usage, error) {
	return a.oneShot(ctx, a.cfg.Context.CompactModel, CompactSystem, CompactPrompt(hist))
}

// SetCompactModel changes the summarizer for this agent ("" = conversation model).
func (a *Agent) SetCompactModel(ref string) { a.cfg.Context.CompactModel = ref }
