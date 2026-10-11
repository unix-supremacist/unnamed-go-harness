// Model: TUI state, updates and command handling.
package tui

import (
	"context"
	"errors"
	"fmt"
	"github.com/unix/ugh/internal/admission"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/policy"
	"github.com/unix/ugh/internal/provider"
	"github.com/unix/ugh/internal/session"
)

// entry is one transcript block; lines is its rendered cache (nil = stale).
type entry struct {
	kind  string // user | assistant | thinking | tool | info | error
	text  string // prompt/assistant message content
	live  bool   // assistant entry still streaming
	tool  *toolEntry
	lines []string
}

// toolEntry is one tool call: compact header line, expanded output.
type toolEntry struct {
	call    llm.ToolCall
	status  string   // running | ok | error | stopped | denied
	flagged []string // classifiers that flagged this call (dim marker)
	elapsed time.Duration
	started time.Time // when the call was first seen (live timer while running)
	output  string
	tail    string // live last lines of a running command
}

// State is the Bubble Tea model.
type State struct {
	maxTurns int // root turn budget
	w, h     int
	vp       viewport.Model
	ta       textarea.Model
	spin     spinner.Model
	entries  []entry

	prog atomic.Pointer[tea.Program]
	done chan struct{}

	// conversation and services
	mu           sync.RWMutex // guards sess (touchable from run goroutines)
	md           *glamour.TermRenderer
	mdWidth      int
	sess         *session.Store
	agent        *agent.Agent
	runtime      *agent.Runtime
	treeOpen     bool
	treeSelected int
	focus        *focusState // nil = root's transcript
	frameOn      bool        // frame loop running
	dirty        bool        // transcript needs a redraw on the next frame
	cache        []byte      // joined lines of entries[:cacheN]
	cacheN       int
	lastTimer    time.Time // last running-tool timer tick
	eng          *perm.Engine
	councils     []session.Record // council outcomes of a resumed session
	broker       *perm.UIBroker
	providers    map[string]llm.Provider

	cwd, system string
	modelRef    string // provider/model shown in the footer
	thinking    string
	running     bool
	started     time.Time
	cancel      context.CancelFunc
	phase       string // spinner label: thinking | <tool>
	usage       llm.Usage

	// theme + honest footer (M8): resolved palette, glamour styles and the
	// configured [pricing] table.
	themeName  string
	dark       bool
	pal        palette
	glamStyles ansi.StyleConfig
	pricing    map[string]config.Price

	// UI state
	asks       askQueue      // ask_user questions from agents
	askTimeout time.Duration // [ui] ask_timeout; 0 = wait as long as it takes
	gen        int           // session generation; late results for an older one are dropped
	compacting bool          // a /compact summary is in flight
	resume     *resumeState
	resumeSeq  int
	modal      *perm.UIRequest // permission modal
	picker     *pickerState
	expanded   bool     // ctrl+o toggles tool output
	history    []string // sent prompts, oldest first
	histIdx    int      // -1 = not browsing
	lastCtrlC  time.Time
	lastEsc    time.Time // esc must be pressed twice while subagents run

	// tab completion (@file paths, slash commands): the candidate list for
	// the current token, the cursor into it and the hint line shown above
	// the editor.
	compCands []string
	compIdx   int
	compSeed  string
	compHint  string
}

// pickerState is the model-picker bottom sheet.
type pickerState struct {
	scopedEdit   bool     // /scoped-models: toggle membership instead of choosing
	compact      bool     // /compact-model: choose the summarizer instead of the conversation model
	all          []string // every provider/model ref reported by the providers
	items        []string // all, narrowed by filter (and scope when onlyScoped)
	curs         int
	filter       string          // type-to-filter text
	onlyScoped   bool            // /model: show only scoped models (tab toggles)
	scoped       map[string]bool // saved scope
	loading      bool
	err          string
	scopeWarning string
	access       map[string]string
}

// msgs
type (
	agentMsg   struct{ ev agent.Event }
	runDoneMsg struct {
		final string
		err   error
		gen   int
	}
	promptMsg   struct{ req perm.UIRequest }
	classifyMsg struct {
		call     perm.Call
		verdicts []perm.Verdict
	}
	modelsMsg struct {
		models []provider.Model
		errs   map[string]error // per-provider listing failures (non-fatal)
	}
	// compactedMsg arrives when the /compact summary request finishes.
	compactedMsg struct {
		summary string
		n       int
		err     error
		gen     int
		usage   llm.Usage // the summarizer call's usage
	}
	// loginMsg lines stream the async /login flow into the transcript.
	loginMsg struct{ line string }
)

// ---- construction ----

