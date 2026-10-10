// Session transitions. Every way of changing the live session — /new, /resume,
// the picker, startup -resume — goes through switchSession: it is refused
// while anything is in flight, builds the new session off to the side, and
// commits (store, agent, transcript) only when all of it succeeded. A
// generation counter rises on each commit so late results addressed to the
// previous session are dropped.
package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
)

// agentSpec is everything buildAgent needs, so building never reads or
// mutates the live State.
type agentSpec struct {
	store                      *session.Store
	modelRef, thinking, system string
	runID                      string
	history                    []llm.Message
	archive, councils          []session.Record
	inherit                    *agent.Agent // same-session predecessor, for its archive
}

// sessionNext is a prepared, not yet committed, session.
type sessionNext struct {
	spec    agentSpec
	created bool // the file was made by this switch (removed if it fails)
	entries []entry
	prompts []string
	notice  string
}

func (n *sessionNext) discard() {
	n.spec.store.Close()
	if n.created {
		path := n.spec.store.Path()
		_ = os.Remove(path)
		_ = os.RemoveAll(session.BlobDir(path))
	}
}

// switchBlocked says why the session cannot be replaced right now.
func (s *State) switchBlocked() error {
	switch {
	case s.running:
		return errors.New("a run is in progress — esc cancels it")
	case s.compacting:
		return errors.New("a compaction is in progress")
	case s.modal != nil:
		return errors.New("a permission question is open")
	case s.askOpen():
		return errors.New("a question from an agent is open")
	}
	return nil
}

// switchSession replaces the live session with whatever prep builds.
func (s *State) switchSession(prep func() (*sessionNext, error)) error {
	if err := s.switchBlocked(); err != nil {
		return err
	}
	next, err := prep()
	if err != nil {
		return err
	}
	ag, err := s.buildAgent(next.spec)
	if err != nil {
		next.discard()
		return err
	}
	s.commitSession(next, ag)
	return nil
}

func (s *State) commitSession(n *sessionNext, ag *agent.Agent) {
	old := s.sessCloser()
	s.mu.Lock()
	s.sess = n.spec.store
	s.mu.Unlock()
	old.Close()
	s.declineAsks("session changed")
	sp := n.spec
	s.runtime.RunID = sp.runID
	s.modelRef, s.thinking, s.system = sp.modelRef, sp.thinking, sp.system
	s.councils = sp.councils
	s.agent = ag
	s.gen++
	s.usage = ag.Usage
	s.entries = n.entries
	s.history, s.histIdx = n.prompts, -1
	s.focus, s.resume = nil, nil
	s.invalidate()
	if n.notice != "" {
		s.addEntry(entry{kind: "info", text: n.notice})
	}
	s.refresh()
	s.vp.GotoBottom()
}

// freshSession prepares a new, empty session file.
func (s *State) freshSession() func() (*sessionNext, error) {
	return func() (*sessionNext, error) {
		store, err := session.Create(s.cwd, session.Record{
			Model: s.modelRef, Thinking: s.thinking, System: s.system, Cwd: s.cwd, Run: s.runtime.RunID,
		})
		if err != nil {
			return nil, err
		}
		return &sessionNext{
			created: true,
			spec:    agentSpec{store: store, modelRef: s.modelRef, thinking: s.thinking, system: s.system, runID: s.runtime.RunID},
			notice:  "· new session → " + filepath.Base(store.Path()),
		}, nil
	}
}

// resumeSession prepares an existing session file: history, archive, model,
// thinking and system come from it, and the transcript is replayed.
func (s *State) resumeSession(path string) func() (*sessionNext, error) {
	return func() (*sessionNext, error) {
		loaded, err := session.Load(path)
		if err != nil {
			return nil, err
		}
		if loaded.Meta.Parent != "" {
			return nil, fmt.Errorf("%s is a subagent session", filepath.Base(path))
		}
		sp := agentSpec{modelRef: s.modelRef, thinking: s.thinking, system: s.system, runID: s.runtime.RunID,
			history: loaded.Messages, archive: loaded.Display, councils: loaded.Councils}
		if loaded.Meta.Run != "" {
			sp.runID = loaded.Meta.Run
		}
		if loaded.Meta.Model != "" {
			sp.modelRef = loaded.Meta.Model
			if loaded.Meta.Thinking != "" {
				sp.thinking = loaded.Meta.Thinking
			}
			if loaded.Meta.System != "" {
				sp.system = loaded.Meta.System
			}
		}
		store, err := session.Open(path)
		if err != nil {
			return nil, err
		}
		sp.store = store
		entries, prompts := replayEntries(loaded.Display)
		return &sessionNext{spec: sp, entries: entries, prompts: prompts,
			notice: fmt.Sprintf("· resumed %d messages from %s", len(loaded.Messages), filepath.Base(path))}, nil
	}
}

