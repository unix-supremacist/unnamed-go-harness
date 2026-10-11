package perm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Engine runs the classifiers for one call, applies the policy, prompts via
// the prompter when needed (stdin broker or TUI channel), and logs every
// verdict plus the final decision.
type Engine struct {
	Policy      Policy
	Prompter    Prompter
	Log         *DecisionLog
	classifiers []Classifier

	// Output screens each tool result for agent-directed instructions (§4.3).
	// It may be nil (no output check).
	Output OutputJudge
	// Notify, when set, receives every completed classification so a UI can
	// mark calls whose verdicts were not shown in a prompt (auto mode,
	// log-state classifiers).
	Notify func(c Call, vs []Verdict)

	// Triage assigns a call its tier (static.Triage); nil: no tiers.
	Triage func(Call) (tier, reason string)

	mu        sync.Mutex
	untrusted map[string]string // agent → reason, sticky until reset
	lastTier  map[string]string // agent → tier of its latest call (output check)
	bg        sync.WaitGroup    // auto-mode background classifications
}

// OutputJudge screens one tool result; implementations live in
// internal/perm/model.
type OutputJudge interface {
	ClassifyOutput(ctx context.Context, agent, tool, output string) Verdict
}

// NewEngine builds an engine; prompter or log may be nil.
func NewEngine(p Policy, b Prompter, l *DecisionLog) *Engine {
	return &Engine{Policy: p, Prompter: b, Log: l}
}

// Add registers classifiers (static ones first is the caller's choice; the
// engine always runs static_* before model classifiers regardless).
func (e *Engine) Add(cs ...Classifier) {
	e.classifiers = append(e.classifiers, cs...)
}

// Approve asks the human directly and reports an explicit allow. It bypasses
// the mode (auto, classifiers) and the session allowlist, so spending that must
// not be auto-approved (direct oracle after a council) is only ever approved by
// a person. Without an interactive prompter it refuses.
func (e *Engine) Approve(c Call) bool {
	if e.Prompter == nil || !e.Prompter.Interactive() {
		return false
	}
	switch e.Prompter.Ask(c, nil) {
	case AnsAllowOnce, AnsAllowSession:
		return true
	}
	return false
}

// Check implements Gate (PLAN.md §4.2 step order).
func (e *Engine) Check(ctx context.Context, c Call) Outcome {
	if e.Prompter != nil && e.Prompter.Allowed(c) {
		return Outcome{Action: Allow, Reason: "allowed for this session"}
	}
	if e.Triage != nil {
		c.Tier, c.TierReason = e.Triage(c)
	}
	if e.Policy.Mode == "auto" {
		e.noteTier(c)
		// Runs immediately; static rules and the tier's model panel run in
		// the background and are logged, never applied.
		e.background(ctx, c, nil, "auto: background classification")
		return Outcome{Action: Allow, Reason: "auto mode"}
	}
	if e.Policy.Mode == "auto-safe" {
		// Unattended runs: static stops still apply synchronously; nothing
		// prompts or denies; the tier's model panel runs in the background,
		// exactly as in classify mode, and is logged without being applied.
		vs := e.runSet(ctx, c, true)
		for _, v := range vs {
			if v.Flagged && c.Tier != "" && c.Tier != "high" {
				c.Tier, c.TierReason = "high", v.Classifier+": "+v.Reason
			}
		}
		e.noteTier(c)
		if d := Decide(e.Policy, vs); d.Action == Stop {
			return e.finish(c, Stop, vs, "", d.Reason)
		}
		e.background(ctx, c, vs, "auto-safe: background classification")
		return Outcome{Action: Allow, Reason: "auto-safe mode"}
	}
	// Static classifiers first (microseconds); a solo stop skips model calls.
	vs := e.runSet(ctx, c, true)
	if d := Decide(e.Policy, vs); d.Action == Stop {
		return e.finish(c, Stop, vs, "", d.Reason)
	}
	// Anything a static rule flags is high tier, whatever triage said.
	for _, v := range vs {
		if v.Flagged && c.Tier != "" && c.Tier != "high" {
			c.Tier, c.TierReason = "high", v.Classifier+": "+v.Reason
		}
	}
	e.noteTier(c)
	e.markUntrusted(&c)
	// Model classifiers: only the panel for this tier. Only a flag (static
	// or model) prompts; an untrusted output just makes the panel judge.
	vs = append(vs, e.runPanel(ctx, c)...)
	d := Decide(e.Policy, vs)
	action, reason := d.Action, d.Reason
	switch action {
	case Stop:
		return e.finish(c, Stop, vs, "", reason)
	case Prompt:
		if e.Prompter == nil {
			return e.finish(c, Deny, vs, "", e.denyReason())
		}
		switch e.Prompter.Ask(c, vs) {
		case AnsAllowOnce:
			return e.finish(c, Allow, vs, "y", reason)
		case AnsAllowSession:
			return e.finish(c, Allow, vs, "a", reason)
		case AnsStop:
			return e.finish(c, Stop, vs, "s", reason)
		default:
			return e.finish(c, Deny, vs, "n", e.denyReason())
		}
	default:
		return e.finish(c, Allow, vs, "", reason)
	}
}