// New builds the TUI model: session file, permission gate and agent ready.
func New(o Options, cwd string) (*State, error) {
	s := &State{
		w: 80, h: 24, cwd: cwd,
		system: o.System, modelRef: o.Model, thinking: o.Thinking,
		histIdx: -1, providers: map[string]llm.Provider{},
	}
	if s.system == "" {
		s.system = defaultSystem
	}
	if s.modelRef == "" {
		s.modelRef = defaultModel
	}
	if s.thinking == "" {
		s.thinking = agent.DefaultThinking
	}
	s.thinking = agent.ClampThinking(s.modelRef, s.thinking)
	s.done = make(chan struct{})
	s.runtime = agent.NewRuntime()
	s.maxTurns = o.MaxTurns
	if s.maxTurns <= 0 {
		s.maxTurns = s.runtime.Config.Agent.MaxTurnsRoot
	}

	// Theme ([ui] theme) and the honest footer's [pricing] table come from
	// the config file; a broken config falls back to the defaults.
	if cfg, err := config.Load(); err == nil {
		s.pricing = cfg.Pricing
		if d, err := time.ParseDuration(cfg.UI.AskTimeout); err == nil && d > 0 {
			s.askTimeout = d
		}
		s.initTheme(cfg.UI.Theme)
	} else {
		s.initTheme(config.Default().UI.Theme)
	}

	s.ta = textarea.New()
	s.ta.SetWidth(76)
	s.ta.SetHeight(1)
	s.ta.KeyMap.InsertNewline.SetKeys("alt+enter") // enter is send
	s.ta.Placeholder = "Ask ugh anything… (/help for commands)"
	s.ta.Focus()

	s.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	s.vp = viewport.New()
	s.vp.SoftWrap = true

	// Permission gate: channel broker → the TUI renders the modal.
	s.broker = perm.NewUIBroker(s.done)
	eng, err := provider.Gate(o.Mode, s.broker)
	if err != nil {
		return nil, err
	}
	// Verdicts that no prompt showed (auto mode, log-state classifiers) mark
	// the matching tool line dim (§4.2 step 4).
	eng.Notify = func(c perm.Call, vs []perm.Verdict) {
		if p := s.prog.Load(); p != nil {
			p.Send(classifyMsg{call: c, verdicts: vs})
		}
	}
	s.eng = eng

	if o.Resume {
		// Resume the previous root session; fall back to a fresh one when
		// the cwd has none yet (a fresh file must not become the resume target).
		path, err := session.Latest(s.cwd)
		switch {
		case err == nil:
			if err := s.switchSession(s.resumeSession(path)); err != nil {
				return nil, err
			}
		case strings.Contains(err.Error(), "no sessions"):
			if err := s.switchSession(s.freshSession()); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	} else if err := s.switchSession(s.freshSession()); err != nil {
		return nil, err
	}

	s.addEntry(entry{kind: "info", text: fmt.Sprintf(
		"ugh — %s · perm %s · cwd %s\nenter send · alt+enter newline · ctrl+l model · ctrl+r resume · "+
			"shift+tab perm mode · ctrl+o expand tools · esc cancel · ctrl+c clear (twice quits) · /help",
		s.modelRef, s.eng.Policy.Mode, cwd)})
	s.refresh()
	return s, nil
}

func (s *State) sessCloser() *session.Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sess
}

// provider resolves and caches clients per provider name.
func (s *State) provider(name string) (llm.Provider, error) {
	if p, ok := s.providers[name]; ok {
		return p, nil
	}
	p, _, err := provider.Resolve(name + "/x") // model part unused here
	if err != nil {
		return nil, err
	}
	s.providers[name] = p
	return p, nil
}

// ---- Bubble Tea interface ----

