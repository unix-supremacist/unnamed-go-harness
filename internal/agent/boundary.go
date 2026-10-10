package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
)

// Admission is the harness-set quota context for an agent's requests. It is
// never populated from model arguments or replayed session metadata.
type Admission struct {
	Manager   *admission.Manager // nil disables premium admission
	Priority  admission.Priority // root turns wait for the next slot; children fall back
	Exemption admission.Exemption
	Purpose   string
	// AccountID resolves the stable Codex account identity lazily (only for
	// metered requests). Empty means unknown: the conservative shared bucket.
	AccountID func() string
}

// drainTimeout bounds how long an attempt waits for its provider goroutine to
// stop after cancellation, before admission is released.
var drainTimeout = 2 * time.Second

// admit acquires the premium slot for one request attempt; the returned
// release is never nil.
func (a *Agent) admit(ctx context.Context) (func(), error) {
	ad := a.cfg.Admission
	if ad.Manager == nil {
		return func() {}, nil
	}
	prov, _, ok := strings.Cut(a.cfg.Node.Model, "/")
	if !ok {
		prov = a.cfg.ProviderName
	}
	req := admission.Request{Provider: prov, Model: a.cfg.Model, Priority: ad.Priority,
		Exemption: ad.Exemption, Purpose: ad.Purpose, RequestID: a.cfg.Agent}
	if req.Exemption != admission.ExemptionNone {
		// The council exemption covers Codex Sol only; other routes in the
		// coordinator's chain are admitted under ordinary rules.
		if c := admission.ClassifyModel(prov, a.cfg.Model); !(c.IsCodex && c.IsSol) {
			req.Exemption = admission.ExemptionNone
		}
	}
	if _, metered, err := admission.ClassifyRequest(req); err == nil && metered && ad.AccountID != nil {
		req.AccountID = ad.AccountID()
		if req.AccountID != "" { // unknown identity stays in the conservative shared bucket
			syncAdmissionLimits(ad.Manager, req.AccountID)
		}
	}
	h, err := ad.Manager.Acquire(ctx, req)
	if err != nil {
		return nil, err
	}
	return func() { _ = h.Close() }, nil
}

// codexAccountID reads the stable Codex account identity from the auth store.
func codexAccountID() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	store, err := auth.Open(cfg.Auth.Store)
	if err != nil {
		return ""
	}
	cred, ok, _ := store.Get("codex")
	if !ok {
		return ""
	}
	return cred.AccountID
}

// DefaultAdmission is the production admission context for a root or child.
func DefaultAdmission(p admission.Priority, purpose string) Admission {
	return Admission{Manager: admission.DefaultManager(), Priority: p, Purpose: purpose, AccountID: codexAccountID}
}

// drain consumes ch until the producer closes it or the timeout passes.
// Receiving an error event does not mean the producer stopped.
func drain(ch <-chan llm.Event) {
	t := time.NewTimer(drainTimeout)
	defer t.Stop()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-t.C:
			return
		}
	}
}

// childAdmission derives a child's quota context from its parent's: same
// manager, never the parent's priority or exemption.
func childAdmission(parent Admission, purpose string) Admission {
	parent.Priority, parent.Exemption, parent.Purpose = admission.PriorityChild, admission.ExemptionNone, purpose
	return parent
}

// adaptHistory serializes completed history for the destination route: an
// assistant turn produced by a different route loses its native/encrypted
// reasoning (it is only valid for the model that wrote it), but keeps its text
// and recorded tool calls, which are never executed again. msgs is not modified.
func adaptHistory(msgs []llm.Message, dest string) []llm.Message {
	var out []llm.Message
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant || m.Route == "" || m.Route == dest || (m.Native == "" && m.Reasoning == "") {
			if out != nil {
				out = append(out, m)
			}
			continue
		}
		if out == nil {
			out = append(make([]llm.Message, 0, len(msgs)), msgs[:i]...)
		}
		m.Native, m.Reasoning = "", ""
		out = append(out, m)
	}
	if out == nil {
		return msgs
	}
	return out
}

func (a *Agent) emitRecord(r session.Record) {
	if a.cfg.OnRecord != nil {
		a.cfg.OnRecord(r)
	}
}

