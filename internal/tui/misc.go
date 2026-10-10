package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// copyLast puts the latest assistant message on the clipboard through the
// terminal (OSC 52), so it works over ssh and tmux without a clipboard tool.
func (s *State) copyLast() tea.Cmd {
	text := ""
	if s.agent != nil {
		for _, m := range s.agent.History() {
			if m.Role == "assistant" && strings.TrimSpace(m.Text) != "" {
				text = m.Text
			}
		}
	}
	if text == "" {
		s.addEntry(entry{kind: "info", text: "· nothing to copy yet"})
		s.refresh()
		return nil
	}
	s.addEntry(entry{kind: "info", text: "· copied the last assistant message (" + plural(len(strings.Fields(text)), "word") + ") via OSC 52"})
	s.refresh()
	return tea.SetClipboard(text)
}

// initPrompt is what /init asks the agent to do.
const initPrompt = "Survey this project and write instruction files for future agents working in it.\n\n" +
	"1. Write AGENTS.md at the project root: what the project is, how to build, test and run it, the conventions that are not obvious from the code, and anything that has bitten people before. Keep it short and concrete; every line should save a future agent from a mistake or a search.\n" +
	"2. Where a subdirectory has rules of its own (a different language, generated code, a fragile area, a test setup quirk), write a small AGENTS.md there with only those rules. They are shown to an agent when it first touches a file under that directory.\n" +
	"3. Never restate what the code or its README already says clearly, and never invent facts: verify commands by running them, and leave out what you could not confirm.\n" +
	"If AGENTS.md already exists, improve it rather than replacing it."

// initProject sends the canned /init prompt as an ordinary user turn.
func (s *State) initProject() tea.Cmd {
	s.history = append(s.history, "/init")
	s.histIdx = -1
	s.addEntry(entry{kind: "user", text: "/init"})
	s.refresh()
	return s.send(initPrompt)
}