func (s *State) Init() tea.Cmd {
	cmds := []tea.Cmd{s.spin.Tick, s.waitPrompt, s.startFrames()}
	if s.wantsBackgroundQuery() {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	return tea.Batch(cmds...)
}

func (s *State) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.w, s.h = msg.Width, msg.Height
		s.vp.SetWidth(s.w)
		s.layout()
		s.invalidate()
		for _, es := range []*[]entry{&s.entries, s.active()} {
			for i := range *es {
				(*es)[i].lines = nil
			}
		}
		s.refresh()
		return s, nil

	case frameMsg:
		return s, s.onFrame()

	case spinner.TickMsg:
		if s.running {
			sp, cmd := s.spin.Update(msg)
			s.spin = sp
			return s, tea.Batch(cmd, s.spin.Tick)
		}
		return s, nil

	case agentMsg:
		s.applyEvent(msg.ev)
		return s, nil

	case runDoneMsg:
		if msg.gen != s.gen {
			return s, nil // a run of a session that has since been replaced
		}
		s.runDone(msg.final, msg.err)
		// Messages queued too late for the run's last turn start a new one.
		if queued := s.agent.TakeSteered(); len(queued) > 0 && !isCancel(msg.err) {
			return s, s.send(strings.Join(queued, "\n\n"))
		}
		return s, nil

	case usageMsg:
		s.addEntry(entry{kind: "usage", text: msg.text})
		s.refresh()
		return s, nil

	case askMsg:
		if msg.gen != s.gen {
			return s, nil
		}
		if msg.err != nil {
			s.addEntry(entry{kind: "error", text: "/ask: " + msg.err.Error()})
		} else {
			s.addEntry(entry{kind: "assistant", text: "(/ask) " + msg.text})
		}
		s.refresh()
		return s, nil

	case promptMsg:
		s.modal = &msg.req
		s.refresh()
		return s, nil

	case classifyMsg:
		s.markFlagged(msg)
		return s, nil

	case tea.BackgroundColorMsg:
		s.applyBackground(msg)
		return s, nil

	case compactedMsg:
		s.compacting = false
		if msg.gen != s.gen {
			return s, nil
		}
		s.applyCompact(msg)
		return s, nil

	case askChangedMsg:
		s.refresh()
		return s, nil

	case resumeScanMsg:
		s.onResumeScan(msg)
		return s, nil

	case loginMsg:
		s.addEntry(entry{kind: "info", text: msg.line})
		s.refresh()
		return s, nil

	case modelsMsg:
		if s.picker != nil {
			s.picker.loading = false
			s.picker.setModels(msg, s.modelRef)
			s.refresh()
		}
		return s, nil

	case tea.KeyPressMsg:
		return s.keyPress(msg)
	}
	return s, nil
}

// waitPrompt receives permission requests from the channel broker.
func (s *State) waitPrompt() tea.Msg {
	select {
	case req := <-s.broker.Requests:
		return promptMsg{req}
	case <-s.done:
		return nil
	}
}

// ---- events from the run goroutine ----

func (s *State) applyEvent(ev agent.Event) {
	switch ev.Kind {
	case "text":
		s.endThinking()
		s.appendAssistantText(ev.Text)
	case "thinking":
		s.phase = "thinking"
		s.appendThinking(ev.Text)
	case "tool_args":
		s.endThinking()
		s.phase = fmt.Sprintf("writing %s call · %s chars", ev.Call.Name, tok(ev.Chars))
	case "tool_start":
		s.endThinking()
		s.phase = ev.Call.Name
		s.addEntry(entry{kind: "tool", tool: &toolEntry{
			call: ev.Call, status: "running", started: time.Now(),
			output: strings.TrimSpace(ev.Call.Args),
		}})
	case "tool_done":
		st := "ok"
		switch {
		case strings.HasPrefix(ev.Output, "⛔"):
			st = "stopped"
		case ev.Output == "⚠ denied" || strings.HasPrefix(ev.Output, "⚠"):
			st = "denied"
		case ev.Err != nil:
			st = "error"
		}
		for i := len(s.entries) - 1; i >= 0; i-- {
			e := &s.entries[i]
			if e.kind == "tool" && e.tool.status == "running" && e.tool.call.ID == ev.Call.ID {
				e.tool.status, e.tool.elapsed, e.tool.output = st, ev.Elapsed, ev.Output
				e.lines = nil
				s.phase = ""
				break
			}
		}
	case "usage":
		s.usage = ev.Usage
	case "tool_progress":
		for i := len(s.entries) - 1; i >= 0; i-- {
			if e := &s.entries[i]; e.kind == "tool" && e.tool.status == "running" && e.tool.call.ID == ev.Call.ID {
				e.tool.tail, e.lines = ev.Output, nil
				break
			}
		}
	case "note":
		s.addEntry(entry{kind: "info", text: "· " + ev.Text})
	}
	s.dirty = true // drawn on the next frame
}

// markFlagged records which classifiers flagged a tool call so the compact
// line can carry a dim marker.
func (s *State) markFlagged(m classifyMsg) {
	var names []string
	for _, v := range m.verdicts {
		if v.Flagged && v.Err == nil {
			names = append(names, v.Classifier)
		}
	}
	if len(names) == 0 {
		return
	}
	changed := false
	for i := range s.entries {
		e := &s.entries[i]
		if e.kind == "tool" && e.tool != nil && sameToolCall(e.tool.call, m.call) {
			e.tool.flagged = names
			e.lines = nil
			changed = true
		}
	}
	if changed {
		s.refresh()
	}
}

// sameToolCall matches a permission call to the tool entry it came from: by
// provider tool-call id when present, else by name and raw args.
func sameToolCall(tc llm.ToolCall, c perm.Call) bool {
	if c.ID != "" && tc.ID != "" {
		return tc.ID == c.ID
	}
	return tc.Name == c.Tool && tc.Args == string(c.Args)
}

