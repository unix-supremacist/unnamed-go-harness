// Package agent: council tool (PLAN.md §6). The aggregator proposes angles,
// each angle × panel model is one read-only scout seat through the spawn
// machinery, and the aggregator synthesizes anonymized reports P1..Pn.
// Failed or timed-out seats are dropped and listed.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/provider"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

// spec and decode are council-local copies of the tools-package helpers.
func spec(name, desc string, params map[string]any) llm.ToolSpec {
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return llm.ToolSpec{Name: name, Description: desc, Parameters: params}
}

func decode(raw json.RawMessage, dst any) error {
	return json.Unmarshal(raw, dst)
}

// DefaultPanel is the M7 panel used when config omits council.panel.
// The cards are the single source of truth for model ids (§5).
var DefaultPanel = []string{"glm-5.3-flash", "deepseek-v4.1-flash", "mimo-v2.6-flash", "qwen3.8-flash", "gpt-luna"}

// Ported council prompts (bounded, §6). Source: pi cards/prompts.
const (
	councilPrescoutPrompt = "Gather evidence ONLY. Identify relevant files, symbols, call flows, tests, and observed facts. For web work, collect sources with URLs and key facts. Do not propose hypotheses, recommendations, or opinions. Mark facts you could not verify. Your tools are read, ls, grep and find — no shell, no writes. Make at most 2-3 targeted evidence checks (specific files only; never whole-repo searches, never the old/ or evals/ trees) and then report with the available evidence; the turn cap is hard, do not explore endlessly. Never read or cite reference answers, grading reports, or other evaluation artifacts. Keep the evidence bundle concise and cite file:line or URL."

	councilAnglesPromptT = "Write exactly N investigation angles for the supplied core question. Each angle must include the core question word for word, address its assigned lens, point toward different evidence or approaches from the other angles, and contain no hypothesis, answer, recommendation, or opinion. Return a numbered list with one concise angle per item."

	councilPanelistPrompt = "Investigate the core question from your assigned angle. The evidence bundle is a starting point, not a boundary. Your tools are read-only and bounded: read, ls, grep, find — there is no shell and no writes. Budget at most 2-3 evidence checks (targeted reads of specific files; never whole-repo searches, never the old/ or evals/ trees), then finish and report with the evidence you have; the turn cap is hard, do not explore endlessly. You are read-only. Return no more than 800 words using exactly these sections:\n\n## Answer\n## Claims\n- <claim> | evidence: <file:line or URL> | confidence: high|medium|low\n## Open questions\n\nDistinguish observed evidence from inference. Do not recommend edits or claim evidence you did not inspect. Never read or cite reference answers, grading reports, or other evaluation artifacts."

	councilSynthesisPrompt = "Synthesize the supplied question, evidence bundle, and anonymized panel reports into an oracle-quality answer. You are read-only. Map agreement and disagreement across reports. Scores estimate evidence quality and relevance; they are not verdicts. Independently verify disputed claims that could change the recommendation using your tools — read, ls, grep, find only — with at most 2-3 targeted checks, and mark anything else unresolved; do not defer to a higher score. Never read or cite reference answers, grading reports, or anything under evals/ or old/. Return: recommendation, supporting evidence, dissent, confidence, remaining risks, and a JSON agreement map with this shape:\n\n{\"conclusions\":[{\"id\":\"c1\",\"summary\":\"...\",\"supported_by\":[\"P2\",\"P4\"],\"contradicted_by\":[\"P1\"]}]}\n\nDo not invent sources or conceal unresolved disagreement."
)

// Lenses are the angle vocabulary (cards/lenses), id → guidance.
var Lenses = map[string]string{
	"adversarial":        "Look for what breaks, fails, or is wrong. Test claims against counterexamples, edge cases, failure paths, and contradictory evidence.",
	"alternatives":       "Construct the strongest evidence-backed case for a competing option. Identify tradeoffs and conditions under which that alternative would be preferable.",
	"conventions":        "Check whether the proposed approach follows the conventions already established in the codebase, adjacent modules, tests, and documentation.",
	"evidence-map":       "Establish what actually exists before judging it. Locate the relevant files, symbols, flows, tests, and sources; separate observed facts from assumptions.",
	"hidden-constraints": "Find unstated requirements, compatibility constraints, invariants, dependencies, and operational assumptions that could change the answer.",
	"minimal-change":     "Find the smallest change or decision that satisfies the question. Identify existing mechanisms that can be reused and avoid unnecessary scope.",
}

