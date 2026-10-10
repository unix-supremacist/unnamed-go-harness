// Command ugh is the harness CLI. M0 ships the auth subcommands; the TUI and
// chat commands arrive with later milestones.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/provider"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ugh:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "auth":
			return authCmd(args[1:])
		case "login":
			return loginCmd(args[1:])
		case "eval":
			return evalCmd(args[1:])
		case "tree":
			return treeCmd(args[1:])
		case "usage", "quota":
			return usageCmd(args[1:])
		case "send":
			return sendCmd(args[1:])
		case "watch":
			return watchCmd(args[1:])
		case "log":
			return logCmd(args[1:])
		case "help", "-h", "--help":
			usage(os.Stdout)
			return nil
		default:
			if !strings.HasPrefix(args[0], "-") {
				usage(os.Stderr)
				return fmt.Errorf("unknown command %q", args[0])
			}
		}
	}
	return chatCmd(args)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: ugh        (interactive TUI)
       ugh [flags] -p "prompt"     (headless run)
       ugh <command>

flags:
  (none)      launch the interactive TUI
  -p "..."    run one prompt headless and exit
  -m ref      model ref provider/model (default: config models.default, else
              OpenRouter MiMo Pro, else OpenCode Go MiMo Pro)
  -thinking   off|minimal|low|medium|high|xhigh|max (default high; per model, clamped)
  -cwd dir    working directory (default: current)
  -system s   system prompt
  -mode m     permission mode: manual|classify|auto-safe|auto (default classify)
              headless prompts arrive on stdin (y/a/n/s); no stdin →
              headless_prompt; the TUI shows them as a modal
  -resume     continue the most recent session for this cwd
  -max-turns  root turn budget (default: config agent.max_turns_root, 200)

commands:
  auth list              show stored credentials (masked) and the backend in use
  auth set <provider>    store a key read from stdin (no echo, never from argv)
  auth rm <provider>     remove a stored credential
  auth import pi         one-shot import of static keys from Pi's auth.json
                         flags: --path P --only a,b --codex --force
  login codex            independent Codex OAuth login (browser PKCE)
  login opencode         store an OpenCode API key (Go, Zen)
  login opencode-console OpenCode Console login (browser); exposes BYOK connections
                         (e.g. opencode-openrouter) as providers
  login <provider>       store a key for a [providers.<name>] config entry
                         flags: --device (device code) --bg (detach) --status
  usage                  subscription/credit usage: OpenCode Go windows, Codex
                         5h/weekly windows, OpenRouter spend and balance
  watch                  live tree of the most recently active run (or
                         --session FILE): status, turns, idle time and what
                         each agent is doing now; works on a running ugh
  send <agent-id> <msg>  message a running agent (--subtree: and its running
                         descendants); it arrives before the agent's next turn
  log <agent-id> [-f]    one agent's transcript with tool calls and results
                         (-f follows, --full shows whole tool output)
  tree --session FILE    print a run's agent tree (status, model, turns,
                         tokens, session file) from any of its session files
  eval council           run evals/council/*.md through council vs single vs
                         oracle; flags: --q FILE --out DIR --research --timeout S

TUI keys: enter send · alt+enter newline · ctrl+l model · ctrl+p next scoped model · ctrl+r resume ·
shift+tab perm mode · ctrl+o expand tools · esc cancel · /help
`)
}

func authCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("auth: expected list|set|rm|import")
	}
	switch args[0] {
	case "list":
		return authList()
	case "set":
		return authSet(args[1:])
	case "rm":
		return authRm(args[1:])
	case "import":
		return authImport(args[1:])
	default:
		return fmt.Errorf("auth: unknown subcommand %q", args[0])
	}
}

func openStore() (*auth.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return auth.Open(cfg.Auth.Store)
}

func authList() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	names, creds, err := st.List()
	if err != nil {
		return err
	}
	// Known providers keep their display order; stored extras follow
	// alphabetically; absent ones are skipped below.
	var extras []string
	for _, p := range names {
		if !contains(known(), p) {
			extras = append(extras, p)
		}
	}
	sort.Strings(extras)
	order := append(known(), extras...)

	fmt.Printf("backend: %s", st.Backend().Name())
	if p, ok := st.Backend().(interface{ Path() string }); ok {
		fmt.Printf(" (%s)", p.Path())
	}
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	for _, p := range order {
		cred, exists := creds[p]
		envVar := auth.EnvVar(p)
		envVal := ""
		if envVar != "" {
			if v, ok := os.LookupEnv(envVar); ok && v != "" {
				envVal = v
			}
		}
		if !exists && envVal == "" {
			continue
		}
		typ := cred.Type
		if typ == "" {
			typ = "api_key"
		}
		value, source := cred.Key, "store"
		if envVal != "" {
			value, source = envVal, "env"
		} else if cred.Type == "oauth" {
			value = cred.Access
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s", p, typ, auth.MaskKey(value), source)
		if cred.Expires > 0 {
			line += fmt.Sprintf("\texpires %s", time.UnixMilli(cred.Expires).UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(tw, line)
	}
	return tw.Flush()
}

func isKnown(p string) bool { return contains(known(), p) }

// known lists the providers ugh manages, in display order.
func known() []string {
	_ = provider.Specs() // registers config-defined providers with auth
	return append(auth.KnownAPIKeyProviders(), "codex")
}

func authSet(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ugh auth set <provider>")
	}
	provider := args[0]
	if !contains(auth.KnownAPIKeyProviders(), provider) {
		return fmt.Errorf("auth: set supports static providers (%s)", strings.Join(auth.KnownAPIKeyProviders(), ", "))
	}
	key, err := readKey()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	if err := st.Put(provider, auth.Credential{Type: "api_key", Key: key}); err != nil {
		return err
	}
	fmt.Printf("stored %s (%s)\n", provider, auth.MaskKey(key))
	return nil
}

func authRm(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ugh auth rm <provider>")
	}
	provider := args[0]
	if !isKnown(provider) && provider != auth.ConsoleCredential {
		return fmt.Errorf("auth: unknown provider %q", provider)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	if err := st.Remove(provider); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", provider)
	return nil
}

func authImport(args []string) error {
	if len(args) == 0 || args[0] != "pi" {
		return errors.New("usage: ugh auth import pi [--path P] [--only a,b] [--codex] [--force]")
	}
	fs := flag.NewFlagSet("auth import pi", flag.ContinueOnError)
	var (
		path  = fs.String("path", "", "path to Pi's auth.json (default ~/.pi/agent/auth.json)")
		only  = fs.String("only", "", "comma-separated providers to import")
		codex = fs.Bool("codex", false, "also import openai-codex OAuth (shared rotating refresh token)")
		force = fs.Bool("force", false, "overwrite existing store entries")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("auth import pi: unexpected argument %q", fs.Arg(0))
	}
	var onlyList []string
	if *only != "" {
		onlyList = strings.Split(*only, ",")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	res, err := auth.ImportPi(st, auth.ImportOptions{Path: *path, Only: onlyList, Codex: *codex, Force: *force})
	if err != nil {
		return err
	}
	for _, line := range res.Imported {
		fmt.Println("imported:", line)
	}
	if len(res.Skipped) > 0 {
		fmt.Println("skipped:", strings.Join(res.Skipped, ", "))
	}
	for _, n := range res.Warnings {
		fmt.Println("warning:", n)
	}
	for _, n := range res.Notes {
		fmt.Println("note:", n)
	}
	return nil
}

// readKey reads a key from stdin with no echo when attached to a terminal.
// Keys are never taken from argv.
func readKey() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "key: ")
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
