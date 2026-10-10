package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ContextReport renders /context: what the ladder did to this conversation's
// tool results, the instructions shown so far, and where compaction stands.
// It reads agent state, so call it while the agent is idle.
func (a *Agent) ContextReport() string {
	cc := a.cfg.Context
	var b strings.Builder
	b.WriteString("context\n")
	if cc.ladderOn() {
		fmt.Fprintf(&b, "  ladder: jev · keep_recent %d · min_chunk %s · min_savings %s\n", cc.KeepRecent, kib(cc.MinChunkBytes), kib(cc.MinSavings))
	} else {
		b.WriteString("  ladder: off (every tool result is shown in full)\n")
	}
	if st := a.lastLadder; !st.At.IsZero() {
		cost := "$?"
		if st.Cost.Known {
			cost = fmt.Sprintf("$%.4f", st.Cost.USD)
		}
		fmt.Fprintf(&b, "  last pass %s ago: %d chunks → %d hide · %d short · %d digest · %d full · %s saved · %s",
			time.Since(st.At).Round(time.Second), st.Chunks, st.Hide, st.Short, st.Digest, st.Full, kib(st.SavedBytes), cost)
		if st.Err != "" {
			b.WriteString(" · " + st.Err)
		}
		b.WriteString("\n")
	}
	var rows []string
	saved := 0
	for _, m := range a.hist {
		if m.Role != "tool" || m.Ord <= 0 || len(m.Text) < cc.MinChunkBytes {
			continue
		}
		e, ok := a.arch.get(m.Ord)
		if !ok {
			continue
		}
		lv := a.view[m.Ord]
		shown := len(a.viewText(m))
		saved += len(m.Text) - shown
		rows = append(rows, fmt.Sprintf("    %-5s %-44s %8s → %-8s %s", e.alias(), clipBytes(callLabel(e), 44), kib(len(m.Text)), kib(shown), lv))
	}
	if len(rows) > 0 {
		fmt.Fprintf(&b, "  tool results ≥ %s in view (%s saved):\n%s\n", kib(cc.MinChunkBytes), kib(saved), strings.Join(rows, "\n"))
	}
	fmt.Fprintf(&b, "  archive: %d tool results kept for recall\n", a.arch.count())
	if cc.Instructions {
		if len(a.instr) == 0 {
			b.WriteString("  instructions: none shown yet (AGENTS.md files appear when a file under them is touched; bash is not covered)\n")
		} else {
			var rels []string
			for _, s := range a.instr {
				rels = append(rels, s.rel)
			}
			sort.Strings(rels)
			fmt.Fprintf(&b, "  instructions shown: %s\n", strings.Join(rels, ", "))
		}
	} else {
		b.WriteString("  instructions: off\n")
	}
	used, limit := a.ContextUsage()
	switch {
	case limit > 0:
		fmt.Fprintf(&b, "  compaction: last prompt %s tokens · compacts at ~%s (%d compactions so far)\n", tok(used), tok(limit), a.Compactions)
	default:
		fmt.Fprintf(&b, "  compaction: no limit defined for %s · last prompt %s tokens\n", a.activeModel(), tok(used))
	}
	return strings.TrimRight(b.String(), "\n")
}

func tok(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
