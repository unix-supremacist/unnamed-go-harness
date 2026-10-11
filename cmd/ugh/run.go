// Headless chat: `ugh -p "..."`. Wiring for the OpenCode Go provider (M1),
// permission gate (M2) and resumable sessions (M3); the default (no -p)
// launches the TUI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/unix/ugh/internal/admission"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/unix/ugh/internal/agent"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/policy"
	"github.com/unix/ugh/internal/provider"
	"github.com/unix/ugh/internal/session"
	"github.com/unix/ugh/internal/tools"
	"github.com/unix/ugh/internal/tui"
	"golang.org/x/term"
)

// defaultModelRef honors an explicit configured default, otherwise the first
// model of the policy's default root scope (OpenRouter MiMo Pro when an
// OpenRouter key exists). The saved /scoped-models list does not pick it. No
// credentials at all means no default rather than a route that cannot work.
func defaultModelRef() string {
	if cfg, err := config.Load(); err == nil && cfg.Models.Default != "" {
		return cfg.Models.Default
	}
	if refs := provider.DefaultRootScope(); len(refs) > 0 {
		return refs[0]
	}
	return ""
}

// chatCmd parses the default (no-subcommand) invocation.
func chatCmd(args []string) error {
	fs := flag.NewFlagSet("ugh", flag.ContinueOnError)
	prompt := fs.String("p", "", "run one prompt headless and exit")
	model := fs.String("m", "", "model ref: provider/model (default: config models.default, else OpenRouter MiMo Pro, else OpenCode Go)")
	thinking := fs.String("thinking", "", "thinking level: off|minimal|low|medium|high|xhigh|max (default high; clamped to what the model supports)")
	system := fs.String("system", "", "system prompt (default: terse coding agent)")
	cwd := fs.String("cwd", "", "working directory (default: current)")
	mode := fs.String("mode", "", "permission mode: manual|classify|auto-safe|auto (default: config, classify)")
	resume := fs.Bool("resume", false, "continue the most recent session for this cwd")
	maxTurns := fs.Int("max-turns", 0, "turn budget for the root agent (default: config agent.max_turns_root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, err := config.Path()
	if err != nil {
		return err
	}
	warnings, err := policy.MigrateConfig(&cfg, path)
	if err != nil {
		return fmt.Errorf("configuration migration: %w", err)
	}
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	// Context windows and thinking levels come from the cached listings;
	// keep them current in the background (at most daily).
	go provider.RefreshModelsIfStale(context.Background(), 24*time.Hour)
	if *model == "" {
		*model = defaultModelRef()
	}
	if *model == "" && !*resume {
		return fmt.Errorf("no eligible root models in the current scope; log in, configure a scope, or choose an explicit -m provider/model")
	}
	dir := *cwd
	if dir == "" {
		d, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = d
	}
	sys := *system
	if sys == "" {
		sys = agent.DefaultSystem
	}
	switch {
	case *prompt != "":
		return headless(*prompt, *model, *thinking, sys, dir, *mode, *resume, *maxTurns)
	case fs.NArg() > 0:
		return fmt.Errorf("unknown argument %q (use -p \"...\" for headless, no args for the TUI)", fs.Arg(0))
	default:
		return tui.Main(tui.Options{
			Model: *model, Thinking: *thinking, System: *system,
			Cwd: dir, Mode: *mode, Resume: *resume, MaxTurns: *maxTurns,
		})
	}
}

// headless runs one prompt through the permission gate and streams the
// transcript (assistant text on stdout, tool trace and prompts on stderr).
// With -resume, the previous session's history seeds the run and new
// messages append to the same JSONL file.
func headless(prompt, modelRef, thinking, system, cwd, mode string, resume bool, maxTurns int) error {
	runtime := agent.NewRuntime()
	var hist []llm.Message
	var sess *session.Store
	var councils, archive []session.Record
	if resume {
		path, err := session.Latest(cwd)
		if err != nil {
			return err
		}
		loaded, err := session.Load(path)
		if err != nil {
			return err
		}
		hist = loaded.Messages
		councils = loaded.Councils
		archive = loaded.Display
		if loaded.Meta.Run != "" {
			runtime.RunID = loaded.Meta.Run // children of the resumed run stay linked
		}
		if loaded.Meta.Model != "" {
			modelRef = loaded.Meta.Model
		} else {
			return fmt.Errorf("session %s has no meta header", path)
		}
		if loaded.Meta.Thinking != "" && thinking == "" {
			thinking = loaded.Meta.Thinking
		}
		if thinking == "" {
			thinking = agent.DefaultThinking
		}
		if loaded.Meta.System != "" {
			system = loaded.Meta.System
		}
		sess, err = session.Open(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[resumed %d messages from %s]\n", len(hist), path)
	} else {
		if thinking == "" {
			thinking = agent.DefaultThinking
		}
		var err error
		sess, err = session.Create(cwd, session.Record{Model: modelRef, Thinking: thinking, System: system, Cwd: cwd, Run: runtime.RunID})
		if err != nil {
			return err
		}
	}
	defer sess.Close()
	if thinking != "" {
		if t := agent.ClampThinking(modelRef, thinking); t != thinking {
			fmt.Fprintf(os.Stderr, "[thinking %s → %s: closest level %s supports]\n", thinking, t, modelRef)
			thinking = t
		}
	}
	prov, model, err := provider.Resolve(modelRef)
	if err != nil {
		return err
	}
	gate, err := newGate(mode)
	if err != nil {
		return err
	}
	if maxTurns <= 0 {
		maxTurns = runtime.Config.Agent.MaxTurnsRoot
	}
	a := agent.New(agent.Config{
		MaxTurns: maxTurns,
		Provider: prov, Model: model, System: system, Thinking: thinking,
		Tools: tools.NewRegistry(tools.All()...), Cwd: cwd, Gate: gate,
		Out: os.Stdout, Trace: os.Stderr, History: hist,
		Tree: runtime.Tree, Spawn: runtime.Spawn, StartChild: runtime.StartChild, Resume: runtime.ResumeChild, StreamLimit: runtime.Limit, Runtime: runtime,
		OnMessage:    func(m llm.Message) { _ = sess.Append(session.Record{Type: "message", Message: &m}) },
		OnCompact:    func(h []llm.Message) { _ = sess.Append(session.Record{Type: "compaction", Seed: h}) },
		OnRecord:     func(rec session.Record) { _ = sess.Append(rec) },
		OnStatus:     func(st string) { _ = sess.Append(session.Record{Type: "status", Status: st}) },
		AutoCompact:  runtime.Config.Agent.AutoCompact,
		Inbox:        rootInbox(runtime.RunID),
		ToolTimeout:  time.Duration(runtime.Config.Tools.TimeoutS) * time.Second,
		ProviderName: providerOf(modelRef), Admission: agent.DefaultAdmission(admission.PriorityRoot, "root"),
		Scratch: session.ScratchPath(runtime.RunID, "root"),
		Archive: archive, BlobDir: session.BlobDir(sess.Path()),
		Context: agent.ContextConfigFrom(runtime.Config),
	})
	for _, rec := range councils {
		a.RestoreCouncil(rec) // outcomes replay; approvals never do
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	final, err := a.Run(ctx, prompt)
	rec := session.Record{Type: "result", Run: runtime.RunID, Agent: "root", Model: modelRef,
		Status: "ok", Turns: a.Turns, Usage: &a.Usage}
	switch {
	case err == nil:
	case errors.Is(err, agent.ErrTurnOut):
		rec.Status = "turn-out"
	case ctx.Err() != nil:
		rec.Status = "cancelled"
	default:
		rec.Status = "error"
	}
	if err != nil {
		rec.Error = err.Error()
	}
	_ = sess.Append(rec)
	var turnOut *agent.TurnOutError
	if errors.As(err, &turnOut) && !turnOut.Answered {
		fmt.Println(final) // the [TURN-OUT] report; an answered turn-out already streamed
	}
	if len(a.Denied) > 0 {
		fmt.Fprintf(os.Stderr, "[%d tool call(s) denied by the permission policy:\n- %s]\n", len(a.Denied), strings.Join(a.Denied, "\n- "))
	}
	if err != nil {
		return err
	}
	// Auto mode classifies in the background; give it a moment to land in
	// decisions.jsonl before the process exits.
	if w, ok := gate.(interface{ WaitBackground(time.Duration) }); ok {
		w.WaitBackground(6 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "[%s · %d in / %d out / %d cached · %s]\n",
		modelRef, a.Usage.In, a.Usage.Out, a.Usage.Cached, time.Since(start).Round(time.Millisecond))
	return nil
}

// newGate builds the stdin-broker permission engine (same classifiers,
// policy and decision log the TUI uses through its channel broker).
func newGate(mode string) (perm.Gate, error) {
	b := perm.NewBroker(os.Stdin, os.Stderr, stdinInteractive(), "")
	eng, err := provider.Gate(mode, b)
	if err != nil {
		return nil, err
	}
	// config decides what an unattended prompt becomes
	b.SetHeadless(eng.Policy.HeadlessPrompt)
	return eng, nil
}

// stdinInteractive reports whether prompts may read answers from stdin: a
// terminal, or a pipe/file with lines coming (EOF falls back to the headless
// policy). A char device that is not a terminal (/dev/null) is headless.
func stdinInteractive() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	if st.Mode()&os.ModeCharDevice != 0 {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	return true
}

func rootInbox(run string) string {
	p, _ := session.InboxPath(run, "root")
	return p
}

func providerOf(ref string) string { p, _, _ := strings.Cut(ref, "/"); return p }
