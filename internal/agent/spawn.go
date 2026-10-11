package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
	"github.com/unix/ugh/internal/worktree"
)

type Task struct {
	Task       string `json:"task"`
	Cwd        string `json:"cwd,omitempty"`
	Role       string `json:"role,omitempty"`
	Tier       string `json:"tier,omitempty"` // traditional mode only: low | high
	Thinking   string `json:"thinking,omitempty"`
	MaxTurns   int    `json:"max_turns,omitempty"`
	MaxMinutes int    `json:"max_minutes,omitempty"`
}
type spawnTool struct{ agent *Agent }

func (s spawnTool) Spec() llm.ToolSpec {
	props := map[string]any{}
	keys := []string{"task", "cwd", "role"}
	if s.agent.rt != nil && s.agent.rt.traditional() {
		// Routers are unavailable: tier and thinking are caller-selected for now.
		keys = append(keys, "tier", "thinking")
	}
	for _, key := range keys {
		props[key] = map[string]any{"type": "string"}
	}
	props["role"] = map[string]any{"type": "string", "description": "optional suggestion; the router decides and eligibility rules still apply"}
	if props["tier"] != nil {
		props["tier"] = map[string]any{"type": "string", "enum": []string{"low", "high"},
			"description": "routers are down, so you pick the tier: low for simple, narrow work; high for difficult, ambiguous work. The tier's chain picks the model"}
	}
	props["max_minutes"] = map[string]any{"type": "integer", "description": "wall-clock budget for this child; when it runs out the child wraps up and reports (graceful turn-out)"}
	props["max_turns"] = map[string]any{"type": "integer", "description": fmt.Sprintf(
		"turn budget for this child (default from its role and depth; at most %d). Give research and orchestration more", maxTurnsCap())}
	desc := "Run child tasks in parallel, optionally specifying cwd and a role suggestion. The router picks the tier, model and thinking."
	params := map[string]any{"tasks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": props, "required": []string{"task"}}}}
	if s.agent.cfg.StartChild != nil {
		desc = "Start child tasks in the background (optionally with cwd and a role suggestion; the router picks the tier, model and thinking) and return at once. " +
			"Keep working while they run: their results arrive automatically at a later turn, agent_status shows progress, " +
			"and wait_agents blocks for them. Set wait=true to block until all of them finish instead."
		params["wait"] = map[string]any{"type": "boolean", "description": "block until all children finish (default false)"}
	}
	return llm.ToolSpec{Name: "spawn", Description: desc, Parameters: map[string]any{"type": "object", "properties": params, "required": []string{"tasks"}}}
}
func (s spawnTool) Run(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
	var req struct {
		Tasks []Task `json:"tasks"`
		Wait  bool   `json:"wait"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", err
	}
	if len(req.Tasks) == 0 || len(req.Tasks) > 16 {
		return "", fmt.Errorf("spawn: tasks must contain 1-16 entries")
	}
	if s.agent.cfg.Node.Depth >= 4 {
		return "", fmt.Errorf("spawn: depth limit")
	}
	a := s.agent
	if a.cfg.StartChild == nil {
		return a.cfg.Spawn(ctx, a, req.Tasks), nil
	}
	kids := a.startKids(ctx, req.Tasks)
	if req.Wait {
		for _, k := range kids {
			for !k.finished() && !a.hasSteered() && ctx.Err() == nil {
				a.block(ctx, 0)
			}
		}
		out := formatResults(a.collectFinished()) // also any earlier finished children
		if n := a.pendingKids(); n > 0 {
			out += fmt.Sprintf("\n\nStopped waiting: the user sent you a message (it follows). %d subagent(s) still running; "+
				"their results arrive automatically, or call wait_agents.", n)
		}
		return out, nil
	}
	labels := make([]string, len(kids))
	for i, k := range kids {
		labels[i] = "- " + k.label
	}
	return fmt.Sprintf("Started %d subagent(s) in the background:\n%s\n%d subagent(s) running in total. Keep working; "+
		"results arrive automatically in a later turn. agent_status shows progress; wait_agents blocks for them.",
		len(kids), strings.Join(labels, "\n"), a.pendingKids()), nil
}

type Runtime struct {
	Tree   *Tree
	RunID  string // links all session files of this tree
	Config config.Config
	mu     sync.Mutex
	limits map[string]chan struct{}
	live   chan struct{} // [limits] max_live slots
	// parkedKids are finished children that their parent may resume.
	parkedKids  map[string]*parked
	reserveOnce sync.Once
	writers     map[string]*worktree.Worktree // feature-writer worktrees by agent id
	tradUntil   time.Time                     // model-selectable delegation is open until then (router outage)
	// runChild runs one child; tests replace it.
	runChild func(ctx context.Context, parent *Agent, t Task) ChildResult
}

func NewRuntime() *Runtime {
	c, _ := config.Load()
	if c.Routing.ProviderConcurrency < 1 {
		c.Routing.ProviderConcurrency = 4
	}
	if c.Limits.MaxLive < 1 {
		c.Limits.MaxLive = 16
	}
	r := &Runtime{Tree: &Tree{}, RunID: newRunID(), Config: c, limits: map[string]chan struct{}{},
		live: make(chan struct{}, c.Limits.MaxLive)}
	r.runChild = r.startChild
	return r
}
func (r *Runtime) Limit(name string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.limits[name] == nil {
		r.limits[name] = make(chan struct{}, r.Config.StreamsFor(name))
	}
	return r.limits[name]
}

// acquireLive takes one live-agent slot, waiting while the tree is full.
func (r *Runtime) acquireLive(ctx context.Context) error {
	select {
	case r.live <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *Runtime) releaseLive() { <-r.live }

func (r *Runtime) Spawn(ctx context.Context, parent *Agent, tasks []Task) string {
	// A parent waiting on its children gives up its live slot, so a full
	// tree of waiting parents can never starve the children they wait for.
	if parent.liveSlot {
		r.releaseLive()
		defer func() {
			_ = r.acquireLive(context.WithoutCancel(ctx))
		}()
	}
	out := make([]string, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, t Task) { defer wg.Done(); out[i] = r.child(ctx, parent, t) }(i, task)
	}
	wg.Wait()
	return strings.Join(out, "\n\n")
}

// StartChild runs one child to completion (Config.StartChild for async spawn).
func (r *Runtime) StartChild(ctx context.Context, parent *Agent, t Task) ChildResult {
	return r.runChild(ctx, parent, t)
}
func (r *Runtime) child(ctx context.Context, parent *Agent, t Task) string {
	return r.runChild(ctx, parent, t).String()
}

// ChildResult is what a parent learns about one spawned child.
type ChildResult struct {
	ID, Model, Cwd, Session string
	Status                  string // ok | turn-out | error | cancelled
	Turns, Tools            int
	MaxTurns                int           // the child's turn budget
	Wall                    time.Duration // running time (queue time excluded)
	Usage                   llm.Usage
	Report                  string
	Denied                  []string // permission denials during the run
}

// String renders the result for the parent: one header line, the session
// path (so a thin report can be checked against the transcript), the report.
func (c ChildResult) String() string {
	var b strings.Builder
	turns := fmt.Sprint(c.Turns)
	if c.MaxTurns > 0 {
		turns += fmt.Sprintf("/%d", c.MaxTurns)
	}
	wall := ""
	if c.Wall > 0 {
		wall = " · wall: " + c.Wall.Round(time.Second).String()
	}
	fmt.Fprintf(&b, "### %s · status: %s · model: %s · turns: %s · tools: %d · tokens: %d in/%d out%s\n",
		orDash(c.ID), c.Status, orDash(c.Model), turns, c.Tools, c.Usage.In, c.Usage.Out, wall)
	fmt.Fprintf(&b, "cwd: %s\nsession: %s\n", orDash(c.Cwd), orDash(c.Session))
	if len(c.Denied) > 0 {
		fmt.Fprintf(&b, "denied: %d\n- %s\n", len(c.Denied), strings.Join(c.Denied, "\n- "))
	}
	fmt.Fprintf(&b, "\n%s", strings.TrimSpace(c.Report))
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// childStatus maps a child's Run outcome to its reported status.
func childStatus(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrTurnOut):
		return "turn-out"
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "error"
	}
}

func (r *Runtime) startChild(ctx context.Context, parent *Agent, t Task) ChildResult {
	r.watchReserve()
	fail := func(msg string) ChildResult { return ChildResult{Status: "error", Report: "spawn error: " + msg} }
	if strings.TrimSpace(t.Task) == "" {
		return fail("empty task")
	}
	cwd := parent.cfg.Cwd
	if t.Cwd != "" {
		if filepath.IsAbs(t.Cwd) {
			cwd = filepath.Clean(t.Cwd)
		} else {
			cwd = filepath.Join(cwd, t.Cwd)
		}
	}
	if t.Role != "" && !contains(roleIDs, t.Role) {
		return fail("unknown role " + t.Role)
	}
	if t.Role == "orchestrator" && !canOrchestrate(parent) {
		return fail("orchestrators cannot be spawned here: the child would be unable to delegate (depth limit) or orchestrators would nest")
	}
	// Oracles are the expensive second opinion: only after a council.
	var (
		choice routeChoice
		err    error
	)
	switch {
	case r.traditional():
		if t.Tier == "" && t.Role != "oracle" {
			role := t.Role
			if role == "" {
				role = "scout"
			}
			return fail("routing is unavailable and tier selection is open: resubmit this task with tier (one of: " +
				strings.Join(traditionalChoices(r.Config, role), ", ") + ") and optionally role and thinking")
		}
		role := t.Role
		if role == "" {
			role = "worker"
		}
		choice, err = r.planTraditional(t, role)
		if err != nil {
			return fail(err.Error())
		}
	default:
		if t.Tier != "" || t.Thinking != "" {
			return fail("tier and thinking are chosen by the router, not the spawning model; resubmit without them")
		}
		choice, err = r.planManaged(ctx, t.Task, t.Role, allowedRoles(parent))
		var unavailable *RoutingUnavailableError
		if errors.As(err, &unavailable) {
			r.openTraditional(unavailable.Cooldown)
			res := fail(fmt.Sprintf("routing-unavailable: neither router could answer (%s). Nothing was launched. "+
				"Tier selection is open for %s: resubmit this task with tier (one of: %s), and optionally role and thinking.",
				unavailable.Reason, unavailable.Cooldown.Round(time.Second), strings.Join(traditionalChoices(r.Config, orDefault(t.Role, "worker")), ", ")))
			res.Status = "routing-unavailable"
			return res
		}
		if err != nil {
			return fail(err.Error())
		}
	}
	role := choice.Role
	if role == "oracle" { // whoever picked it (caller, router or traditional selection), spending is gated here
		if err := oracleGate(ctx, r.Config, parent); err != nil {
			return fail(err.Error())
		}
	}
	rc, err := readCard("roles", role)
	if err != nil {
		return fail(err.Error())
	}
	routes := make([]string, len(choice.Routes))
	for i, rt := range choice.Routes {
		routes[i] = rt.RouteKey()
	}
	ref := routeRef(routes[0])
	prov, id, err := resolveRoute(ref)
	if err != nil {
		for i, route := range routes[1:] {
			ref = routeRef(route)
			prov, id, err = resolveRoute(ref)
			if err == nil {
				routes = routes[i+1:]
				break
			}
		}
	}
	if err != nil {
		return fail(err.Error())
	}
	thinking := choice.Thinking
	if levels, known := ModelThinking(ref); known {
		thinking = clampLevel(levels, thinking)
	}
	note := choice.describe()
	node := r.Tree.Child(parent.cfg.Node, role, ref, cwd)
	var wt *worktree.Worktree
	if isFeatureWriter(parent, role) {
		var werr error
		if wt, cwd, werr = r.allocateWriter(ctx, node, t.Task, cwd); werr != nil {
			node.setStatus("error")
			res := fail("worktree: " + werr.Error())
			res.ID, res.Model, res.Cwd = node.ID, ref, cwd
			return res
		}
		node.tree.mu.Lock()
		node.Cwd = cwd
		node.tree.mu.Unlock()
	}
	sess, err := session.Create(cwd, session.Record{Model: ref, Thinking: thinking, System: rc.Prompt, Cwd: cwd, Parent: parent.cfg.Node.ID, Agent: node.ID, Run: r.RunID})
	if err != nil {
		node.setStatus("error")
		res := fail(err.Error())
		res.ID, res.Model, res.Cwd = node.ID, ref, cwd
		return res
	}
	sw := &sessWriter{s: sess}
	if wt != nil {
		wt.SessionCallback = func(rec session.Record) error { sw.append(rec); return nil }
		_ = wt.SaveMeta()
	}
	reg := tools.NewRegistry()
	all := tools.NewRegistry(tools.All()...)
	for _, name := range rc.Tools {
		if tool, ok := all.Get(name); ok {
			reg.Add(tool)
		}
	}
	a := New(Config{Provider: prov, Model: id, System: rc.Prompt, Thinking: thinking, Tools: reg, Cwd: cwd, Gate: parent.cfg.Gate, Agent: node.ID, Tree: r.Tree, Node: node, Spawn: r.Spawn, StartChild: r.StartChild, Resume: r.ResumeChild, Routes: routes, StreamLimit: r.Limit, Runtime: r, Admission: childAdmission(parent.cfg.Admission, "delegated"), MaxTurns: r.Config.Agent.ChildTurns(role, node.Depth, t.MaxTurns), AutoCompact: r.Config.Agent.AutoCompact,
		ToolTimeout: time.Duration(r.Config.Tools.TimeoutS) * time.Second, BlobDir: session.BlobDir(sess.Path()), Context: parent.cfg.Context,
		OnMessage: func(m llm.Message) { sw.append(session.Record{Type: "message", Message: &m}) },
		OnCompact: func(h []llm.Message) { sw.append(session.Record{Type: "compaction", Seed: h}) },
		OnRecord:  func(rec session.Record) { sw.append(rec) },
		OnStatus:  func(st string) { sw.append(session.Record{Type: "status", Status: st}) }})
	a.maxMinutes = t.MaxMinutes
	a.rt = r
	a.routeNote = note
	a.thinkingWant = choice.Thinking
	sw.append(session.Record{Type: "route", Agent: node.ID, Meta: choice.meta()})
	a.cfg.Inbox, _ = session.InboxPath(r.RunID, node.ID)
	a.cfg.Scratch = session.ScratchPath(r.RunID, node.ID)
	return r.execChild(ctx, parent, a, sw, t.Task)
}

// execChild runs (or continues) a child agent with a live slot, records its
// result, and parks it so its parent can resume it later.
func (r *Runtime) execChild(ctx context.Context, parent, a *Agent, sw *sessWriter, task string) ChildResult {
	node := a.cfg.Node
	defer sw.close()
	if err := r.acquireLive(ctx); err != nil {
		node.setStatus("cancelled")
		return ChildResult{ID: node.ID, Model: node.Model, Cwd: a.cfg.Cwd, Session: sw.path(), Status: "cancelled", Report: "cancelled while queued"}
	}
	a.liveSlot = true
	a.cfg.Deadline = time.Time{}
	d := r.Config.Agent.ChildTimeoutDuration()
	if m := time.Duration(a.maxMinutes) * time.Minute; m > 0 && (d == 0 || m < d) {
		d = m
	}
	if d > 0 {
		a.cfg.Deadline = time.Now().Add(d) // time spent queued does not count
	}
	began, turnsBefore, toolsBefore := time.Now(), a.Turns, a.ToolCount
	final, err := a.Run(ctx, task)
	a.liveSlot = false
	r.releaseLive()
	res := ChildResult{ID: node.ID, Model: node.Model, Cwd: a.cfg.Cwd, Session: sw.path(),
		Status: childStatus(ctx, err), Turns: a.Turns - turnsBefore, Tools: a.ToolCount - toolsBefore, Usage: a.Usage, Report: final, Denied: a.Denied,
		MaxTurns: a.cfg.MaxTurns, Wall: time.Since(began)}
	if err != nil && !errors.Is(err, ErrTurnOut) {
		res.Report = "error: " + err.Error()
		if t := strings.TrimSpace(a.lastText()); t != "" {
			res.Report += "\nLast assistant text:\n" + t
		}
		res.Report += "\n" + a.toolSummary()
	}
	rec := session.Record{Type: "result", Run: r.RunID, Agent: node.ID, Model: node.Model, Status: res.Status, Turns: a.Turns, Usage: &res.Usage}
	if err != nil {
		rec.Error = err.Error()
	}
	sw.append(rec)
	if a.routeNote != "" {
		res.Report = "(" + a.routeNote + ")\n" + res.Report
		a.routeNote = ""
	}
	r.finishWriter(node, &res)
	if res.Status != "cancelled" {
		r.park(parent, a, sw)
		if res.Status == "turn-out" {
			res.Report += "\n\n(resume_agent can continue " + node.ID + " with more turns and its history.)"
		}
	}
	return res
}

// sessWriter appends to a child's session file and can reopen it when the
// child is resumed.
type sessWriter struct {
	mu sync.Mutex
	s  *session.Store
	p  string
}

func (w *sessWriter) append(r session.Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.s != nil {
		_ = w.s.Append(r)
	}
}
func (w *sessWriter) path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.s != nil {
		return w.s.Path()
	}
	return w.p
}
func (w *sessWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.s != nil {
		w.p = w.s.Path()
		_ = w.s.Close()
		w.s = nil
	}
}
func (w *sessWriter) reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, err := session.Open(w.p)
	if err != nil {
		return err
	}
	w.s = s
	return nil
}

// parked is a finished child its parent may resume.
type parked struct {
	agent  *Agent
	sw     *sessWriter
	parent string
}

func (r *Runtime) park(parent, a *Agent, sw *sessWriter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.parkedKids == nil {
		r.parkedKids = map[string]*parked{}
	}
	r.parkedKids[a.cfg.Node.ID] = &parked{agent: a, sw: sw, parent: parent.cfg.Node.ID}
}

// ResumeChild continues a parked child of parent with a new message and a
// fresh turn budget (Config.Resume).
func (r *Runtime) ResumeChild(ctx context.Context, parent *Agent, id, task string, maxTurns int) ChildResult {
	r.mu.Lock()
	p := r.parkedKids[id]
	if p != nil && p.parent == parent.cfg.Node.ID {
		delete(r.parkedKids, id)
	}
	r.mu.Unlock()
	if p == nil || p.parent != parent.cfg.Node.ID {
		return ChildResult{ID: id, Status: "error", Report: "resume error: " + id + " is not a finished child of " + parent.cfg.Node.ID}
	}
	a := p.agent
	if a.cfg.Node.Role == "oracle" { // a resumed oracle spends like a new one: same gate
		if err := oracleGate(ctx, r.Config, parent); err != nil {
			r.mu.Lock()
			r.parkedKids[id] = p // still resumable once the gate allows it
			r.mu.Unlock()
			return ChildResult{ID: id, Status: "error", Report: "resume error: " + err.Error()}
		}
	}
	if wt := r.writer(id); wt != nil {
		_ = wt.MarkActive() // the same single writer continues in its own worktree
	}
	if err := p.sw.reopen(); err != nil {
		return ChildResult{ID: id, Status: "error", Report: "resume error: " + err.Error()}
	}
	a.cfg.MaxTurns = r.Config.Agent.ChildTurns(a.cfg.Node.Role, a.cfg.Node.Depth, maxTurns)
	if strings.TrimSpace(task) == "" {
		task = "Continue your task from where you stopped. Your earlier turns are above; do not redo finished work."
	}
	return r.execChild(ctx, parent, a, p.sw, task)
}
func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%x", time.Now().Format("20060102-150405"), b)
}

func usableRoutes(routes []string) []string {
	var out []string
	for _, r := range routes {
		if strings.HasPrefix(r, "goat:") {
			continue
		}
		prov, model, ok := strings.Cut(r, ":")
		if !ok || prov == "" || model == "" {
			continue
		}
		// Filter default unsupported transports:
		// OpenCode Go chat client does not support Responses protocol required by gpt-6-luna.
		if prov == "opencode-go" && (model == "gpt-6-luna" || model == "gpt-luna") {
			continue
		}
		// Gemini must never route through OpenCode Go.
		if prov == "opencode-go" && strings.Contains(model, "gemini") {
			continue
		}
		out = append(out, r)
	}
	return out
}
func routeRef(route string) string { p, m, _ := strings.Cut(route, ":"); return p + "/" + m }
func maxTurnsCap() int {
	c, err := config.Load()
	if err != nil || c.Agent.MaxTurnsCap < 1 {
		return config.Default().Agent.MaxTurnsCap
	}
	return c.Agent.MaxTurnsCap
}
