package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/unix/ugh/internal/provider"
	"github.com/unix/ugh/internal/usage"
)

type usageMsg struct{ text string }

// showUsage fetches provider usage in the background (/usage, /quota).
func (s *State) showUsage() tea.Cmd {
	s.addEntry(entry{kind: "info", text: "· fetching usage…"})
	s.refresh()
	color := func(bar string, pct float64) string {
		switch {
		case pct >= 90:
			return s.pal.red.Render(bar)
		case pct >= 70:
			return s.pal.yellow.Render(bar)
		}
		return s.pal.green.Render(bar)
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		secs, errs := usage.Fetch(ctx, provider.UsageSources())
		return usageMsg{text: usage.Render(secs, errs, time.Now(), color)}
	}
}