// replayEntries rebuilds the transcript from the session's chronological
// records: every message (tool results attached to their calls) and each
// compaction as a dim summary block at its original position. Calls that
// never got a result (a torn tail) are marked rather than left spinning.
func replayEntries(display []session.Record) ([]entry, []string) {
	var es []entry
	var prompts []string
	idx := map[string]int{}
	for _, rec := range display {
		switch {
		case rec.Type == "compaction":
			text := "── earlier turns compacted"
			if len(rec.Seed) > 0 {
				text += "\n" + clip(rec.Seed[0].Text, 700)
			}
			es = append(es, entry{kind: "info", text: text})
		case rec.Message != nil:
			m := *rec.Message
			if m.Role == llm.RoleUser && typedPrompt(m.Text) {
				prompts = append(prompts, m.Text)
			}
			appendMessage(&es, idx, m)
		}
	}
	for _, i := range idx {
		if t := es[i].tool; t != nil && t.status == "running" {
			t.status, t.output = "error", "(no result recorded)"
		}
	}
	return es, prompts
}

// typedPrompt is false for user messages the harness injected.
func typedPrompt(t string) bool {
	for _, p := range []string{"[Message from the user, sent while you were working]", "Turn budget nearly exhausted",
		"Original task:", "Summary of the earlier conversation", "[Subagent"} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return strings.TrimSpace(t) != ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// buildAgent constructs the root agent for spec. It reads nothing mutable
// from State, and its session callbacks are bound to spec.store, so a late
// callback can only ever write to the session it was built for.
func (s *State) buildAgent(sp agentSpec) (*agent.Agent, error) {
	resolved, err := agent.CardRef(sp.modelRef)
	if err != nil {
		return nil, err
	}
	provName, model, ok := strings.Cut(resolved, "/")
	if !ok {
		return nil, fmt.Errorf("model %q must be provider/model", sp.modelRef)
	}
	prov, err := s.provider(provName)
	if err != nil {
		return nil, err
	}
	store := sp.store
	reg := tools.NewRegistry(tools.All()...)
	ag := agent.New(agent.Config{
		Provider: prov, Model: model, System: sp.system, Thinking: sp.thinking,
		Tools: reg, Cwd: s.cwd, Gate: s.eng, History: sp.history, MaxTurns: s.maxTurns,
		Tree: s.runtime.Tree, Spawn: s.runtime.Spawn, StartChild: s.runtime.StartChild, Resume: s.runtime.ResumeChild, StreamLimit: s.runtime.Limit, Runtime: s.runtime,
		Events: func(ev agent.Event) {
			if p := s.prog.Load(); p != nil {
				p.Send(agentMsg{ev})
			}
		},
		OnMessage:    func(msg llm.Message) { _ = store.Append(session.Record{Type: "message", Message: &msg}) },
		AutoCompact:  s.runtime.Config.Agent.AutoCompact,
		Inbox:        inboxFor(sp.runID),
		ToolTimeout:  time.Duration(s.runtime.Config.Tools.TimeoutS) * time.Second,
		ProviderName: provName, Admission: agent.DefaultAdmission(admission.PriorityRoot, "root"),
		Scratch: session.ScratchPath(sp.runID, "root"),
		Archive: sp.archive, BlobDir: session.BlobDir(store.Path()),
		Context:   agent.ContextConfigFrom(s.runtime.Config),
		OnCompact: func(h []llm.Message) { _ = store.Append(session.Record{Type: "compaction", Seed: h}) },
		OnRecord:  func(rec session.Record) { _ = store.Append(rec) },
		OnStatus:  func(st string) { _ = store.Append(session.Record{Type: "status", Status: st}) },
	})
	ag.InheritFrom(sp.inherit)
	for _, rec := range sp.councils {
		ag.RestoreCouncil(rec) // outcomes replay; approvals never do
	}
	return ag, nil
}

// rebuildAgent replaces the agent within the current session (model or
// thinking changed), keeping its history and tool-result archive. On error
// the previous agent stays.
func (s *State) rebuildAgent(history []llm.Message) error {
	ag, err := s.buildAgent(agentSpec{
		store: s.sessCloser(), modelRef: s.modelRef, thinking: s.thinking, system: s.system,
		runID: s.runtime.RunID, history: history, councils: s.councils, inherit: s.agent,
	})
	if err != nil {
		return err
	}
	s.agent = ag
	return nil
}

// contextConfig is the agent-facing configuration with this UI's ask_user hook.
func (s *State) contextConfig() agent.ContextConfig {
	c := agent.ContextConfigFrom(s.runtime.Config)
	c.AskUser = s.askUser
	return c
}

// commitSessionForTest switches to a fresh session through the real path.
func (s *State) commitSessionForTest() error { return s.switchSession(s.freshSession()) }