// Seat is one panelist result (exported for the eval harness and tests).
type Seat struct {
	Label, Angle, Lens, Model, Report string
	Err                               error
}

// Result is one council run (exported for the eval harness).
type Result struct {
	ID       string
	Report   string
	Seats    []*Seat
	Failed   []string
	Usage    llm.Usage
	Outcome  CouncilOutcomeState
	Strategy string
	Plan     *CouncilPlan
	// Assessment is the post-run viability reading.
	Assessment *ViabilityAssessment
}

// Render formats the council result for the calling model: id, outcome,
// strategy and actual coverage (including omitted families and failures).
func (r *Result) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Council %s · outcome: %s · strategy: %s\n", r.ID, r.Outcome, orDash(r.Strategy))
	if a := r.Assessment; a != nil {
		fmt.Fprintf(&b, "Panel: %d/%d seats succeeded across %d famil(ies), covering %d/%d angles", a.SucceededSeats, a.TotalSeats, a.DistinctFamilies, a.CoveredAngles, a.TotalAngles)
		if r.Plan != nil && len(r.Plan.OmittedFamilies) > 0 {
			fmt.Fprintf(&b, "; omitted (unavailable): %v", r.Plan.OmittedFamilies)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + r.Report)
	if len(r.Failed) > 0 {
		fmt.Fprintf(&b, "\n\nFailed or timed-out seats (dropped): %s", strings.Join(r.Failed, "; "))
	}
	return b.String()
}

// councilTool is the root-facing `council` tool.
type councilTool struct{ agent *Agent }

func (c councilTool) Spec() llm.ToolSpec {
	props := map[string]any{
		"question": map[string]any{"type": "string"},
		"angles":   map[string]any{"description": "number of angles (default from config, 2), or a list of lens ids to pin angles to"},
		"research": map[string]any{"description": "run a read-only pre-research scout to build an evidence bundle first (default false)"},
		"retry":    map[string]any{"type": "string", "description": "id of a council whose synthesis failed: re-run only the synthesis from its retained reports (question is then ignored)"},
	}
	if c.agent.rt != nil && c.agent.rt.traditional() {
		props["strategy"] = map[string]any{"type": "string", "enum": []string{"economical", "oracle-led"},
			"description": "routing is unavailable: choose the synthesis strategy when the normal coordinator chain is gone"}
	}
	return spec("council", "Convene a multi-angle multi-model council on one design question: a coordinator proposes distinct investigation angles, read-only panelists from several model families investigate each angle through the spawn machinery, and the coordinator synthesizes the anonymized reports into a recommendation with evidence, dissent, confidence and an agreement map. The result names the council id, outcome, strategy and actual coverage; failed seats are dropped and listed, and a council needs at least three distinct panel families.", map[string]any{
		"type": "object", "properties": props,
		"required": []string{},
	})
}

func (c councilTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var req struct {
		Question string          `json:"question"`
		Angles   json.RawMessage `json:"angles"`
		Research bool            `json:"research"`
		Strategy string          `json:"strategy"`
		Retry    string          `json:"retry"`
	}
	if err := decode(raw, &req); err != nil {
		return "", err
	}
	res, err := councilRun(ctx, c.agent, req.Question, req.Angles, req.Research, councilOpts{Strategy: req.Strategy, Retry: req.Retry})
	if err != nil {
		return "", err
	}
	return res.Render(), nil
}

// CouncilRun executes one full council in cwd and returns the synthesis plus
// seat metadata. It is the entry point for `ugh eval council` and owns its
// own tree; the root-facing council tool calls councilRun below so its seats
// hang off the parent's tree node and are visible in the main TUI tree.
func CouncilRun(ctx context.Context, cwd, question string, anglesRaw json.RawMessage, research bool, gate perm.Gate, limit func(string) chan struct{}) (*Result, error) {
	root := New(Config{Tree: &Tree{}, Cwd: cwd, Gate: gate, Agent: "council", StreamLimit: limit, Admission: DefaultAdmission(admission.PriorityChild, "council")})
	return councilRun(ctx, root, question, anglesRaw, research, councilOpts{})
}

