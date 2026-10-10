// Theme: the TUI palette and glamour style ([ui] theme = dark|light|system).
// dark and light are static palettes; system asks the terminal for its
// background color (bubbletea's RequestBackgroundColor) and resolves to one
// of them. Markdown uses glamour's matching dark/light style config.
package tui

import (
	"image/color"

	tea "charm.land/bubbletea/v2"

	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

// palette carries every color choice the views make. Rebuilt whenever the
// resolved dark/light state changes.
type palette struct {
	dim, red, green, yellow, user, dot, expanded lipgloss.Style
	modalBorder, pickerBorder                    color.Color
}

func themePalette(dark bool) palette {
	if dark {
		return palette{
			dim:          lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
			red:          lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
			green:        lipgloss.NewStyle().Foreground(lipgloss.Color("10")),
			yellow:       lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
			user:         lipgloss.NewStyle().Foreground(lipgloss.Color("12")),
			dot:          lipgloss.NewStyle().Foreground(lipgloss.Color("11")),
			modalBorder:  lipgloss.Color("9"),
			pickerBorder: lipgloss.Color("11"),
			expanded:     lipgloss.NewStyle().Foreground(lipgloss.Color("7")),
		}
	}
	return palette{
		dim:          lipgloss.NewStyle().Foreground(lipgloss.Color("240")),
		red:          lipgloss.NewStyle().Foreground(lipgloss.Color("160")),
		green:        lipgloss.NewStyle().Foreground(lipgloss.Color("28")),
		yellow:       lipgloss.NewStyle().Foreground(lipgloss.Color("136")),
		user:         lipgloss.NewStyle().Foreground(lipgloss.Color("26")),
		dot:          lipgloss.NewStyle().Foreground(lipgloss.Color("130")),
		modalBorder:  lipgloss.Color("160"),
		pickerBorder: lipgloss.Color("130"),
		expanded:     lipgloss.NewStyle().Foreground(lipgloss.Color("241")),
	}
}

func glamourStyles(dark bool) ansi.StyleConfig {
	if dark {
		return glamourstyles.DarkStyleConfig
	}
	return glamourstyles.LightStyleConfig
}

// initTheme applies the configured theme. The config file was already parsed
// in New; here we only build the resolved styles.
func (s *State) initTheme(theme string) {
	s.themeName = theme
	s.setDark(true) // dark is the default; system resolves on the first
	// terminal reply.
}

// setDark rebuilds the palette and glamour renderer for a resolved
// dark/light state and invalidates all cached transcript renders.
func (s *State) setDark(dark bool) {
	s.dark, s.pal = dark, themePalette(dark)
	s.glamStyles = glamourStyles(dark)
	if s.md != nil {
		s.md = nil // force a renderer rebuild with the new styles
	}
	for i := range s.entries {
		s.entries[i].lines = nil
	}
	s.refresh()
}

// wantsBackgroundQuery reports whether the terminal background must be
// requested (theme "system").
func (s *State) wantsBackgroundQuery() bool { return s.themeName == "system" }

// applyBackground resolves the "system" theme from the terminal's reply.
func (s *State) applyBackground(m tea.BackgroundColorMsg) {
	if s.themeName != "system" {
		return
	}
	s.setDark(m.IsDark())
}