// appendAssistantText streams assistant text into the last live entry.
func (s *State) appendAssistantText(t string) {
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := &s.entries[i]
		if e.kind == "assistant" && e.live {
			e.text += t
			e.lines = nil
			s.dirty = true
			return
		}
		if e.kind != "info" && e.kind != "error" {
			break
		}
	}
	s.addEntry(entry{kind: "assistant", text: t, live: true})
}

// runDone finalizes a run: error reporting, usage line, live entry flush.
func (s *State) runDone(final string, err error) {
	s.running = false
	s.phase = ""
	for i := range s.entries {
		if s.entries[i].live {
			s.entries[i].live = false
			s.entries[i].lines = nil
		}
	}
	a := s.agent
	usage := fmt.Sprintf("%s · %s in / %s out / %s cached tok · %s",
		s.modelRef, tok(a.Usage.In), tok(a.Usage.Out), tok(a.Usage.Cached),
		time.Since(s.started).Round(time.Millisecond))
	switch {
	case err == nil:
		s.addEntry(entry{kind: "info", text: "· " + usage})
	case isCancel(err):
		s.addEntry(entry{kind: "info", text: "· run cancelled · " + usage})
	case errors.As(err, new(*agent.TurnOutError)):
		var to *agent.TurnOutError
		errors.As(err, &to)
		if !to.Answered {
			s.addEntry(entry{kind: "info", text: final})
		}
		s.addEntry(entry{kind: "error", text: "· ⏳ " + err.Error() + " · " + usage})
	case isStopped(err):
		s.addEntry(entry{kind: "error", text: "· ⛔ " + err.Error() + " · " + usage})
	default:
		s.addEntry(entry{kind: "error", text: "· error: " + err.Error()})
	}
	s.refresh()
}

func isStopped(err error) bool {
	return err != nil && strings.Contains(err.Error(), "stopped by permission policy")
}

func isCancel(err error) bool {
	return err != nil && (context.Canceled == err || strings.Contains(err.Error(), "canceled"))
}