// ---- config plumbing ----

func councilConfig() config.Council {
	out := config.Default().Council
	if cfg, err := config.Load(); err == nil {
		c := cfg.Council
		if c.Aggregator != "" {
			out.Aggregator = c.Aggregator
		}
		if len(c.Panel) > 0 {
			out.Panel = c.Panel
		}
		if c.DefaultAngles > 0 {
			out.DefaultAngles = c.DefaultAngles
		}
		if c.MaxSeats > 0 {
			out.MaxSeats = c.MaxSeats
		}
		if c.SeatTimeoutS > 0 {
			out.SeatTimeoutS = c.SeatTimeoutS
		}
		if c.MaxReportWords > 0 {
			out.MaxReportWords = c.MaxReportWords
		}
		if c.Single != "" {
			out.Single = c.Single
		}
		if c.Oracle != "" {
			out.Oracle = c.Oracle
		}
	}
	if len(out.Panel) == 0 {
		out.Panel = DefaultPanel
	}
	return out
}

// angleSpec decodes the tool's `angles` argument: absent/number → N angles;
// a JSON string or array names lens ids to pin angles to.
func angleSpec(raw json.RawMessage, def int) (n int, lenses []string) {
	n = def
	if len(raw) == 0 || string(raw) == "null" {
		return n, nil
	}
	var num int
	if json.Unmarshal(raw, &num) == nil && num > 0 {
		return num, nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return 1, []string{one}
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		return len(list), list
	}
	return n, nil
}

// lensVocab renders the lens list for the aggregator, pinning angles to the
// supplied lens ids when present.
func lensVocab(pinned []string) string {
	var b strings.Builder
	if len(pinned) > 0 {
		for i, id := range pinned {
			fmt.Fprintf(&b, "%d. %s: %s\n", i+1, id, Lenses[id])
		}
		return strings.TrimRight(b.String(), "\n")
	}
	for id, body := range Lenses {
		fmt.Fprintf(&b, "- %s: %s\n", id, body)
	}
	return strings.TrimRight(b.String(), "\n")
}

type angle struct {
	text, lens string
}

var numberedLine = regexp.MustCompile(`^\d+[.)]\s+(.*)$`)

// parseAngles extracts one angle per numbered list line from the aggregator
// text and tags each with the pinned lens (round-robin) when supplied.
func parseAngles(text string, n int, lenses []string) []angle {
	var out []angle
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if m := numberedLine.FindStringSubmatch(line); m != nil {
			line = strings.TrimSpace(m[1])
		} else if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			line = strings.TrimSpace(line[2:])
		}
		if len(line) < 20 { // a real angle is a sentence, not a heading
			continue
		}
		if n > 0 && len(out) >= n {
			break
		}
		lens := ""
		if len(lenses) > 0 {
			lens = lenses[len(out)%len(lenses)]
		}
		out = append(out, angle{text: line, lens: lens})
	}
	return out
}

// seatTask builds the panelist's task text: core question, assigned angle
// and the evidence bundle as a starting point. It restates the tool budget
// so weaker flash models finish instead of exploring to the turn cap.
func seatTask(core, angleText, lens, evidence string, seatsTotal int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are one read-only panel seat of %d investigating a design question with your repository tools.\n", seatsTotal)
	fmt.Fprintf(&b, "\nCore question: %s\n", core)
	if lens != "" {
		fmt.Fprintf(&b, "\nAssigned lens: %s\n", lens)
	}
	fmt.Fprintf(&b, "\nAssigned angle: %s\n", angleText)
	fmt.Fprintf(&b, "\nEvidence bundle (a starting point, not a boundary):\n%s\n", orNone(evidence))
	b.WriteString("\nYour tools are read, ls, grep and find only (no shell, no writes). Make at most 2-3 targeted evidence checks on specific files — no whole-repo searches, and skip the old/ and evals/ trees entirely — then answer with the evidence you have within the turn cap.\n")
	return b.String()
}

