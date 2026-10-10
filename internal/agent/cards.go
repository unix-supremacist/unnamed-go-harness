package agent

import (
	"fmt"
	"github.com/unix/ugh/internal/provider"
	"strings"
)

// card is a built-in model or role card. Model cards carry routes ("provider:model",
// in preference order), thinking levels, context window and compact_at; role
// cards carry the router-facing purpose (Body), the system prompt and tools.
type card struct {
	ID                      string
	Tools, Routes, Thinking []string
	Prompt, Body            string
	Context                 int
	CompactAt               int // prompt tokens that trigger compaction on this model
}

// modelCardTable and roleCardTable are ugh's built-in cards.
var modelCardTable = map[string]card{
	"deepseek-v4.1-flash": {Routes: []string{"opencode-go:deepseek-v4.1-flash"}, Context: 1000000, Thinking: []string{"low", "high", "max"}},
	"glm-5.3-flash":       {Routes: []string{"opencode-go:glm-5.3-flash"}, Context: 1000000, Thinking: []string{"low", "high", "max"}},
	"gpt-astra":           {Routes: []string{"codex:gpt-6-astra"}, Context: 272000, Thinking: []string{"low", "medium", "high", "xhigh", "max"}},
	"gpt-luna":            {Routes: []string{"codex:gpt-6-luna"}, Context: 272000, Thinking: []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
	"gpt-sol":             {Routes: []string{"codex:gpt-6.1-sol", "codex:gpt-6-sol", "codex:gpt-5.6-sol"}, Context: 272000, Thinking: []string{"low", "medium", "high", "xhigh", "max"}},
	"mimo-v2.6-flash":     {Routes: []string{"opencode-go:mimo-v2.6-flash"}, Context: 1000000},
	"mimo-v2.6-pro":       {Routes: []string{"opencode-go:mimo-v2.6-pro"}, Context: 1048576},
	"qwen3.8-flash":       {Routes: []string{"opencode-go:qwen3.8-flash"}, Context: 1000000},
}

var (
	readTools     = []string{"read", "grep", "find", "ls", "bash"}
	roleCardTable = map[string]card{
		"scout": {Tools: readTools,
			Body:   "Purpose: Perform read-only repository reconnaissance and identify relevant files, symbols, and flows.\nTypical tasks: find an implementation seam; trace a call path; inventory affected files; gather repository evidence.\nNot for: parsing supplied logs (log-parser), writing an implementation plan (planner), or changing files (worker).",
			Prompt: "You are a fast read-only reconnaissance subagent. Inspect only the relevant code and return a concise, evidence-backed handoff with exact file paths and line ranges, key flows, likely change locations, constraints, risks, and open questions. Do not edit files or guess. Use bash only for read-only inspection."},
		"worker": {Tools: []string{"read", "grep", "find", "ls", "bash", "edit", "write"},
			Body:   "Purpose: Implement a requested change and validate it against project conventions.\nTypical tasks: fix a bug; add a feature; refactor known seams; run tests and report changed files.\nNot for: architecture advice without implementation (oracle), independent review (reviewer), repository reconnaissance (scout), or producing a plan without implementing it (planner).",
			Prompt: "You are a focused implementation subagent. Follow the bounded assignment and existing project conventions. Inspect relevant code before making the smallest coherent change, then run appropriate validation. Do not make unapproved product or architecture decisions; report blockers instead. Return changed files, validation performed, and remaining risks."},
		"planner": {Tools: readTools,
			Body:   "Purpose: Produce a self-contained implementation plan grounded in repository evidence.\nTypical tasks: order a multi-file change; identify symbols and validation; turn requirements into implementation steps; expose unresolved decisions.\nNot for: executing the plan (worker), selecting architecture among competing designs (oracle), or returning repository reconnaissance alone (scout).",
			Prompt: "You are a read-only planning subagent. Inspect the relevant repository evidence, resolve implementation details where possible, and return the current orchestrator a self-contained, decision-ready Markdown plan. Include objective, constraints, affected files and symbols, ordered implementation steps, validation, risks, and any genuinely unresolved decision. Do not edit files. Do not merely describe your investigation; the final response itself must be the Markdown plan that the orchestrator can directly use or relay."},
		"reviewer": {Tools: readTools,
			Body:   "Purpose: Independently review a diff, plan, test set, or code seam and issue an evidence-based verdict.\nTypical tasks: find correctness regressions; assess security-sensitive changes; identify missing tests; detect scope drift.\nNot for: authoring the implementation (worker), architecture selection (oracle), repository mapping (scout), or diagnosing failures from logs or test output (log-parser).",
			Prompt: "You are an evidence-based read-only reviewer. Inspect the named diff, plan, tests, or code seam and report only concrete supported findings. Cite exact paths and line numbers. Prioritize correctness, regressions, security, missing tests, and scope drift. If no issue qualifies, say exactly “No issues found.” End with a verdict: BLOCK, OK, or OK with notes."},
		"oracle": {Tools: readTools,
			Body:   "Purpose: Advise on architecture and decisions using repository evidence and explicit tradeoffs.\nTypical tasks: resolve competing designs; uncover hidden constraints; challenge assumptions; recommend an architectural direction.\nNot for: routine implementation (worker), ordinary code review (reviewer), locating repository facts (scout), or producing step-by-step implementation plans (planner).",
			Prompt: "You are a read-only architecture and decision oracle. Reconstruct the task constraints and prior decisions, inspect relevant evidence, identify contradictions and hidden assumptions, compare viable options, and recommend the narrowest justified next move. Clearly separate evidence from inference, state residual risks and unresolved decisions, and never edit files or become the implementation owner."},
		"log-parser": {Tools: readTools,
			Body:   "Purpose: Analyze logs and stack traces into an evidence-backed incident sequence.\nTypical tasks: isolate a root error from noisy output; correlate stack frames; distinguish a primary failure from follow-on noise.\nNot for: repository-wide code discovery (scout), implementation (worker), or independent patch review (reviewer).",
			Prompt: "You are a read-only log and stack-trace analyst. Inspect only the named logs or command output; do not edit, rebuild, or relaunch anything. Return a compressed incident report containing sources inspected, the relevant event sequence, exact errors with file/line evidence, likely root cause versus follow-on noise, and specific next checks. Ignore unrelated spam. If evidence is missing, say so and stop."},
		"orchestrator": {Tools: []string{"read", "grep", "find", "ls"}, Body: orchestratorPurpose, Prompt: orchestratorPrompt},
	}
)

func readCard(kind, id string) (card, error) {
	table := modelCardTable
	if kind == "roles" {
		table = roleCardTable
	}
	c, ok := table[id]
	if !ok {
		return card{}, fmt.Errorf("no %s card %q", strings.TrimSuffix(kind, "s"), id)
	}
	c.ID = id
	return c, nil
}

var roleIDs = []string{"scout", "worker", "planner", "reviewer", "oracle", "log-parser", "orchestrator"}
var modelIDs = []string{"deepseek-v4.1-flash", "glm-5.3-flash", "gpt-astra", "gpt-luna", "gpt-sol", "mimo-v2.6-flash", "mimo-v2.6-pro", "qwen3.8-flash"}

// ContextWindow reports the context window for a model ref (card id or
// provider/model): the provider's own /models listing first, then the model
// card. False when neither knows, so callers can show an unknown context
// honestly instead of guessing.
func ContextWindow(ref string) (int, bool) {
	if win, ok := provider.ContextWindow(ref); ok {
		return win, true
	}
	if !strings.Contains(ref, "/") {
		if c, err := readCard("models", ref); err == nil && c.Context > 0 {
			return c.Context, true
		}
	}
	for _, c := range modelCards() {
		if c.Context <= 0 {
			continue
		}
		for _, r := range usableRoutes(c.Routes) {
			// Agents may know only the bare model id (no provider prefix).
			if rr := routeRef(r); rr == ref || strings.HasSuffix(rr, "/"+ref) {
				return c.Context, true
			}
		}
	}
	return 0, false
}

// CompactAt reports a model card's compact_at for a model ref or card id.
func CompactAt(ref string) (int, bool) {
	if !strings.Contains(ref, "/") {
		if c, err := readCard("models", ref); err == nil && c.CompactAt > 0 {
			return c.CompactAt, true
		}
	}
	for _, c := range modelCards() {
		if c.CompactAt <= 0 {
			continue
		}
		for _, r := range usableRoutes(c.Routes) {
			if rr := routeRef(r); rr == ref || strings.HasSuffix(rr, "/"+ref) {
				return c.CompactAt, true
			}
		}
	}
	return 0, false
}

// CardRef resolves a card id to its first supported route; explicit provider/model refs pass through.
func CardRef(id string) (string, error) {
	if strings.Contains(id, "/") {
		return id, nil
	}
	c, e := readCard("models", id)
	if e != nil {
		return "", e
	}
	routes := usableRoutes(c.Routes)
	if len(routes) == 0 {
		return "", fmt.Errorf("no supported routes for %s", id)
	}
	return routeRef(routes[0]), nil
}
func modelCards() []card {
	var out []card
	for _, id := range modelIDs {
		if c, e := readCard("models", id); e == nil {
			out = append(out, c)
		}
	}
	return out
}
func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// ThinkingLevels are Pi's thinking levels, weakest first.
var ThinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// cardForRef finds the model card for a card id or provider/model ref.
func cardForRef(ref string) (card, bool) {
	if !strings.Contains(ref, "/") {
		if c, err := readCard("models", ref); err == nil {
			return c, true
		}
	}
	for _, c := range modelCards() {
		for _, r := range usableRoutes(c.Routes) {
			if rr := routeRef(r); rr == ref || strings.HasSuffix(rr, "/"+ref) {
				return c, true
			}
		}
	}
	return card{}, false
}

// ModelThinking lists the levels a model supports, "off" first, from its
// card's thinking list (Pi's per-model level map). Without a card, or with
// an empty list (levels not verified, like Pi models with no level map),
// every level is offered and the provider decides.
func ModelThinking(ref string) (levels []string, known bool) {
	c, ok := cardForRef(ref)
	if !ok || len(c.Thinking) == 0 {
		return ThinkingLevels, false
	}
	out := []string{"off"}
	for _, l := range ThinkingLevels[1:] {
		if contains(c.Thinking, l) {
			out = append(out, l)
		}
	}
	return out, true
}

// ClampThinking maps a requested level onto the closest level the model
// supports: the nearest weaker one, else the nearest stronger one.
func ClampThinking(ref, level string) string {
	levels, _ := ModelThinking(ref)
	return clampLevel(levels, level)
}

func clampLevel(levels []string, level string) string {
	if level == "" || contains(levels, level) {
		return level
	}
	want := indexOf(ThinkingLevels, level)
	if want < 0 {
		return level
	}
	for i := want - 1; i >= 1; i-- {
		if contains(levels, ThinkingLevels[i]) {
			return ThinkingLevels[i]
		}
	}
	for i := want + 1; i < len(ThinkingLevels); i++ {
		if contains(levels, ThinkingLevels[i]) {
			return ThinkingLevels[i]
		}
	}
	return "off"
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