func tok(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// ---- keys ----

func (s *State) keyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if s.modal != nil {
		switch msg.String() {
		case "y", "Y":
			return s.answerPrompt(perm.AnsAllowOnce)
		case "a", "A":
			return s.answerPrompt(perm.AnsAllowSession)
		case "n", "N", "esc":
			return s.answerPrompt(perm.AnsDeny)
		case "s", "S":
			return s.answerPrompt(perm.AnsStop)
		}
		return s, nil
	}
	if it := s.askActive(); it != nil && msg.String() != "ctrl+c" {
		return s.askKey(it, msg)
	}
	switch msg.String() {
	case "ctrl+c":
		if s.modal != nil { // never idle-out of a permission question
			return s, nil
		}
		if time.Since(s.lastCtrlC) < 2*time.Second {
			s.declineAsks("quit")
			return s, tea.Quit
		}
		s.lastCtrlC = time.Now()
		if s.ta.Value() != "" {
			s.ta.Reset()
			s.layout()
		}
		return s, nil
	case "ctrl+t":
		s.treeOpen = !s.treeOpen
		return s, nil
	case "ctrl+o":
		s.expanded = !s.expanded
		s.invalidate()
		for _, es := range []*[]entry{&s.entries, s.active()} {
			for i := range *es {
				(*es)[i].lines = nil
			}
		}
		s.refresh()
		return s, nil
	case "shift+tab":
		s.cycleMode()
		return s, nil
	case "ctrl+l":
		if s.picker != nil {
			s.picker = nil
			s.refresh()
			return s, nil
		}
		return s, s.openPicker(false)
	case "ctrl+r":
		if s.resume != nil {
			s.resume = nil
			s.refresh()
			return s, nil
		}
		return s, s.openResume()
	case "ctrl+p":
		s.cycleScoped(1)
		return s, nil
	case "ctrl+shift+p":
		s.cycleScoped(-1)
		return s, nil
	}
	if s.picker != nil {
		return s.pickerKey(msg)
	}
	if s.resume != nil {
		return s.resumeKey(msg)
	}
	if s.treeOpen {
		switch msg.String() {
		case "esc":
			s.treeOpen = false
			return s, nil
		case "up":
			if s.treeSelected > 0 {
				s.treeSelected--
			}
			return s, nil
		case "down":
			if s.treeSelected < len(s.runtime.Tree.Outline())-1 {
				s.treeSelected++
			}
			return s, nil
		case "enter":
			nodes := s.runtime.Tree.Outline()
			if s.treeSelected < len(nodes) {
				s.treeOpen = false
				return s, s.setFocus(nodes[s.treeSelected].ID)
			}
			return s, nil
		}
	}
	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(s.ta.Value())
		if text == "" {
			return s, nil
		}
		s.ta.Reset()
		s.clearComp()
		s.layout()
		if strings.HasPrefix(text, "/") {
			return s, s.command(text)
		}
		if f := s.focus; f != nil {
			s.history = append(s.history, text)
			s.histIdx = -1
			switch {
			case f.info.Status == "done" || f.info.Status == "error" || f.info.Status == "turn-out" || f.info.Status == "cancelled":
				f.entries = append(f.entries, entry{kind: "error", text: "· " + f.id + " has finished (" + f.info.Status + "); esc returns to root"})
			case s.runtime.Tree.Steer(f.id, text):
				f.entries = append(f.entries, entry{kind: "info", text: "· sent to " + f.id + "; it reads it at its next turn"})
			default:
				f.entries = append(f.entries, entry{kind: "error", text: "· could not reach " + f.id})
			}
			s.refresh()
			return s, nil
		}
		if s.running {
			// Never drop the message: root reads it at its next turn.
			s.agent.Steer(text)
			s.history = append(s.history, text)
			s.histIdx = -1
			s.addEntry(entry{kind: "user", text: text})
			s.addEntry(entry{kind: "info", text: "· queued for root's next turn (it may be busy waiting on subagents) · /ask answers now from live status"})
			s.refresh()
			return s, nil
		}
		s.history = append(s.history, text)
		s.histIdx = -1
		s.addEntry(entry{kind: "user", text: text})
		s.refresh()
		return s, s.send(text)
	case "esc":
		if s.focus != nil {
			return s, s.setFocus("")
		}
		if s.running && s.cancel != nil {
			if n := s.liveChildren(); n > 0 && time.Since(s.lastEsc) > 2*time.Second {
				s.lastEsc = time.Now()
				s.addEntry(entry{kind: "info", text: fmt.Sprintf("· press esc again to cancel the run and its %d running subagent(s)", n)})
				s.refresh()
				return s, nil
			}
			s.cancel()
			s.addEntry(entry{kind: "info", text: "· cancelling…"})
			s.refresh()
		}
		return s, nil
	case "up":
		if s.ta.Line() == 0 && len(s.history) > 0 {
			if s.histIdx == -1 {
				s.histIdx = len(s.history) - 1
			} else if s.histIdx > 0 {
				s.histIdx--
			}
			s.ta.SetValue(s.history[s.histIdx])
			s.layout()
			return s, nil
		}
	case "down":
		if s.histIdx >= 0 {
			if s.histIdx < len(s.history)-1 {
				s.histIdx++
			} else {
				s.histIdx = -1
			}
			v := ""
			if s.histIdx >= 0 {
				v = s.history[s.histIdx]
			}
			s.ta.SetValue(v)
			s.layout()
			return s, nil
		}
	case "pgup", "pgdown":
		vp, _ := s.vp.Update(msg)
		s.vp = vp
		return s, nil
	case "tab":
		s.tabComplete()
		return s, nil
	}
	ta, cmd := s.ta.Update(msg)
	s.ta = ta
	s.clearComp()
	s.layout()
	return s, cmd
}

// answerPrompt resolves the modal and re-arms the broker listener.
func (s *State) answerPrompt(ans perm.Answer) (tea.Model, tea.Cmd) {
	req := *s.modal
	words := map[perm.Answer]string{
		perm.AnsAllowOnce: "allowed once", perm.AnsAllowSession: "allowed for session",
		perm.AnsDeny: "denied", perm.AnsStop: "stopping",
	}
	if ans == perm.AnsAllowSession {
		s.broker.Remember(req.Call)
	}
	s.modal = nil
	req.Reply <- ans
	s.addEntry(entry{kind: "info", text: fmt.Sprintf("· %s %s → %s",
		req.Call.Tool, oneLine(req.Call), words[ans])})
	s.refresh()
	return s, s.waitPrompt
}

// ---- run ----

// send starts one prompt in its own goroutine; Run's completion arrives
// as runDoneMsg and progress as agentMsgs.
func (s *State) send(text string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.running, s.started, s.phase = cancel, true, time.Now(), ""
	a, gen := s.agent, s.gen
	return tea.Batch(func() tea.Msg {
		final, err := a.Run(ctx, text)
		return runDoneMsg{final: final, err: err, gen: gen}
	}, s.startFrames())
}

// ---- slash commands ----