func synthesisTask(q, evidence string, reports, failed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Core question: %s\n", q)
	fmt.Fprintf(&b, "\nEvidence bundle:\n%s\n", orNone(evidence))
	fmt.Fprintf(&b, "\nAnonymized panel reports (angle kept, model hidden):\n\n%s", strings.Join(reports, "\n\n---\n\n"))
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\n\nFailed or timed-out seats (dropped): %s", strings.Join(failed, "; "))
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// clampWords truncates s to max words on word boundaries.
func clampWords(s string, max int) string {
	words := strings.Fields(s)
	if len(words) <= max {
		return s
	}
	return strings.Join(words[:max], " ") + " …[truncated]"
}

// ---- scout machinery (shared with the eval harness) ----

// resolveModel resolves a model card id to its first usable provider route,
// walking the card's route order on failure (§5: cards are the source of
// truth; goat: routes are dropped). It returns the remaining route order so
// the caller can attach it for fallback.
func resolveModel(modelID string) (llm.Provider, string, string, []string, error) {
	c, err := readCard("models", modelID)
	if err != nil {
		return nil, "", "", nil, err
	}
	routes := usableRoutes(c.Routes)
	if len(routes) == 0 {
		return nil, "", "", nil, fmt.Errorf("no supported route for %s", modelID)
	}
	var last error
	for i, r := range routes {
		ref := routeRef(r)
		p, m, e := provider.Resolve(ref)
		if e == nil {
			return p, m, ref, routes[i:], nil
		}
		last = e
	}
	return nil, "", "", nil, last
}

// readOnlyTools returns the bounded read-only toolset: read, ls, grep, find.
// No bash and no write/edit — council seats can only inspect evidence.
func readOnlyTools() *tools.Registry {
	reg := tools.NewRegistry()
	all := tools.NewRegistry(tools.All()...)
	for _, name := range []string{"read", "ls", "grep", "find"} {
		if t, ok := all.Get(name); ok {
			reg.Add(t)
		}
	}
	return reg
}

// runScout runs one read-only scout child through the spawn machinery: a
// tree node under root, a recorded session, the per-provider stream
// semaphore and the parent's permission gate. The card's remaining route
// order is attached, so a provider failure falls through to the next route
// exactly like spawn children do (stream-level fallback and accounting).
func runScout(ctx context.Context, root *Agent, modelID, system, task, role string, maxTurns int) (string, llm.Usage, error) {
	prov, model, ref, routes, err := resolveModel(modelID)
	if err != nil {
		return "", llm.Usage{}, err
	}
	node := root.cfg.Tree.Child(root.cfg.Node, role, ref, root.cfg.Cwd)
	node.setStatus("running")
	sess, err := session.Create(root.cfg.Cwd, session.Record{Model: ref, Cwd: root.cfg.Cwd, Parent: root.cfg.Node.ID, Agent: node.ID})
	if err != nil {
		node.setStatus("error")
		return "", llm.Usage{}, err
	}
	defer sess.Close()
	thinking := "low"
	if mc, e := readCard("models", modelID); e == nil {
		if len(mc.Thinking) == 0 {
			thinking = "off"
		} else if !contains(mc.Thinking, thinking) {
			thinking = mc.Thinking[0]
		}
	}
	a := New(Config{
		Provider: prov, Model: model, System: system, Thinking: thinking,
		Tools: readOnlyTools(), Cwd: root.cfg.Cwd, Gate: root.cfg.Gate,
		Agent: node.ID, Tree: root.cfg.Tree, Node: node, Routes: routes,
		StreamLimit: root.cfg.StreamLimit, MaxTurns: maxTurns,
		Admission: childAdmission(root.cfg.Admission, "council-panel"),
		OnMessage: func(m llm.Message) { _ = sess.Append(session.Record{Type: "message", Message: &m}) },
	})
	final, err := a.Run(ctx, task)
	if err != nil {
		node.setStatus("error")
		return "", a.Usage, err
	}
	node.setStatus("done")
	return final, a.Usage, nil
}

// ScoutRun answers one task with a model card id in a standalone read-only
// scout (used by `ugh eval council` for the single-model and oracle runs).
func ScoutRun(ctx context.Context, cwd, modelID, task string) (string, llm.Usage, error) {
	tree := &Tree{}
	root := New(Config{Tree: tree, Cwd: cwd, Agent: "scout", Admission: DefaultAdmission(admission.PriorityChild, "scout")})
	return runScout(ctx, root, modelID, "", task, "scout", 12)
}
