package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/perm/model"
	"github.com/unix/ugh/internal/policy"
	"github.com/unix/ugh/internal/provider"
)

// routeAccess reports local access evidence for a provider/model ref (tests replace it).
var routeAccess = provider.ModelAccess

// routeEvidence records routes that completed a request in this process: the
// only detected (not configured) proof of entitlement the harness has.
var routeEvidence sync.Map

func noteRouteSuccess(ref string) { routeEvidence.Store(ref, struct{}{}) }

// delegationAccess evaluates access for automatic delegation. Unknown access
// is only promoted by evidence of use or the explicit trust_credentials opt-in;
// exhaustion, denial and missing credentials are never promoted.
func delegationAccess(cfg config.Config) policy.AccessEvaluator {
	var mu sync.Mutex
	type res struct {
		st  policy.AccessStatus
		why string
	}
	memo := map[string]res{}
	return policy.FuncAccessEvaluator(func(prov, id string) (policy.AccessStatus, string) {
		ref := prov + "/" + id
		mu.Lock()
		defer mu.Unlock()
		if r, ok := memo[ref]; ok {
			return r.st, r.why
		}
		st, why := routeAccess(ref)
		if st == policy.AccessUnknown {
			if _, used := routeEvidence.Load(ref); used {
				st, why = policy.AccessGranted, "route completed a request this session"
			} else if cfg.Routing.TrustCredentials {
				st, why = policy.AccessGranted, "credential present; routing.trust_credentials opt-in"
			}
		}
		memo[ref] = res{st, why}
		return st, why
	})
}

// tierChain is the configured family chain for a tier: [routing.tiers] names
// its first family and [routing.fallback] links each family to the next.
// Unknown ids are skipped (MigrateConfig warns about them).
func tierChain(cfg config.Config, tier policy.Tier) []policy.Family {
	var out []policy.Family
	seen := map[string]bool{}
	for id := cfg.Routing.Tiers[string(tier)]; id != "" && !seen[id]; id = cfg.Routing.Fallback[id] {
		seen[id] = true
		if f, ok := policy.FamilyByID(id); ok {
			out = append(out, f)
		}
	}
	return out
}

// tierFor is the first tier whose chain contains fam (oracle for the oracle role).
func tierFor(cfg config.Config, fam policy.Family, role string) policy.Tier {
	if role == "oracle" {
		return policy.TierOracle
	}
	for _, tier := range orderedTiers {
		for _, f := range tierChain(cfg, tier) {
			if f == fam {
				return tier
			}
		}
	}
	return policy.Families[fam].DelegatedTier
}

// orderedTiers lists the delegation tiers in router-offer order.
var orderedTiers = []policy.Tier{policy.TierLow, policy.TierHigh}

// eligibleTiers lists the ordinary tiers that currently have at least one
// eligible route. Oracle is a role workflow, never offered here.
func eligibleTiers(cfg config.Config, role string, eval policy.AccessEvaluator) []policy.Tier {
	var out []policy.Tier
	for _, tier := range orderedTiers {
		if len(policy.DelegatedFallbackChain(tierChain(cfg, tier), role, eval)) > 0 {
			out = append(out, tier)
		}
	}
	return out
}

// RoutingUnavailableError reports that neither decision router could answer a
// managed spawn. Nothing was launched.
type RoutingUnavailableError struct {
	Reason   string
	Cooldown time.Duration
}

func (e *RoutingUnavailableError) Error() string { return "routing unavailable: " + e.Reason }

// decided is one router answer with the router that gave it.
type decided struct {
	Value    string
	Router   string
	Fallback string // why the preferred router was not used, if it was not
}

// decideRouter asks the preferred router, then the other, and reports who answered.
var decide = decideRouter

