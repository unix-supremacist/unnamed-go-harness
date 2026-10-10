// /ask: answer a question about the run from a live snapshot of the agent
// tree, with a separate one-shot model call, so a root that is busy (often
// blocked waiting on its subagents) never has to be interrupted.
package tui

import (
	"context"
	"fmt"
	"github.com/unix/ugh/internal/admission"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/llm"
)

type askMsg struct {
	text string
	err  error
	gen  int
}

const askSystem = "You answer the user's question about an in-progress coding-agent run. You get a live " +
	"status snapshot of the agent tree (each agent's id, model, status, token use and latest step) and the " +
	"root agent's recent conversation. Answer briefly and concretely from that evidence only; say what is " +
	"unknown. Plain text."

func (s *State) ask(question string) tea.Cmd {
	if strings.TrimSpace(question) == "" {
		s.addEntry(entry{kind: "info", text: "· usage: /ask <question about the run>"})
		s.refresh()
		return nil
	}
	resolved, err := agent.CardRef(s.modelRef)
	if err != nil {
		s.addEntry(entry{kind: "error", text: "/ask: " + err.Error()})
		s.refresh()
		return nil
	}
	provName, model, _ := strings.Cut(resolved, "/")
	prov, err := s.provider(provName)
	if err != nil {
		s.addEntry(entry{kind: "error", text: "/ask: " + err.Error()})
		s.refresh()
		return nil
	}
	s.addEntry(entry{kind: "user", text: "/ask " + question})
	s.refresh()
	root, _ := s.runtime.Tree.View(s.agent.ID, 0)
	prompt := askPrompt(s.runtime.Tree.StatusReport(), root.Messages, question)
	thinking, gen := s.thinking, s.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		a := agent.New(agent.Config{Provider: prov, Model: model, System: askSystem, Thinking: thinking, MaxTurns: 1,
			ProviderName: provName, Admission: agent.DefaultAdmission(admission.PriorityRoot, "ask")})
		text, err := a.Run(ctx, prompt)
		return askMsg{text: strings.TrimSpace(text), err: err, gen: gen}
	}
}

func askPrompt(status string, hist []llm.Message, question string) string {
	tail := hist
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return fmt.Sprintf("Live agent tree status:\n%s\nRoot's recent conversation (summarized):\n%s\nQuestion: %s",
		status, agent.CompactPrompt(tail), question)
}