// background classifies c off the calling goroutine and logs the verdicts
// as an allow. static, when non-nil, holds static verdicts already computed.
func (e *Engine) background(ctx context.Context, c Call, static []Verdict, reason string) {
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		bg := context.WithoutCancel(ctx)
		vs := static
		if vs == nil {
			vs = e.runSet(bg, c, true)
			for _, v := range vs {
				if v.Flagged && c.Tier != "" && c.Tier != "high" {
					c.Tier, c.TierReason = "high", v.Classifier+": "+v.Reason
				}
			}
		}
		// The same model calls classify would make for this tier, logged
		// and never applied.
		e.markUntrusted(&c)
		vs = append(vs, e.runPanel(bg, c)...)
		e.Log.Add(e.entry(c, vs, Allow, "", reason))
		e.notify(c, vs)
	}()
}

// markUntrusted notes on c that an earlier output of its agent flagged the
// untrusted-output check (§4.3), and raises a trivial call to low so the
// model panel judges it with that context. It never prompts by itself.
func (e *Engine) markUntrusted(c *Call) {
	r := e.untrustedReason(c.Agent)
	if r == "" {
		return
	}
	c.Untrusted = r
	if c.Tier == "trivial" {
		c.Tier, c.TierReason = "low", r
	}
}

func (e *Engine) untrustedReason(agent string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.untrusted[agent]
}

// CheckOutput screens one tool result and, when the output check is in state
// "on", records an untrusted flag for that agent. In auto mode it runs in the
// background so the tool call never waits on it.
func (e *Engine) CheckOutput(ctx context.Context, agent, tool, output string) {
	if e == nil || e.Output == nil {
		return
	}
	cfg, ok := e.Policy.Classifiers[OutputCheckName]
	if !ok || cfg.state() == "off" {
		return
	}
	e.mu.Lock()
	tier := e.lastTier[agent]
	e.mu.Unlock()
	// Output of trivial calls (reading your own files, builds) is not
	// screened, in every mode.
	if tier == "trivial" {
		return
	}
	if e.Policy.Mode == "auto" || e.Policy.Mode == "auto-safe" {
		e.bg.Add(1)
		go func() {
			defer e.bg.Done()
			e.checkOutput(context.WithoutCancel(ctx), agent, tool, output, cfg)
		}()
		return
	}
	e.checkOutput(ctx, agent, tool, output, cfg)
}