func (s *State) command(line string) tea.Cmd {
	name, arg, known := splitCommand(line)
	if spec, ok := lookupCmd(name); ok && spec.idleOnly && s.running {
		s.addEntry(entry{kind: "info", text: "· " + spec.name + " is unavailable while a run is in progress — esc cancels"})
		s.refresh()
		return nil
	}
	if !known {
		s.addEntry(entry{kind: "error", text: "unknown command " + name + " — /help lists commands"})
		s.refresh()
		return nil
	}
	switch name {
	case "/quit":
		s.declineAsks("quit")
		return tea.Quit
	case "/new":
		if err := s.switchSession(s.freshSession()); err != nil {
			s.addEntry(entry{kind: "error", text: "/new: " + err.Error()})
			s.refresh()
		}
	case "/compact":
		return s.compact()
	case "/copy":
		return s.copyLast()
	case "/init":
		return s.initProject()
	case "/context":
		s.addEntry(entry{kind: "info", text: s.agent.ContextReport()})
		s.refresh()
	case "/ask":
		return s.ask(arg)
	case "/send":
		id, text, _ := strings.Cut(arg, " ")
		if id == "" || strings.TrimSpace(text) == "" {
			s.addEntry(entry{kind: "info", text: "· usage: /send <agent-id> <message> (ctrl+t lists ids)"})
		} else if s.runtime.Tree.Steer(id, strings.TrimSpace(text)) {
			s.addEntry(entry{kind: "info", text: "· sent to " + id + "; it reads it at its next turn"})
		} else {
			s.addEntry(entry{kind: "error", text: "· no running agent " + id})
		}
		s.refresh()
	case "/usage":
		return s.showUsage()
	case "/routing":
		s.addEntry(entry{kind: "info", text: agent.RoutingStatus(s.runtime, admission.DefaultManager())})
		s.refresh()
	case "/login":
		s.startLogin()
		return nil
	case "/resume":
		if f := strings.Fields(arg); len(f) > 0 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				s.resumeN(n)
				return nil
			}
		}
		return s.openResume()
	case "/model":
		if arg != "" {
			s.setModel(arg)
			return nil
		}
		return s.openPicker(false)
	case "/scoped-models":
		return s.openPicker(true)
	case "/compact-model":
		if arg != "" {
			s.setCompactModel(arg)
			return nil
		}
		cmd := s.openPicker(false)
		s.picker.compact, s.picker.onlyScoped = true, false
		s.picker.all = []string{compactDefault}
		s.picker.access[compactDefault] = "summaries are written by the conversation model"
		s.picker.apply()
		return cmd
	case "/thinking":
		if arg == "" {
			s.setThinking(s.nextLevel())
		} else {
			s.setThinking(strings.ToLower(strings.Fields(arg)[0]))
		}
	case "/mode":
		if isMode(arg) {
			s.eng.Policy.Mode = arg
			s.addEntry(entry{kind: "info", text: "· perm mode → " + arg})
		} else {
			s.cycleMode()
			return nil
		}
		s.refresh()
	case "/tree":
		s.treeOpen = !s.treeOpen
		s.refresh()
	case "/view":
		return s.setFocus(arg)
	case "/help":
		s.addEntry(entry{kind: "info", text: helpText()})
		s.refresh()
	}
	return nil
}

func isMode(m string) bool {
	return m == "manual" || m == "classify" || m == "auto-safe" || m == "auto"
}

// nextLevel cycles through the levels the current model supports.
func (s *State) nextLevel() string {
	levels, _ := agent.ModelThinking(s.modelRef)
	for i, l := range levels {
		if l == s.thinking {
			return levels[(i+1)%len(levels)]
		}
	}
	return levels[0]
}

func (s *State) setThinking(level string) {
	if s.running {
		s.addEntry(entry{kind: "info", text: "· thinking cannot change while a run is in progress — esc cancels it"})
		s.refresh()
		return
	}
	if !contains(agent.ThinkingLevels, level) {
		s.addEntry(entry{kind: "error", text: "/thinking: use " + strings.Join(agent.ThinkingLevels, "|")})
		return
	}
	levels, known := agent.ModelThinking(s.modelRef)
	s.thinking = agent.ClampThinking(s.modelRef, level)
	if err := s.rebuildAgent(s.agentHistory()); err != nil {
		s.addEntry(entry{kind: "error", text: "/thinking: " + err.Error()})
		return
	}
	note := ""
	switch {
	case !known:
		note = " (levels for " + s.modelRef + " are not reported by its provider or model card; sent as-is)"
	case s.thinking != level:
		note = " (" + level + " is not supported by " + s.modelRef + "; it supports " + strings.Join(levels, "|") + ")"
	}
	s.addEntry(entry{kind: "info", text: "· thinking → " + s.thinking + note})
}

func (s *State) cycleMode() {
	s.eng.Policy.Mode = map[string]string{"manual": "classify", "classify": "auto-safe", "auto-safe": "auto", "auto": "manual"}[s.eng.Policy.Mode]
	s.addEntry(entry{kind: "info", text: "· perm mode → " + s.eng.Policy.Mode})
	s.refresh()
}

