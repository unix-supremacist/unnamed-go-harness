package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/policy"
	"github.com/unix/ugh/internal/session"
)

// panelPolicyFamily maps the council panel families onto policy families.
var panelPolicyFamily = map[PanelFamily]policy.Family{
	FamilyLuna:          policy.FamilyGPTLuna,
	FamilyMiMoFlash:     policy.FamilyMiMoFlash,
	FamilyDeepSeekFlash: policy.FamilyDeepSeekFlash,
	FamilyGLMFlash:      policy.FamilyGLMFlash,
	FamilyQwen:          policy.FamilyQwen,
	FamilyHaiku:         policy.FamilyClaudeHaiku,
	FamilyGemini:        policy.FamilyGeminiFlash,
}

// familyRoutes lists the eligible routes of one policy family in provider
// order for a council purpose.
func familyRoutes(fam policy.Family, purpose policy.Purpose, oracleLed bool, eval policy.AccessEvaluator) []policy.Route {
	var out []policy.Route
	for _, p := range policy.Families[fam].ProviderOrder {
		r, ok := policy.ConcreteRoutes[fam][p]
		if !ok {
			continue
		}
		st, why := eval.CheckAccess(p, r.ModelID)
		if policy.CheckEligibility(policy.EligibilityRequest{Purpose: purpose, Family: fam, Provider: p, ModelID: r.ModelID,
			Transport: r.Transport, Access: st, AccessReason: why, OracleLed: oracleLed}).Eligible {
			out = append(out, r)
		}
	}
	return out
}

func chainRoutes(fams []policy.Family, purpose policy.Purpose, oracleLed bool, eval policy.AccessEvaluator) []policy.Route {
	var out []policy.Route
	for _, f := range fams {
		out = append(out, familyRoutes(f, purpose, oracleLed, eval)...)
	}
	return out
}

func routeKeys(rs []policy.Route) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.RouteKey()
	}
	return out
}

