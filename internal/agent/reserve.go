package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/unix/ugh/internal/provider"
	"github.com/unix/ugh/internal/provstate"
	"github.com/unix/ugh/internal/usage"
)

// reserveEvery is how often provider usage is checked against [limits]
// reserve while a tree runs.
const reserveEvery = 5 * time.Minute

// watchReserve starts (once) a background check of provider usage. A
// provider whose busiest window is at or above its reserve is marked
// reserved, so new spawns route around it while running agents finish;
// the mark lifts when usage drops back or the window resets.
func (r *Runtime) watchReserve() {
	if len(r.Config.Limits.Reserve) == 0 {
		return
	}
	r.reserveOnce.Do(func() {
		go func() {
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				secs, _ := usage.Fetch(ctx, provider.UsageSources())
				cancel()
				applyReserve(secs, r.Config.Limits.Reserve, time.Now())
				time.Sleep(reserveEvery)
			}
		}()
	})
}

// applyReserve marks or clears reserved providers from one usage reading.
func applyReserve(secs []usage.Section, reserve map[string]float64, now time.Time) {
	for _, s := range secs {
		limit, ok := reserve[s.Provider]
		if !ok {
			continue
		}
		var worst *usage.Window
		for i := range s.Windows {
			if w := &s.Windows[i]; !w.NoBar && w.Percent >= limit && (worst == nil || w.Percent > worst.Percent) {
				worst = w
			}
		}
		if worst == nil {
			provstate.Clear(s.Provider, "reserved")
			continue
		}
		until := worst.ResetsAt
		if until.IsZero() || until.Before(now) {
			until = now.Add(2 * reserveEvery)
		}
		provstate.Mark(s.Provider, provstate.State{Kind: "reserved", Until: until,
			Reason: fmt.Sprintf("%s window at %.0f%% ≥ reserve %.0f%%", worst.Label, worst.Percent, limit)})
	}
}