// WaitBackground blocks until auto-mode background classifications finish or
// the timeout expires, so a headless run logs its verdicts before exit.
func (e *Engine) WaitBackground(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		e.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (e *Engine) checkOutput(ctx context.Context, agent, tool, output string, cfg ClassifierConfig) {
	if cfg.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	v := e.Output.ClassifyOutput(ctx, agent, tool, output)
	if v.Classifier == "" {
		v.Classifier = OutputCheckName
	}
	reason := "output check clean"
	flagged := v.Err == nil && v.Flagged
	if v.Err != nil {
		reason = "output check error"
	}
	if flagged && cfg.state() == "on" {
		reason = "untrusted instructions in previous output"
		e.mu.Lock()
		if e.untrusted == nil {
			e.untrusted = map[string]string{}
		}
		e.untrusted[agent] = reason
		e.mu.Unlock()
	} else if flagged {
		reason = "untrusted instructions in previous output (log state)"
	}
	e.Log.Add(logEntry{
		Mode: e.Policy.Mode, Agent: agent, Tool: tool, Detail: "output check",
		Verdicts: []Verdict{v}, Action: "output", Reason: reason,
	})
}

func (e *Engine) notify(c Call, vs []Verdict) {
	if e.Notify != nil {
		e.Notify(c, vs)
	}
}

// denyReason distinguishes a human's "n" from an unattended prompt.
func (e *Engine) denyReason() string {
	if e.Prompter != nil && e.Prompter.Interactive() {
		return "denied by user"
	}
	return "denied by permission policy (no human attached)"
}

// runSet runs each registered classifier in the requested group (static_*
// or model) that judges this call, exactly once per classifier.
// runPanel runs the model classifiers on the call's tier panel.
func (e *Engine) runPanel(ctx context.Context, c Call) []Verdict {
	var picked []Classifier
	for _, cl := range e.classifiers {
		if isStatic(cl.Name()) || !e.Policy.Panel(c.Tier, cl.Name()) {
			continue
		}
		cfg, ok := e.Policy.Classifiers[cl.Name()]
		if !ok || cfg.State == "off" || !cfg.Judges(c.Tool) {
			continue
		}
		picked = append(picked, cl)
	}
	return runAll(ctx, e.Policy, c, picked)
}

func (e *Engine) noteTier(c Call) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastTier == nil {
		e.lastTier = map[string]string{}
	}
	e.lastTier[c.Agent] = c.Tier
}

func (e *Engine) runSet(ctx context.Context, c Call, wantStatic bool) []Verdict {
	var picked []Classifier
	for _, cl := range e.classifiers {
		if isStatic(cl.Name()) != wantStatic {
			continue
		}
		cfg, ok := e.Policy.Classifiers[cl.Name()]
		if !ok || cfg.State == "off" || !cfg.Judges(c.Tool) {
			continue
		}
		picked = append(picked, cl)
	}
	return runAll(ctx, e.Policy, c, picked)
}

// runAll runs classifiers sequentially when there is at most one, otherwise
// in parallel with per-classifier timeouts.
func runAll(ctx context.Context, p Policy, c Call, cls []Classifier) []Verdict {
	if len(cls) <= 1 {
		out := make([]Verdict, 0, len(cls))
		for _, cl := range cls {
			out = append(out, withTimeout(ctx, p, cl, c))
		}
		return out
	}
	out := make([]Verdict, len(cls))
	var wg sync.WaitGroup
	for i, cl := range cls {
		wg.Add(1)
		go func(i int, cl Classifier) {
			defer wg.Done()
			out[i] = withTimeout(ctx, p, cl, c)
		}(i, cl)
	}
	wg.Wait()
	return out
}

func withTimeout(ctx context.Context, p Policy, cl Classifier, c Call) Verdict {
	cfg := p.Classifiers[cl.Name()]
	if cfg.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	v := cl.Classify(ctx, c)
	if v.Err == nil && ctx.Err() != nil {
		v.Err = ctx.Err()
	}
	return v
}

func (e *Engine) finish(c Call, action string, vs []Verdict, answer, reason string) Outcome {
	e.Log.Add(e.entry(c, vs, action, answer, reason))
	e.notify(c, vs)
	return Outcome{Action: action, Reason: reason}
}

func (e *Engine) entry(c Call, vs []Verdict, action, answer, reason string) logEntry {
	return logEntry{
		Mode: e.Policy.Mode, Agent: c.Agent, Cwd: c.Cwd, Tool: c.Tool, Tier: c.Tier, TierWhy: c.TierReason,
		Detail: detail(c), Verdicts: vs, Action: action, Reason: reason, Answer: answer,
	}
}

func isStatic(name string) bool { return strings.HasPrefix(name, "static_") }