func decideRouter(ctx context.Context, cfg config.Config, task, instructions string, opts map[string]string) (decided, error) {
	if len(opts) == 0 {
		return decided{}, fmt.Errorf("no options")
	}
	preferred, other := cfg.Routing.PreferredRouter, cfg.Routing.FallbackRouter
	if preferred == "" {
		preferred = "jev"
	}
	if other == "" {
		other = "clef"
	}
	var why []string
	for _, name := range []string{preferred, other} {
		rc, ok := cfg.Routing.Routers[name]
		if !ok || rc.BaseURL == "" {
			why = append(why, name+": not configured")
			continue
		}
		timeout := 3 * time.Second
		if rc.TimeoutMS > 0 {
			timeout = time.Duration(rc.TimeoutMS) * time.Millisecond
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		client := model.Client{
			BaseURL: rc.BaseURL, Path: rc.Path, Model: rc.Model, SessionHeader: rc.SessionHeader,
			Key: func(context.Context) (string, error) {
				store, e := auth.Open(cfg.Auth.Store)
				if e != nil {
					return "", e
				}
				keyProv := "openrouter"
				if rc.KeyEnv != "" && !strings.Contains(rc.KeyEnv, "OPENROUTER") {
					keyProv = strings.ToLower(strings.TrimSuffix(rc.KeyEnv, "_API_KEY"))
				}
				return auth.Resolve(keyProv, os.LookupEnv, store)
			},
		}
		res, err := client.Ask(callCtx, map[string]string{"task": task}, map[string]model.Question{"pick": model.Choice(instructions, opts)})
		cancel()
		if err == nil {
			v, _ := res["pick"].Answer.(string)
			if _, ok := opts[v]; ok {
				d := decided{Value: v, Router: name}
				if name != preferred {
					d.Fallback = strings.Join(why, "; ")
				}
				return d, nil
			}
			err = errors.New("invalid choice")
		}
		why = append(why, name+": "+err.Error())
	}
	return decided{}, errors.New(strings.Join(why, "; "))
}

// routeChoice is the outcome of routing one spawn: everything the harness
// (never the spawning model) decided.
type routeChoice struct {
	Role, Thinking string
	Tier           policy.Tier
	Routes         []policy.Route // complete ordered chain, already eligible
	Mode           string         // managed | table | traditional
	Router         string
	Fallback       string // preferred router unavailable, and why
	Notes          []string
}

func (c routeChoice) describe() string {
	var b strings.Builder
	switch {
	case c.Mode == "traditional":
		b.WriteString("selected by the caller (routers unavailable)")
	case c.Mode == "table":
		b.WriteString("table routing")
	default:
		b.WriteString("routed by " + c.Router)
	}
	if c.Fallback != "" {
		b.WriteString(" (preferred router unavailable: " + c.Fallback + ")")
	}
	fmt.Fprintf(&b, " · role %s · tier %s · thinking %s", c.Role, c.Tier, orDash(c.Thinking))
	if len(c.Routes) > 0 {
		b.WriteString(" · route " + c.Routes[0].Ref())
	}
	for _, n := range c.Notes {
		b.WriteString(" · " + n)
	}
	return b.String()
}

func (c routeChoice) meta() map[string]any {
	refs := make([]string, len(c.Routes))
	for i, r := range c.Routes {
		refs[i] = r.Ref()
	}
	return map[string]any{"mode": c.Mode, "router": c.Router, "fallback": c.Fallback, "role": c.Role,
		"tier": string(c.Tier), "thinking": c.Thinking, "routes": refs, "notes": c.Notes}
}

// planManaged lets the configured router pick role, tier and thinking from the
// eligible sets; deterministic policy supplies the route chain. When a router
// is needed and none answers, it returns *RoutingUnavailableError and nothing
// is defaulted.
func (r *Runtime) planManaged(ctx context.Context, task, explicitRole string, roles []string) (routeChoice, error) {
	cfg := r.Config
	eval := delegationAccess(cfg)
	table := cfg.Routing.PickModel == "table"
	out := routeChoice{Mode: "managed"}
	if table {
		out.Mode = "table"
	}
	var routerErr []string
	note := func(d decided) {
		if d.Router != "" && out.Router == "" {
			out.Router = d.Router
		}
		if d.Fallback != "" {
			out.Fallback = d.Fallback
		}
	}

	// Recheck at admission: if the picked tier loses every route between the
	// decision and the chain build, reroute over the reduced set (bounded).
	for attempt := 0; attempt < 2; attempt++ {
		role := explicitRole
		if role == "" {
			opts := map[string]string{}
			for _, id := range roles {
				if c, e := readCard("roles", id); e == nil {
					opts[id] = c.Body
				}
			}
			switch {
			case table || len(opts) == 1:
				role = "scout"
			default:
				d, err := decide(ctx, cfg, task, "Which role best fits this task?", opts)
				if err != nil {
					routerErr = append(routerErr, err.Error())
					return out, &RoutingUnavailableError{Reason: strings.Join(routerErr, "; "), Cooldown: r.cooldown()}
				}
				note(d)
				role = d.Value
			}
		}
		out.Role = role

		var tier policy.Tier
		if role == "oracle" {
			tier = policy.TierOracle
		} else {
			tiers := eligibleTiers(cfg, role, eval)
			if len(tiers) == 0 {
				return out, errors.New(noRouteHint(cfg))
			}
			if !containsTier(tiers, policy.TierHigh) {
				out.Notes = appendUnique(out.Notes, "no high-tier route available: delegation limited to low tier")
			}
			switch {
			case len(tiers) == 1:
				tier = tiers[0]
			case table:
				tier = policy.TierLow
			default:
				opts := map[string]string{"low": "simple, narrow", "high": "difficult, ambiguous"}
				d, err := decide(ctx, cfg, task, "Choose complexity tier for this task.", opts)
				if err != nil {
					return out, &RoutingUnavailableError{Reason: err.Error(), Cooldown: r.cooldown()}
				}
				note(d)
				tier = policy.Tier(d.Value)
			}
		}
		out.Tier = tier
		out.Routes = policy.DelegatedFallbackChain(tierChain(cfg, tier), role, delegationAccess(cfg)) // fresh access: recheck at admission
		eval = delegationAccess(cfg)
		if len(out.Routes) == 0 {
			if role == "oracle" {
				return out, errors.New("no eligible oracle route (Astra on Codex, then Fable): " + noRouteHint(cfg))
			}
			explicitRole = role
			continue // tier vanished: reroute over the reduced eligible set
		}
		break
	}
	if len(out.Routes) == 0 {
		return out, errors.New("no eligible route after rerouting: " + noRouteHint(cfg))
	}

	// Thinking: the router picks among the levels the first route supports.
	levels, known := ModelThinking(out.Routes[0].Ref())
	switch {
	case table:
		out.Thinking = clampLevel(levels, DefaultThinking)
	case !known:
		out.Thinking = DefaultThinking // levels unverified: passed through, not confirmed
		out.Notes = append(out.Notes, "thinking levels unverified for "+out.Routes[0].Ref())
	case len(levels) == 1:
		out.Thinking = levels[0]
	default:
		opts := map[string]string{}
		for _, l := range levels {
			opts[l] = "thinking effort " + l
		}
		d, err := decide(ctx, cfg, task, "Choose the thinking effort for this task.", opts)
		if err != nil {
			return out, &RoutingUnavailableError{Reason: err.Error(), Cooldown: r.cooldown()}
		}
		note(d)
		out.Thinking = d.Value
	}
	return out, nil
}

// DefaultThinking is the thinking level when nobody chose one: the root agent
// and subagents the router did not set a level for.
const DefaultThinking = "high"

func containsTier(ts []policy.Tier, t policy.Tier) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

func noRouteHint(cfg config.Config) string {
	h := "no eligible delegated route. Automatic delegation needs confirmed access: set [routing.access] <provider> = \"granted\" " +
		"for providers you pay for, or routing.trust_credentials = true to trust stored credentials"
	if cfg.Routing.TrustCredentials {
		h = "no eligible delegated route: providers are missing credentials, exhausted, reserved, or unsupported (see /usage)"
	}
	return h
}

// planTraditional resolves a caller-selected tier under the same eligibility
// predicate as managed routing. It is reachable only while routers are
// unavailable: the caller stands in for the router's tier decision, and the
// tier's chain decides the models exactly as in managed routing.
func (r *Runtime) planTraditional(task Task, role string) (routeChoice, error) {
	eval := delegationAccess(r.Config)
	tier := policy.Tier(strings.ToLower(strings.TrimSpace(task.Tier)))
	if role == "oracle" {
		tier = policy.TierOracle
	} else if !containsTier(eligibleTiers(r.Config, role, eval), tier) {
		return routeChoice{}, fmt.Errorf("tier %q is not available for role %s; choose one of: %s", task.Tier, role, strings.Join(traditionalChoices(r.Config, role), ", "))
	}
	routes := policy.DelegatedFallbackChain(tierChain(r.Config, tier), role, eval)
	if len(routes) == 0 {
		return routeChoice{}, errors.New(noRouteHint(r.Config))
	}
	out := routeChoice{Mode: "traditional", Role: role, Tier: tier, Routes: routes}
	levels, known := ModelThinking(routes[0].Ref())
	want := task.Thinking
	if want == "" {
		want = DefaultThinking
	}
	if known {
		out.Thinking = clampLevel(levels, want)
		if out.Thinking != want {
			out.Notes = append(out.Notes, fmt.Sprintf("thinking %s requested, %s applied", want, out.Thinking))
		}
	} else {
		out.Thinking = want
		out.Notes = append(out.Notes, "thinking levels unverified for "+routes[0].Ref())
	}
	return out, nil
}

// traditionalChoices lists the tiers a caller may pick for role right now,
// each with the model it would start on.
func traditionalChoices(cfg config.Config, role string) []string {
	eval := delegationAccess(cfg)
	var out []string
	tiers := append([]policy.Tier(nil), orderedTiers...)
	if role == "oracle" {
		tiers = []policy.Tier{policy.TierOracle}
	}
	for _, tier := range tiers {
		if chain := policy.DelegatedFallbackChain(tierChain(cfg, tier), role, eval); len(chain) > 0 {
			rt := chain[0]
			out = append(out, fmt.Sprintf("%s (%s via %s)", tier, policy.Families[rt.Family].CardID, rt.Provider))
		}
	}
	return out
}

// cooldown is how long traditional selection stays open after a router outage.
func (r *Runtime) cooldown() time.Duration {
	if s := r.Config.Routing.TraditionalCooldownS; s > 0 {
		return time.Duration(s) * time.Second
	}
	return 2 * time.Minute
}

// traditional reports whether model-selectable delegation is currently open.
func (r *Runtime) traditional() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Before(r.tradUntil)
}

func (r *Runtime) openTraditional(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tradUntil = time.Now().Add(d)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