func (s *State) setModel(ref string) {
	if s.running {
		s.addEntry(entry{kind: "info", text: "· the model cannot change while a run is in progress — esc cancels it"})
		s.refresh()
		return
	}
	prevRef, prev := s.modelRef, s.thinking
	s.modelRef = ref
	s.thinking = agent.ClampThinking(ref, s.thinking)
	if err := s.rebuildAgent(s.agentHistory()); err != nil {
		s.modelRef, s.thinking = prevRef, prev
		s.addEntry(entry{kind: "error", text: "/model: " + err.Error()})
	} else {
		note := ""
		if s.thinking != prev {
			note = " · thinking " + prev + " → " + s.thinking + " (closest level it supports)"
		}
		s.addEntry(entry{kind: "info", text: "· model → " + s.modelRef + note})
	}
	s.refresh()
}

// compactDefault is the picker row that clears the summarizer choice.
const compactDefault = "default"

// setCompactModel persists the summarizer choice and applies it to the live
// agent. "default" (or "off") returns to [compaction] model / the conversation
// model.
func (s *State) setCompactModel(ref string) {
	if s.running || s.compacting {
		s.addEntry(entry{kind: "info", text: "· the compaction model cannot change while a run or compaction is in progress"})
		s.refresh()
		return
	}
	ref = strings.TrimSpace(ref)
	clear := ref == compactDefault || ref == "off" || ref == ""
	if clear {
		ref = ""
	} else if _, err := agent.CardRef(ref); err != nil {
		s.addEntry(entry{kind: "error", text: "/compact-model: " + err.Error()})
		s.refresh()
		return
	}
	if err := provider.SaveCompactModel(ref); err != nil {
		s.addEntry(entry{kind: "error", text: "/compact-model: save: " + err.Error()})
		s.refresh()
		return
	}
	effective := agent.ContextConfigFrom(s.runtime.Config).CompactModel
	s.agent.SetCompactModel(effective)
	if effective == "" {
		s.addEntry(entry{kind: "info", text: "· compaction summaries → the conversation model"})
	} else {
		s.addEntry(entry{kind: "info", text: "· compaction summaries → " + effective})
	}
	s.refresh()
}

// agentHistory copies the current history out of the live agent.
func (s *State) agentHistory() []llm.Message {
	if s.agent == nil {
		return nil
	}
	return append([]llm.Message(nil), s.agent.History()...)
}

// ---- model picker (ctrl+l, /model) ----

// openPicker shows the sheet and queries every provider's model list. With
// scopedEdit it is the /scoped-models editor; otherwise the /model chooser.
func (s *State) openPicker(scopedEdit bool) tea.Cmd {
	scope := provider.Scope()
	scoped := map[string]bool{}
	for _, r := range scope.Refs {
		if r != "*" {
			scoped[r] = true
		}
	}
	s.picker = &pickerState{loading: true, scopedEdit: scopedEdit, scoped: scoped,
		scopeWarning: strings.Join(scope.Warnings, " · "), access: map[string]string{},
		onlyScoped: !scopedEdit && scope.State != policy.ScopeStateUnrestricted, all: []string{s.modelRef}}
	s.picker.apply()
	s.refresh()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ms, errs := provider.ListModels(ctx)
		return modelsMsg{models: ms, errs: errs}
	}
}

// setModels loads the providers' answer into the picker. A provider that
// failed is named (its cached listing, if any, is still shown).
func (p *pickerState) setModels(msg modelsMsg, current string) {
	seen := map[string]bool{}
	p.all = p.all[:0]
	if p.compact {
		p.all = append(p.all, compactDefault)
		seen[compactDefault] = true
	}
	for _, m := range msg.models {
		if r := m.Ref(); !seen[r] {
			seen[r] = true
			p.all = append(p.all, r)
		}
	}
	for _, ref := range sortedScopeRefs(p.scoped) {
		if !seen[ref] {
			p.all = append(p.all, ref)
			seen[ref] = true
		}
	}
	if current != "" && !seen[current] {
		p.all = append([]string{current}, p.all...)
	}
	if p.access == nil {
		p.access = map[string]string{}
	}
	var bad []string
	for name, err := range msg.errs {
		bad = append(bad, name+": "+err.Error())
	}
	sort.Strings(bad)
	if p.scopeWarning != "" {
		bad = append(bad, p.scopeWarning)
	}
	p.err = strings.Join(bad, " · ")
	p.apply()
	for _, ref := range p.items {
		if ref == compactDefault {
			p.access[ref] = "summaries are written by the conversation model"
			continue
		}
		status, reason := provider.ModelAccess(ref)
		p.access[ref] = string(status) + ": " + reason
	}
}