var (
	limitSyncMu sync.Mutex
	limitSyncAt = map[string]time.Time{}
)

// syncAdmissionLimits makes the account-scoped admission config follow the
// user's [routing] chatgpt_plan and [council] exempt_sol_limit. The account
// file stays the single shared truth every process reads at admission; this
// only writes it when the user's settings changed (checked every few seconds).
func syncAdmissionLimits(m *admission.Manager, accountID string) {
	limitSyncMu.Lock()
	if time.Since(limitSyncAt[accountID]) < 5*time.Second {
		limitSyncMu.Unlock()
		return
	}
	limitSyncAt[accountID] = time.Now()
	limitSyncMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		return
	}
	preset, limit := plannedPreset(cfg)
	exempt := cfg.Council.ExemptSolLimit
	if exempt < 1 {
		exempt = 1
	}
	cur, err := m.GetConfig(accountID)
	if err != nil || (cur.Preset == preset && cur.CombinedLimit == limit && cur.CouncilExemptLimit == exempt) {
		return
	}
	cur.Preset, cur.CombinedLimit, cur.CouncilExemptLimit = preset, limit, exempt
	_ = m.SetConfig(accountID, cur)
}

// plannedPreset picks the burn-rate preset: an explicit routing.chatgpt_plan
// wins; "auto" (the default) detects it from the Codex login's plan claim and
// falls back to basic when the plan cannot be detected.
func plannedPreset(cfg config.Config) (admission.Preset, int) {
	if p := cfg.Routing.ChatGPTPlan; p != "" && p != "auto" {
		return admission.ParsePreset(p)
	}
	if store, err := auth.Open(cfg.Auth.Store); err == nil {
		if cred, ok, _ := store.Get("codex"); ok {
			if p, n, known := admission.PresetForPlanType(auth.CodexPlanType(cred.Access)); known {
				return p, n
			}
		}
	}
	return admission.PresetBasic, 1
}

// refreshAuth tries to renew credentials after an authentication failure
// (tests replace it). Only Codex OAuth is refreshable; it is rate-limited so a
// genuinely revoked login does not loop.
var refreshAuth = func(ctx context.Context, prov string) bool {
	if prov != "codex" {
		return false
	}
	refreshMu.Lock()
	if time.Since(lastRefresh) < time.Minute {
		refreshMu.Unlock()
		return false
	}
	lastRefresh = time.Now()
	refreshMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	store, err := auth.Open(cfg.Auth.Store)
	if err != nil {
		return false
	}
	return auth.CodexRefreshNow(ctx, store) == nil
}

var (
	refreshMu   sync.Mutex
	lastRefresh time.Time
)

// ambiguousResult is the stand-in result for a tool call whose outcome was
// never persisted (crash or kill mid-call). The call is never re-run.
const ambiguousResult = "outcome unknown: the session ended before this call's result was recorded. " +
	"It may or may not have run and has NOT been re-run; check the current state before relying on it."

// reconcileHistory gives every assistant tool call that has no recorded result
// an explicit "outcome unknown" result, so a resumed conversation is valid and
// the model sees the ambiguity instead of assuming success or failure.
// msgs is not modified.
func reconcileHistory(msgs []llm.Message) []llm.Message {
	var out []llm.Message
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if out != nil {
			out = append(out, m)
		} else if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		have := map[string]bool{}
		j := i + 1
		for ; j < len(msgs) && msgs[j].Role == llm.RoleTool; j++ {
			have[msgs[j].ToolCallID] = true
		}
		var missing []llm.Message
		for _, c := range m.ToolCalls {
			if !have[c.ID] {
				missing = append(missing, llm.Message{Role: llm.RoleTool, ToolCallID: c.ID, Text: ambiguousResult})
			}
		}
		if len(missing) == 0 {
			continue
		}
		if out == nil {
			out = append(make([]llm.Message, 0, len(msgs)+len(missing)), msgs[:i+1]...)
		}
		out = append(out, msgs[i+1:j]...)
		out = append(out, missing...)
		i = j - 1
	}
	if out == nil {
		return msgs
	}
	return out
}