// panelAvailability returns the eligible routes per panel family. A family
// with no route is omitted, never replaced by another family.
func panelAvailability(cfg config.Config, eval policy.AccessEvaluator) map[PanelFamily][]policy.Route {
	out := map[PanelFamily][]policy.Route{}
	cc := councilConfig()
	custom := !equalStrings(cc.Panel, config.Default().Council.Panel) && len(cc.Panel) > 0
	allowed := map[PanelFamily]bool{}
	for _, m := range cc.Panel {
		if f, ok := PanelFamilyForModel(m); ok {
			allowed[f] = true
		}
	}
	for _, f := range DefaultPanelFamilies {
		if custom && !allowed[f] {
			continue
		}
		if rs := familyRoutes(panelPolicyFamily[f], policy.PurposeCouncilPanel, false, eval); len(rs) > 0 {
			out[f] = rs
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedPanelFamilies(m map[PanelFamily][]policy.Route) []PanelFamily {
	var out []PanelFamily
	for _, f := range DefaultPanelFamilies {
		if _, ok := m[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

// councilAvailable is the cheap, spend-free preflight: enough panel families
// to meet the quorum, and some way to coordinate/synthesize.
func councilAvailable(cfg config.Config) bool {
	eval := delegationAccess(cfg)
	return len(panelAvailability(cfg, eval)) >= MinCouncilQuorumFamilies && len(coordinatorOptions(eval).all()) > 0
}

// ---- coordinator ----

// coordSpec is the committed synthesis strategy: who coordinates and
// synthesizes, and in what order they fall back.
type coordSpec struct {
	Strategy SynthesisStrategy
	Routes   []policy.Route
}

type coordOptions struct{ normal, economical, oracle []policy.Route }

func (o coordOptions) all() []policy.Route {
	return append(append(append([]policy.Route{}, o.normal...), o.economical...), o.oracle...)
}

func coordinatorOptions(eval policy.AccessEvaluator) coordOptions {
	return coordOptions{
		normal:     chainRoutes(coordinatorChain(councilConfig().Aggregator), policy.PurposeCouncilCoord, false, eval),
		economical: chainRoutes([]policy.Family{policy.FamilyClaudeHaiku, policy.FamilyGPTLuna, policy.FamilyDeepSeekFlash}, policy.PurposeCouncilSynth, false, eval),
		oracle:     chainRoutes([]policy.Family{policy.FamilyGPTAstra, policy.FamilyClaudeFable}, policy.PurposeCouncilSynth, true, eval),
	}
}

// coordinatorChain puts the configured aggregator first, then the rest of the
// built-in coordinator chain.
func coordinatorChain(aggregator string) []policy.Family {
	chain := policy.CouncilCoordinatorChain()
	f, ok := policy.FamilyByID(aggregator)
	if !ok {
		return chain
	}
	out := []policy.Family{f}
	for _, c := range chain {
		if c != f {
			out = append(out, c)
		}
	}
	return out
}

// ErrNoCoordinator means no coordinator or synthesizer route exists at all:
// the council is genuinely unavailable.
var ErrNoCoordinator = errors.New("council: no coordinator or synthesizer route is available")

// StrategyRequiredError is returned (before any spend) when the normal
// coordinator chain is gone and no router can choose between the exceptional
// strategies; the caller must select one explicitly.
type StrategyRequiredError struct{ Options []string }

func (e *StrategyRequiredError) Error() string {
	return "council synthesis strategy needs an explicit choice (neither router is available): re-run council with strategy set to one of " +
		strings.Join(e.Options, ", ")
}

// chooseCoordinator commits the synthesis strategy. The normal chain needs no
// decision; otherwise the router picks, or (router outage) the caller must pass
// an explicit strategy while traditional selection is open.
func chooseCoordinator(ctx context.Context, parent *Agent, cfg config.Config, question, explicit string, opts coordOptions) (coordSpec, error) {
	if len(opts.normal) > 0 {
		if explicit != "" {
			return coordSpec{}, errors.New("council: strategy applies only when the normal coordinator chain is unavailable")
		}
		return coordSpec{StrategyNormal, opts.normal}, nil
	}
	avail := map[string]coordSpec{}
	desc := map[string]string{}
	if len(opts.economical) > 0 {
		avail["economical"] = coordSpec{StrategyEconomical, opts.economical}
		desc["economical"] = "synthesize cheaply with Haiku, Luna or DeepSeek Flash"
	}
	if len(opts.oracle) > 0 {
		avail["oracle-led"] = coordSpec{StrategyOracleLed, opts.oracle}
		desc["oracle-led"] = "an oracle model (Astra, else Fable) coordinates and synthesizes; for a sufficiently demanding task"
	}
	if len(avail) == 0 {
		return coordSpec{}, ErrNoCoordinator
	}
	rt := parent.rt
	if explicit != "" {
		if rt == nil || !rt.traditional() {
			return coordSpec{}, errors.New("council: strategy is chosen by the router; it can be passed only while routing is unavailable")
		}
		c, ok := avail[explicit]
		if !ok {
			return coordSpec{}, &StrategyRequiredError{Options: sortedStrings(avail)}
		}
		return c, nil
	}
	pick := map[string]string{}
	for k, v := range desc {
		pick[k] = v
	}
	if len(avail) == 1 && avail["oracle-led"].Routes != nil {
		pick["stop"] = "do not run the council: the task is not demanding enough to justify an oracle-led synthesis"
	}
	d, err := decide(ctx, cfg, question, "The normal council coordinator chain is unavailable. Choose the synthesis strategy.", pick)
	if err != nil {
		if rt != nil {
			rt.openTraditional(rt.cooldown())
		}
		return coordSpec{}, &StrategyRequiredError{Options: sortedStrings(avail)}
	}
	if d.Value == "stop" {
		return coordSpec{}, errors.New("council not run: the router judged the task not demanding enough for an oracle-led synthesis")
	}
	return avail[d.Value], nil
}

func sortedStrings[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- running scouts on policy routes ----

type scoutReq struct {
	Routes                     []string // provider:model keys, in order
	System, Task, Role         string
	MaxTurns                   int
	Admission                  Admission
	QueueDeadline, ExecTimeout time.Duration
}

// runRoutes runs one read-only scout over an ordered route chain. The queue
// deadline bounds waiting for admission; the execution timeout starts only
// once a request is admitted.
func runRoutes(ctx context.Context, root *Agent, q scoutReq) (string, llm.Usage, error) {
	if len(q.Routes) == 0 {
		return "", llm.Usage{}, errors.New("no route")
	}
	var (
		prov llm.Provider
		id   string
		ref  string
		err  error
		rest []string
	)
	for i, key := range q.Routes {
		ref = routeRef(key)
		if prov, id, err = resolveRoute(ref); err == nil {
			rest = q.Routes[i:]
			break
		}
	}
	if err != nil {
		return "", llm.Usage{}, err
	}
	node := root.cfg.Tree.Child(root.cfg.Node, q.Role, ref, root.cfg.Cwd)
	node.setStatus("running")
	sess, err := session.Create(root.cfg.Cwd, session.Record{Model: ref, Cwd: root.cfg.Cwd, Parent: root.cfg.Node.ID, Agent: node.ID})
	if err != nil {
		node.setStatus("error")
		return "", llm.Usage{}, err
	}
	defer sess.Close()
	thinking := "low"
	if levels, known := ModelThinking(ref); known {
		thinking = clampLevel(levels, thinking)
	}

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reason atomic.Value
	var once sync.Once
	var queueT, execT *time.Timer
	if q.QueueDeadline > 0 {
		queueT = time.AfterFunc(q.QueueDeadline, func() { reason.Store("queue deadline exceeded before admission"); cancel() })
	}
	admitted := func() {
		once.Do(func() {
			if queueT != nil {
				queueT.Stop()
			}
			if q.ExecTimeout > 0 {
				execT = time.AfterFunc(q.ExecTimeout, func() { reason.Store("execution timeout"); cancel() })
			}
		})
	}
	a := New(Config{
		Provider: prov, Model: id, System: q.System, Thinking: thinking,
		Tools: readOnlyTools(), Cwd: root.cfg.Cwd, Gate: root.cfg.Gate,
		Agent: node.ID, Tree: root.cfg.Tree, Node: node, Routes: rest,
		StreamLimit: root.cfg.StreamLimit, MaxTurns: q.MaxTurns, Admission: q.Admission, OnAdmitted: admitted,
		OnMessage: func(m llm.Message) { _ = sess.Append(session.Record{Type: "message", Message: &m}) },
	})
	final, runErr := a.Run(rctx, q.Task)
	if queueT != nil {
		queueT.Stop()
	}
	if execT != nil {
		execT.Stop()
	}
	if runErr != nil {
		node.setStatus("error")
		if r, ok := reason.Load().(string); ok && ctx.Err() == nil {
			runErr = errors.New(r)
		}
		return "", a.Usage, runErr
	}
	node.setStatus("done")
	return final, a.Usage, nil
}

// ---- outcomes, records, approvals ----

func newCouncilID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "council-" + hex.EncodeToString(b)
}

// rootAgentOf returns the root of an agent's tree (councils are convened and
// remembered there).
func rootAgentOf(a *Agent) *Agent {
	n := a.cfg.Node
	for n.Parent != nil {
		n = n.Parent
	}
	n.tree.mu.Lock()
	ag := n.agent
	n.tree.mu.Unlock()
	if ag == nil {
		return a
	}
	return ag
}

// councilTaskID identifies the task a council answers: approvals and oracle
// escalation are bound to it, never to another question.
func councilTaskID(a *Agent) string {
	root := rootAgentOf(a)
	h := sha256.Sum256([]byte(strings.TrimSpace(root.task)))
	return hex.EncodeToString(h[:6])
}

func (a *Agent) rememberCouncil(m *CouncilRecordMeta) {
	a.councilMu.Lock()
	defer a.councilMu.Unlock()
	if a.councils == nil {
		a.councils = map[string]*CouncilRecordMeta{}
	}
	a.councils[m.CouncilID] = m
}

func (a *Agent) council(id string) *CouncilRecordMeta {
	a.councilMu.Lock()
	defer a.councilMu.Unlock()
	return a.councils[id]
}

// councilsForTask lists the councils convened for a task, newest first.
func (a *Agent) councilsForTask(taskID string) []*CouncilRecordMeta {
	a.councilMu.Lock()
	defer a.councilMu.Unlock()
	var out []*CouncilRecordMeta
	for _, m := range a.councils {
		if m.TaskID == taskID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// persistCouncil stores the outcome in the existing session records.
func (a *Agent) persistCouncil(m *CouncilRecordMeta) {
	a.rememberCouncil(m)
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil {
		return
	}
	delete(meta, "approval") // approvals are never replayed as authority
	a.emitRecord(session.Record{Type: "council", Agent: a.cfg.Node.ID, Status: string(m.Outcome), Meta: meta})
}

// RestoreCouncil rebuilds a council outcome from its session record on
// resume. Approvals are deliberately dropped: authority is revalidated.
func (a *Agent) RestoreCouncil(rec session.Record) {
	if rec.Type != "council" || rec.Meta == nil {
		return
	}
	raw, err := json.Marshal(rec.Meta)
	if err != nil {
		return
	}
	var m CouncilRecordMeta
	if json.Unmarshal(raw, &m) != nil || m.CouncilID == "" {
		return
	}
	m.Approval = nil
	a.rememberCouncil(&m)
}

type approver interface{ Approve(perm.Call) bool }

// approveEscalation asks the human directly (bypassing auto modes and the
// session allowlist) whether to spend on a direct oracle after a council.
// Headless runs, with nobody to ask, are refused.
func approveEscalation(ctx context.Context, a *Agent, m *CouncilRecordMeta, why string) error {
	ap, ok := a.cfg.Gate.(approver)
	if !ok {
		return fmt.Errorf("%w: no interactive approver is attached; direct oracle escalation after a council needs the user", ErrApprovalNotGranted)
	}
	args, _ := json.Marshal(map[string]string{"council_id": m.CouncilID, "task": m.TaskID, "reason": why})
	call := perm.Call{Agent: a.cfg.Agent, Cwd: a.cfg.Cwd, Tool: "council_escalate", Args: args, Task: a.task}
	if !ap.Approve(call) {
		return ErrApprovalNotGranted
	}
	m.Approval = &CouncilApproval{CouncilID: m.CouncilID, TaskID: m.TaskID, ReportRevisionHash: m.ReportRevisionHash,
		Approved: true, GrantedAt: time.Now(), ApprovedBy: "user", Reason: why}
	return nil
}

// oracleCheck is the non-prompting half of the oracle gate: nil error with a
// nil council means a direct oracle is allowed outright; a non-nil council
// means the user's task- and evidence-bound approval is still required.
func oracleCheck(cfg config.Config, parent *Agent) (*CouncilRecordMeta, error) {
	root := rootAgentOf(parent)
	for _, m := range root.councilsForTask(councilTaskID(parent)) {
		switch {
		case m.Outcome == StateUnavailableBeforeSpend:
			return nil, nil
		case m.Outcome.IsViable():
			return m, nil
		case m.Outcome == StatePanelFailed, m.Outcome == StateSynthesisFailed:
			return nil, fmt.Errorf("council %s ended %s with its evidence retained: retry the synthesis or revise the question; "+
				"direct oracle escalation is not automatic", m.CouncilID, m.Outcome)
		default:
			return nil, fmt.Errorf("council %s was %s: restart the task explicitly before using an oracle", m.CouncilID, m.Outcome)
		}
	}
	if councilAvailable(cfg) {
		return nil, errors.New("oracles come after council: run council on the question first (only the root can run council); " +
			"a direct oracle is available only when no council can run")
	}
	return nil, nil // council unavailable before spend: Astra → Fable directly
}

// oracleGate decides whether parent may spawn a direct oracle for its current
// task: only when no council can run (unavailable before spend), or after a
// viable council with the user's approval, bound to this task and report set.
func oracleGate(ctx context.Context, cfg config.Config, parent *Agent) error {
	m, err := oracleCheck(cfg, parent)
	if err != nil || m == nil {
		return err
	}
	root := rootAgentOf(parent)
	taskID := councilTaskID(parent)
	if err := approveEscalation(ctx, root, m, "council answer judged not good enough"); err != nil {
		return fmt.Errorf("a council already answered this task (%s); a direct oracle needs your approval: %w", m.CouncilID, err)
	}
	token := MintEscalationAuthority(m.CouncilID, taskID, m.ReportRevisionHash)
	return ValidateEscalationApproval(m.Approval, token, taskID, m.ReportRevisionHash, m.Outcome)
}

// ---- the engine ----

// councilOpts carries harness-validated inputs of one council call.
type councilOpts struct {
	Strategy string // explicit synthesis strategy (traditional selection only)
	Retry    string // council id whose synthesis to retry from retained reports
}

func councilCfgOf(parent *Agent) config.Config {
	if parent.rt != nil {
		return parent.rt.Config
	}
	c, _ := config.Load()
	return c
}

func queueDeadline(cc config.Council) time.Duration {
	if cc.QueueDeadlineS > 0 {
		return time.Duration(cc.QueueDeadlineS) * time.Second
	}
	return 10 * time.Minute
}

// councilRun runs a council end to end: preflight (no spend), commit the
// synthesis strategy, evidence and angles, bounded seat scheduling, viability
// reassessment, and synthesis. Every stage runs through the common request
// boundary; panel seats never carry the coordinator exemption.
func councilRun(ctx context.Context, parent *Agent, question string, anglesRaw json.RawMessage, research bool, opts councilOpts) (*Result, error) {
	q := strings.TrimSpace(question)
	if opts.Retry != "" {
		return councilRetry(ctx, parent, opts)
	}
	if q == "" {
		return nil, fmt.Errorf("council: a question is required")
	}
	cfg := councilCfgOf(parent)
	eval := delegationAccess(cfg)
	cc := councilConfig()
	root := rootAgentOf(parent)
	meta := &CouncilRecordMeta{CouncilID: newCouncilID(), TaskID: councilTaskID(parent), Question: q, CreatedAt: time.Now()}
	res := &Result{ID: meta.CouncilID}
	finish := func(state CouncilOutcomeState, err error) (*Result, error) {
		if ctx.Err() != nil && !state.IsViable() && state != StateUnavailableBeforeSpend {
			state = StateCancelled
		}
		meta.Outcome, meta.CompletedAt = state, time.Now()
		meta.Usage = &res.Usage
		res.Outcome = state
		root.persistCouncil(meta)
		return res, err
	}

	// 1. Preflight, before any paid work: quorum, coverage and size limits.
	n, lensList := angleSpec(anglesRaw, cc.DefaultAngles)
	panel := panelAvailability(cfg, eval)
	limits := CouncilLimits{MaxSeats: cc.MaxSeats, MaxReportContextWords: cc.MaxContextWords, MaxTotalWorkUnits: cc.MaxWorkUnits,
		MaxAngles: 12, MaxReportWordsPerSeat: cc.MaxReportWords}
	placeholder := make([]CouncilAngle, n)
	for i := range placeholder {
		placeholder[i] = CouncilAngle{ID: fmt.Sprintf("a%d", i+1)}
	}
	req := AllocationRequest{Question: q, Angles: placeholder, EligibleFamilies: sortedPanelFamilies(panel), Limits: limits}
	// Only genuine unavailability is recorded (and so lets a direct oracle
	// through). Bad requests and strategy errors return without an outcome.
	if _, err := PlanAllocation(req); err != nil {
		if errors.Is(err, ErrInsufficientQuorum) || errors.Is(err, ErrUncoveredAngle) {
			return finish(StateUnavailableBeforeSpend, fmt.Errorf("council unavailable before any spend: %w (available panel families: %d)", err, len(panel)))
		}
		return res, err
	}
	co, err := chooseCoordinator(ctx, parent, cfg, q, opts.Strategy, coordinatorOptions(eval))
	if err != nil {
		if errors.Is(err, ErrNoCoordinator) {
			return finish(StateUnavailableBeforeSpend, err)
		}
		return res, err
	}
	meta.Strategy, meta.SynthesisStrategy = co.Strategy, co.Strategy
	res.Strategy = string(co.Strategy)
	fmt.Fprintf(parent.cfg.Trace, "◈ council %s: strategy %s, coordinator %s, %d panel families\n", meta.CouncilID, co.Strategy, co.Routes[0].Ref(), len(panel))

	coord := func(system, task, role string, turns int) (string, llm.Usage, error) {
		ad := childAdmission(root.cfg.Admission, "council-coord")
		ad.Exemption = admission.ExemptionCouncilCoordinatorSol // minted here, in the council path only
		text, u, e := runRoutes(ctx, root, scoutReq{Routes: routeKeys(co.Routes), System: system, Task: task, Role: role, MaxTurns: turns,
			Admission: ad, QueueDeadline: queueDeadline(cc), ExecTimeout: time.Duration(cc.SeatTimeoutS) * time.Second * 2})
		res.Usage.In += u.In
		res.Usage.Out += u.Out
		return text, u, e
	}

	// 2. Optional evidence bundle, then angles, both by the coordinator.
	evidence := ""
	if research {
		text, _, err := coord(councilPrescoutPrompt, "Core question: "+q+"\n\nGather the evidence bundle.", "prescout", 12)
		if err != nil {
			return finish(StatePanelFailed, fmt.Errorf("council: prescout: %w", err))
		}
		evidence = clampWords(text, 1200)
	}
	meta.Evidence = evidence
	aggTask := fmt.Sprintf("N=%d\n\nCore question: %s\n\nLens vocabulary:\n%s", n, q, lensVocab(lensList))
	text, _, err := coord(councilAnglesPromptT, aggTask, "aggregator", 6)
	if err != nil {
		return finish(StatePanelFailed, fmt.Errorf("council: angles: %w", err))
	}
	parsed := parseAngles(text, n, lensList)
	if len(parsed) == 0 {
		return finish(StatePanelFailed, fmt.Errorf("council: the coordinator proposed no usable angles"))
	}
	angles := make([]CouncilAngle, len(parsed))
	for i, p := range parsed {
		angles[i] = CouncilAngle{ID: fmt.Sprintf("a%d", i+1), Text: p.text, Lens: p.lens}
	}

	// 3. Allocation with stable IDs, then bounded seat scheduling.
	req.Angles = angles
	plan, err := PlanAllocation(req)
	if err != nil { // after spend: evidence is the prescout/angles only, and nothing unlocks
		return finish(StatePanelFailed, err)
	}
	res.Plan = plan
	results := runSeats(ctx, root, plan, panel, evidence, cc)
	res.Seats = nil
	for i, r := range results {
		res.Seats = append(res.Seats, &Seat{Label: plan.AllocatedSeats[i].Label, Angle: angleText(plan, r.AngleID), Model: string(r.Family), Report: r.Report, Err: r.Err})
		if r.Err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s (angle %s, %s): %v", r.SeatID, r.AngleID, r.Family, r.Err))
		}
	}

	// 4. Reassess viability from what actually succeeded.
	as := ReassessPanelViability(plan, results)
	res.Assessment = &as
	meta.RetainedReports, meta.FailedSeatIDs = as.RetainedReports, as.FailedSeatIDs
	meta.SeatsTotal, meta.SeatsSucceeded, meta.SeatsFailed = as.TotalSeats, as.SucceededSeats, as.FailedSeats
	meta.ReportRevisionHash = ComputeReportRevisionHash(meta.TaskID, q, evidence, as.RetainedReports)
	if !as.Viable {
		return finish(StatePanelFailed, fmt.Errorf("council %s: %s (%d/%d seats, %d families, %d/%d angles); evidence retained, no automatic oracle escalation",
			meta.CouncilID, as.Reason, as.SucceededSeats, as.TotalSeats, as.DistinctFamilies, as.CoveredAngles, as.TotalAngles))
	}
	return synthesize(ctx, root, meta, res, co, as.State, coord, cc, plan, finish)
}

func angleText(plan *CouncilPlan, id string) string {
	for _, a := range plan.Angles {
		if a.ID == id {
			return a.Text
		}
	}
	return ""
}

// runSeats executes the allocated seats on at most MaxParallelSeats workers.
// Each seat tries its own family's providers in order; it never switches family.
func runSeats(ctx context.Context, root *Agent, plan *CouncilPlan, panel map[PanelFamily][]policy.Route, evidence string, cc config.Council) []SeatExecutionResult {
	results := make([]SeatExecutionResult, len(plan.AllocatedSeats))
	workers := cc.MaxParallelSeats
	if workers < 1 {
		workers = 6
	}
	if workers > len(results) {
		workers = len(results)
	}
	work := make(chan int, len(results))
	for i := range results {
		work <- i
	}
	close(work)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				s := plan.AllocatedSeats[i]
				r := SeatExecutionResult{SeatID: s.ID, AngleID: s.AngleID, Family: s.Family, ModelID: s.ModelID}
				if ctx.Err() != nil {
					r.Err = ctx.Err()
					results[i] = r
					continue
				}
				var angle CouncilAngle
				for _, a := range plan.Angles {
					if a.ID == s.AngleID {
						angle = a
					}
				}
				report, _, err := runRoutes(ctx, root, scoutReq{
					Routes: routeKeys(panel[s.Family]), System: councilPanelistPrompt,
					Task: seatTask(plan.Question, angle.Text, angle.Lens, evidence, len(results)), Role: "panelist", MaxTurns: 12,
					Admission:     childAdmission(root.cfg.Admission, "council-panel"), // never exempt
					QueueDeadline: queueDeadline(cc), ExecTimeout: time.Duration(cc.SeatTimeoutS) * time.Second})
				r.Err = err
				if err == nil {
					r.Report = clampWords(report, cc.MaxReportWords)
					r.WordCount = len(strings.Fields(r.Report))
				}
				if r.Err != nil {
					r.ErrorText = r.Err.Error()
				}
				results[i] = r
			}
		}()
	}
	wg.Wait()
	return results
}

// synthesize runs the coordinator's synthesis over the retained reports and
// records the outcome; a failure keeps the reports for a synthesis-only retry.
func synthesize(ctx context.Context, root *Agent, meta *CouncilRecordMeta, res *Result, co coordSpec, panelState CouncilOutcomeState,
	coord func(system, task, role string, turns int) (string, llm.Usage, error), cc config.Council, plan *CouncilPlan,
	finish func(CouncilOutcomeState, error) (*Result, error)) (*Result, error) {
	var reports []string
	words := 0
	for _, r := range meta.RetainedReports {
		body := clampWords(r.Text, cc.MaxReportWords)
		words += len(strings.Fields(body))
		reports = append(reports, fmt.Sprintf("## %s (angle %s)\n\n%s", r.Label, r.AngleID, body))
	}
	if cc.MaxContextWords > 0 && words > cc.MaxContextWords {
		return finish(StatePanelFailed, &OversizedRequestError{Metric: "synthesis_context_words", Limit: cc.MaxContextWords, Actual: words,
			Detail: "retained reports exceed the synthesis context limit; revise the request or raise council.max_context_words"})
	}
	failed := res.Failed
	syn, _, err := coord(councilSynthesisPrompt, synthesisTask(meta.Question, meta.Evidence, reports, failed), "synthesis", 12)
	if err != nil {
		return finish(StateSynthesisFailed, fmt.Errorf("council %s: synthesis failed (%v); the %d panel reports are retained: retry with council retry=%q",
			meta.CouncilID, err, len(meta.RetainedReports), meta.CouncilID))
	}
	res.Report = syn
	return finish(panelState, nil)
}

// councilRetry re-runs only the synthesis from retained reports (no new panel).
func councilRetry(ctx context.Context, parent *Agent, opts councilOpts) (*Result, error) {
	root := rootAgentOf(parent)
	prior := root.council(opts.Retry)
	if prior == nil {
		return nil, fmt.Errorf("council: unknown council %q (retry works only for a council of this session)", opts.Retry)
	}
	if prior.TaskID != councilTaskID(parent) {
		return nil, &ApprovalBindingError{Field: "task_id", Expected: councilTaskID(parent), Actual: prior.TaskID,
			Message: "that council answered a different task"}
	}
	if err := ValidateSynthesisRetryMeta(prior, prior.ReportRevisionHash); err != nil {
		return nil, err
	}
	cfg := councilCfgOf(parent)
	eval := delegationAccess(cfg)
	cc := councilConfig()
	co, err := chooseCoordinator(ctx, parent, cfg, prior.Question, opts.Strategy, coordinatorOptions(eval))
	if err != nil {
		return nil, err
	}
	if co.Strategy == StrategyOracleLed && prior.Strategy != StrategyOracleLed {
		// Prior council authorization is not blanket approval for newly added Astra/Fable spending.
		if err := approveEscalation(ctx, root, prior, "the original coordinator chain is gone; synthesis would now run on an oracle"); err != nil {
			return nil, fmt.Errorf("council retry would add oracle spending: %w", err)
		}
	}
	meta := *prior
	meta.Strategy, meta.SynthesisStrategy, meta.Outcome, meta.Approval = co.Strategy, co.Strategy, "", nil
	res := &Result{ID: meta.CouncilID, Strategy: string(co.Strategy), Failed: nil}
	finish := func(state CouncilOutcomeState, err error) (*Result, error) {
		meta.Outcome, meta.CompletedAt = state, time.Now()
		res.Outcome = state
		root.persistCouncil(&meta)
		return res, err
	}
	coord := func(system, task, role string, turns int) (string, llm.Usage, error) {
		ad := childAdmission(root.cfg.Admission, "council-coord")
		ad.Exemption = admission.ExemptionCouncilCoordinatorSol
		text, u, e := runRoutes(ctx, root, scoutReq{Routes: routeKeys(co.Routes), System: system, Task: task, Role: role, MaxTurns: turns,
			Admission: ad, QueueDeadline: queueDeadline(cc), ExecTimeout: 2 * time.Duration(cc.SeatTimeoutS) * time.Second})
		res.Usage.In += u.In
		res.Usage.Out += u.Out
		return text, u, e
	}
	state := StateCompleted
	if len(meta.FailedSeatIDs) > 0 {
		state = StateDegradedViable
	}
	return synthesize(ctx, root, &meta, res, co, state, coord, cc, nil, finish)
}