func sortedScopeRefs(scope map[string]bool) []string {
	var refs []string
	for ref, selected := range scope {
		if selected && ref != "*" {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	return refs
}

// apply recomputes the visible items from the filter and scope.
func (p *pickerState) apply() {
	cur := ""
	if p.curs < len(p.items) {
		cur = p.items[p.curs]
	}
	p.items = p.items[:0:0]
	q := strings.ToLower(p.filter)
	for _, r := range p.all {
		if p.onlyScoped && !p.scoped[r] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r), q) {
			continue
		}
		p.items = append(p.items, r)
	}
	p.curs = 0
	for i, r := range p.items {
		if r == cur {
			p.curs = i
		}
	}
}

func (p *pickerState) scopedRefs() []string {
	var out []string
	for _, r := range p.all {
		if p.scoped[r] {
			out = append(out, r)
		}
	}
	return out
}

func (s *State) pickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := s.picker
	switch k := msg.String(); k {
	case "up":
		if p.curs > 0 {
			p.curs--
		}
	case "down":
		if p.curs < len(p.items)-1 {
			p.curs++
		}
	case "pgup":
		p.curs = max(0, p.curs-8)
	case "pgdown":
		p.curs = min(max(0, len(p.items)-1), p.curs+8)
	case "tab":
		if !p.scopedEdit {
			p.onlyScoped = !p.onlyScoped
			p.apply()
		}
	case "ctrl+a", "ctrl+x":
		if p.scopedEdit {
			for _, r := range p.items {
				if k == "ctrl+a" {
					p.scoped[r] = true
				} else {
					delete(p.scoped, r)
				}
			}
			s.saveScope()
		}
	case "enter", "space":
		if len(p.items) == 0 {
			s.picker = nil
			break
		}
		ref := p.items[p.curs]
		if p.scopedEdit {
			if p.scoped[ref] {
				delete(p.scoped, ref)
			} else {
				p.scoped[ref] = true
			}
			s.saveScope()
			break
		}
		if k == "enter" {
			compact := p.compact
			s.picker = nil
			if compact {
				s.setCompactModel(ref)
			} else {
				s.setModel(ref)
			}
		}
	case "backspace":
		if p.filter != "" {
			r := []rune(p.filter)
			p.filter = string(r[:len(r)-1])
			p.apply()
		}
	case "esc":
		s.picker = nil
	default:
		if msg.Text != "" && msg.Text != " " {
			p.filter += msg.Text
			p.apply()
		}
	}
	s.refresh()
	return s, nil
}

// saveScope persists the /scoped-models selection.
func (s *State) saveScope() {
	if err := provider.SaveScoped(s.picker.scopedRefs()); err != nil {
		s.picker.err = "save scope: " + err.Error()
	}
}

// cycleScoped switches to the next (dir 1) or previous (dir -1) scoped model.
func (s *State) cycleScoped(dir int) {
	if s.running {
		return
	}
	refs := provider.Scoped()
	if len(refs) == 1 && refs[0] == "*" {
		s.addEntry(entry{kind: "info", text: "· unrestricted scope — choose a model with /model"})
		s.refresh()
		return
	}
	if len(refs) == 0 {
		s.addEntry(entry{kind: "info", text: "· no scoped models — choose some with /scoped-models"})
		s.refresh()
		return
	}
	i := slices.Index(refs, s.modelRef)
	next := refs[((i+dir)%len(refs)+len(refs))%len(refs)]
	if i < 0 && dir < 0 {
		next = refs[len(refs)-1]
	}
	s.setModel(next)
}

// ---- transcript plumbing ----

func (s *State) addEntry(e entry) {
	s.entries = append(s.entries, e)
}

// layout keeps the editor 1-10 lines tall.
func (s *State) layout() {
	h := s.ta.LineCount()
	if h < 1 {
		h = 1
	}
	if h > 10 {
		h = 10
	}
	s.ta.SetWidth(s.w)
	s.ta.SetHeight(h)
}

// oneLine collapses a permission call's command/args for one compact line.
func oneLine(c perm.Call) string {
	text := c.Command
	if text == "" {
		text = string(c.Args)
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 60 {
		return text[:60] + "…"
	}
	return text
}

func currentDir() (string, error) { return os.Getwd() }

// liveChildren counts subagents that have not finished.
func (s *State) liveChildren() int {
	n := 0
	for _, node := range s.runtime.Tree.Outline() {
		if node.Depth > 0 && node.Status != "done" && node.Status != "error" && node.Status != "turn-out" && node.Status != "cancelled" {
			n++
		}
	}
	return n
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func inboxFor(run string) string {
	p, _ := session.InboxPath(run, "root")
	return p
}
