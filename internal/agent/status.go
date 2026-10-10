package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/provstate"
)

// RoutingStatus is the human-readable routing/quota picture behind /routing:
// router mode, the Codex premium pool (plan source, live slots, waiters), the
// providers currently exhausted or reserved, and this run's managed worktrees.
func RoutingStatus(r *Runtime, m *admission.Manager) string {
	var b strings.Builder
	cfg := r.Config
	pref, other := cfg.Routing.PreferredRouter, cfg.Routing.FallbackRouter
	mode := "managed (router chooses model, tier, thinking)"
	if cfg.Routing.PickModel == "table" {
		mode = "table (no router)"
	}
	if r.traditional() {
		mode = fmt.Sprintf("traditional: routers unavailable, callers choose models until %s", r.tradUntilTime().Local().Format("15:04:05"))
	}
	fmt.Fprintf(&b, "routing: %s · preferred %s, backup %s\n", mode, orDash(pref), orDash(other))

	source := "set in config"
	if p := cfg.Routing.ChatGPTPlan; p == "" || p == "auto" {
		source = "detected from the Codex login (basic if undetectable)"
	}
	preset, limit := plannedPreset(cfg)
	fmt.Fprintf(&b, "codex premium pool: %s = %d concurrent Sol/Astra (%s)\n", preset, limit, source)
	if m != nil {
		if id := codexAccountID(); id != "" {
			if d, err := m.Diagnostics(id); err == nil {
				fmt.Fprintf(&b, "  in use %d/%d · council coordinator %d/%d · waiting roots %d\n",
					len(d.ActiveCombined), d.CombinedLimit, len(d.ActiveExempt), d.CouncilExemptLimit, len(d.Waiters))
			}
		} else {
			b.WriteString("  no Codex account identity: shared conservative bucket\n")
		}
	}
	if s := strings.TrimSpace(provstate.Summary()); s != "" {
		b.WriteString("providers: " + strings.ReplaceAll(s, "\n", "; ") + "\n")
	}
	r.mu.Lock()
	var ws []string
	for id, wt := range r.writers {
		ws = append(ws, fmt.Sprintf("  %s · %s · %s · %s", id, wt.Meta.Lifecycle, wt.Meta.Branch, wt.Dir))
	}
	r.mu.Unlock()
	sort.Strings(ws)
	if len(ws) > 0 {
		b.WriteString("worktrees (kept until you clean them up):\n" + strings.Join(ws, "\n") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *Runtime) tradUntilTime() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tradUntil
}
