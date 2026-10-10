// Package tui is ugh's Bubble Tea interface (PLAN.md §7, M3): a scrolling
// transcript, a growing editor, a footer with model/thinking/perm
// mode/tokens/cwd, a spinner with elapsed time, streamed assistant text and
// compact tool lines (ctrl+o expands), a permission modal fed by the
// channel prompt broker, a model picker, and slash commands. All provider
// and agent work runs in goroutines that send tea.Msgs; nothing blocks in
// Update.
package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/unix/ugh/internal/agent"
)

// Options configures one TUI launch.
type Options struct {
	Model    string // provider/model ref
	Thinking string // off|low|medium|high
	System   string
	Cwd      string
	Mode     string // permission mode override (manual|classify|auto-safe|auto)
	MaxTurns int    // root turn budget override (0: config agent.max_turns_root)
	Resume   bool   // resume the cwd's latest session
}

const (
	defaultModel  = "openrouter/xiaomi/mimo-v2.6-pro"
	defaultSystem = agent.DefaultSystem
)

// Main runs the TUI to completion.
func Main(o Options) error {
	cwd := o.Cwd
	if cwd == "" {
		var err error
		if cwd, err = currentDir(); err != nil {
			return err
		}
	}
	s, err := New(o, cwd)
	if err != nil {
		return err
	}
	prog := tea.NewProgram(s)
	s.prog.Store(prog)
	_, err = prog.Run()
	close(s.done) // unblocks any Ask still waiting; its run returns AnsStop
	return err
}
