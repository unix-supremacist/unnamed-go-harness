package static

// Triage sorts a tool call into a risk tier before any model classifier
// runs, so cheap calls get no (or one) model verdict and risky calls get
// the full panel. Tiers, in order: trivial, low, middle, high.
//
//   trivial: read-only work inside the workspace (file tools, read-only shell
//            commands, builds/tests, orchestration tools)
//   low:     edits/writes inside the workspace, scripts, unknown commands
//   middle:  mutations inside the workspace (rm, mv, git commit/reset, sed
//            -i, kill), redirects into files, installs
//   high:    network or uploads, git push, privilege escalation, anything
//            touching a sensitive path, writes outside the workspace, and
//            commands that cannot be parsed

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/unix/ugh/internal/perm"
	"mvdan.cc/sh/v3/syntax"
)

// Tier names, weakest first.
const (
	Trivial = "trivial"
	Low     = "low"
	Middle  = "middle"
	High    = "high"
)

var tierRank = map[string]int{Trivial: 0, Low: 1, Middle: 2, High: 3}

// ---- workspace ----

var (
	wsMu    sync.RWMutex
	wsRoots []string // configured workspace roots, scratch base, temp dir
)

// SetWorkspace sets the extra directories that count as inside the
// workspace (besides the cwd and its git root): configured roots, the
// agents' scratch base and the system temp directory.
func SetWorkspace(roots ...string) {
	wsMu.Lock()
	defer wsMu.Unlock()
	wsRoots = nil
	for _, r := range roots {
		if r != "" {
			wsRoots = append(wsRoots, filepath.Clean(r))
		}
	}
}

// InWorkspace reports whether full (absolute, clean) is inside the cwd, the
// cwd's git root, a configured root, the scratch base or the temp dir.
func InWorkspace(full, cwd string) bool {
	if under(full, cwd) {
		return true
	}
	if g := gitRoot(cwd); g != "" && under(full, g) {
		return true
	}
	wsMu.RLock()
	defer wsMu.RUnlock()
	for _, r := range wsRoots {
		if under(full, r) {
			return true
		}
	}
	return false
}

var gitRoots sync.Map // cwd → git root ("" when none)

func gitRoot(cwd string) string {
	if v, ok := gitRoots.Load(cwd); ok {
		return v.(string)
	}
	root := ""
	for d := cwd; d != "" && d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
	}
	gitRoots.Store(cwd, root)
	return root
}

// ---- sensitive paths ----

// sensitiveRel are home-relative locations whose contents are secret or
// control login and shells: even reading them is high tier.
var sensitiveRel = []string{
	".ssh", ".gnupg", ".aws", ".kube", ".docker/config.json", ".netrc", ".git-credentials",
	".config/ugh/credentials.json", ".pi/agent/auth.json", ".local/share/keyrings",
	".bashrc", ".bash_profile", ".profile", ".zshrc", ".zprofile", ".zshenv",
}

// Sensitive reports whether full is a secret or system-control path.
func Sensitive(full string) bool {
	if full == "/etc" || strings.HasPrefix(full, "/etc/") || strings.HasPrefix(full, "/root") {
		return true
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return false
	}
	if full == home {
		return true // the home directory itself is not a project
	}
	for _, r := range sensitiveRel {
		if p := filepath.Join(home, r); full == p || strings.HasPrefix(full, p+"/") {
			return true
		}
	}
	return false
}

// ---- triage ----

// Triage returns the call's tier and a short reason.
func Triage(c perm.Call) (string, string) {
	t := &triage{cwd: c.Cwd, tier: Trivial, reason: "read-only"}
	if Sensitive(c.Cwd) {
		t.raise(High, "working directory is sensitive: "+c.Cwd)
	}
	switch c.Tool {
	case "spawn", "wait_agents", "agent_status", "resume_agent", "council":
		t.raise(Trivial, "orchestration")
	case "ask_user":
		t.raise(Trivial, "asks the user a question")
	case "recall":
		t.raise(Trivial, "re-reads this session's own tool output")
	case "council_escalate":
		t.raise(High, "direct oracle spending after a council")
	case "worktree_status":
		t.raise(Trivial, "worktree listing")
	case "worktree_integrate", "worktree_cleanup":
		t.raise(Middle, "mutates the shared repository: "+c.Tool)
	case "read", "ls", "grep", "find":
		t.path(stringArg(c.Args, "path"), false)
	case "write", "edit":
		t.raise(Low, "workspace edit")
		t.path(stringArg(c.Args, "path"), true)
	case "bash":
		t.bash(c.Command)
	default:
		t.raise(Low, "tool "+c.Tool)
	}
	return t.tier, t.reason
}

type triage struct {
	cwd          string
	tier, reason string
}

func (t *triage) raise(tier, reason string) {
	if tierRank[tier] > tierRank[t.tier] || (t.reason == "read-only" && tierRank[tier] == tierRank[t.tier]) {
		t.tier, t.reason = tier, reason
	}
}

// path judges one path argument (write: the call modifies it).
func (t *triage) path(p string, write bool) {
	if p == "" {
		p = "."
	}
	if strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$HOME") {
		home, _ := os.UserHomeDir()
		p = home + strings.TrimPrefix(strings.TrimPrefix(p, "~"), "$HOME")
	}
	full := resolvePath(t.cwd, p)
	switch {
	case Sensitive(full):
		t.raise(High, "sensitive path "+full)
	case !InWorkspace(full, t.cwd) && write:
		t.raise(High, "write outside workspace "+full)
	case !InWorkspace(full, t.cwd):
		t.raise(Low, "read outside workspace "+full)
	}
}

// Command classes for bash triage.
var (
	readOnlyCmds = set(`cat head tail less more wc ls tree stat file grep rg egrep fgrep zgrep find fd sed awk sort uniq
		cut tr diff cmp jq yq echo printf pwd which type command basename dirname realpath readlink date javap
		unzip zipinfo test [ true false sleep du df ps pgrep nl xxd od hexdump strings column comm paste xargs
		cd pushd popd export set unset local read wait env printenv id whoami hostname uname nproc free uptime
		md5sum sha1sum sha256sum base64 seq yes time timeout`)
	buildCmds = set(`go gradle gradlew mvn make cargo npm pnpm yarn npx javac java kotlinc tsc node-gyp cmake ninja
		pytest jest vitest`)
	scriptCmds   = set(`python python3 node bash sh zsh fish ruby perl lua deno bun php`)
	mutatingCmds = set(`rm rmdir mv cp chmod chown chgrp ln kill pkill killall tar truncate install
		patch shred unlink rsync`)
	createCmds  = set(`mkdir touch`)
	networkCmds = set(`curl wget ssh scp sftp nc ncat netcat telnet ftp socat`)
	privCmds    = set(`sudo doas su pkexec`)
	pkgCmds     = set(`apt apt-get dnf yum pacman zypper apk brew pip pip3 pipx gem cargo-install`)
)

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

func (t *triage) bash(cmd string) {
	if strings.TrimSpace(cmd) == "" {
		t.raise(Low, "empty command")
		return
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		t.raise(High, "unparseable command")
		return
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.Stmt:
			t.redirects(x.Redirs)
		case *syntax.CallExpr:
			t.call(x)
		case *syntax.FuncDecl:
			t.raise(Low, "defines a shell function")
		}
		return true
	})
}

func (t *triage) call(c *syntax.CallExpr) {
	var rawArgs []string
	for _, w := range c.Args {
		lit, _ := partsLit(w.Parts)
		rawArgs = append(rawArgs, lit)
	}
	if len(rawArgs) == 0 {
		return // bare assignments (JAR=x)
	}
	args := unwrapCommand(rawArgs)
	if len(args) == 0 {
		args = rawArgs
	}
	name := baseName(args[0])
	rest := args[1:]
	// Paths named on the command line: sensitive ones raise to high.
	for _, a := range rest {
		if strings.HasPrefix(a, "/") || strings.HasPrefix(a, "~") || strings.HasPrefix(a, "$HOME") {
			if full := resolvePath(t.cwd, expandHome(a)); Sensitive(full) {
				t.raise(High, "sensitive path "+full)
			}
		}
	}
	switch {
	case privCmds[name]:
		t.raise(High, "privilege escalation ("+name+")")
	case networkCmds[name]:
		t.raise(High, "network ("+name+")")
	case IsGitBinary(name):
		tier, reason, _ := ClassifyGit(args, t.cwd)
		t.raise(tier, reason)
	case createCmds[name]:
		t.raise(Low, name)
	case pkgCmds[name] || (name == "npm" || name == "pnpm" || name == "yarn") && hasAny(rest, "install", "add", "i", "remove", "uninstall"):
		t.raise(Middle, "package install/remove ("+name+")")
	case name == "sed" && hasPrefixAny(rest, "-i", "--in-place"):
		t.raise(Middle, "sed -i (edits files)")
	case mutatingCmds[name]:
		t.raise(Middle, name+" (modifies files or processes)")
	case buildCmds[name]:
		if hasAny(rest, "publish", "deploy", "upload", "release") {
			t.raise(High, name+" publish")
		}
	case scriptCmds[name] || strings.HasSuffix(name, ".sh") || strings.HasPrefix(args[0], "./"):
		t.raise(Low, "runs a script ("+name+")")
	case readOnlyCmds[name]:
	case name == "":
		t.raise(Low, "dynamic command")
	default:
		t.raise(Low, "command "+name)
	}
}

func (t *triage) redirects(rs []*syntax.Redirect) {
	for _, r := range rs {
		if r == nil || r.Word == nil {
			continue
		}
		switch r.Op {
		case syntax.RdrOut, syntax.RdrClob, syntax.RdrAll, syntax.AppOut, syntax.AppAll:
		default:
			continue
		}
		target, _ := partsLit(r.Word.Parts)
		if target == "" || target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" || strings.HasPrefix(target, "/dev/fd/") {
			continue
		}
		full := resolvePath(t.cwd, expandHome(target))
		switch {
		case Sensitive(full):
			t.raise(High, "redirect into sensitive path "+full)
		case !InWorkspace(full, t.cwd):
			t.raise(High, "redirect outside workspace "+full)
		default:
			t.raise(Middle, "redirect into "+target)
		}
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") || strings.HasPrefix(p, "$HOME") {
		home, _ := os.UserHomeDir()
		return home + strings.TrimPrefix(strings.TrimPrefix(p, "~"), "$HOME")
	}
	return p
}

func unwrapCommand(args []string) []string {
	i := 0
	for i < len(args) {
		base := baseName(args[i])
		var next int
		switch base {
		case "env":
			next = skipStringEnv(args, i+1)
		case "time", "command", "builtin", "exec", "nohup", "setsid":
			next = skipStringFlags(args, i+1)
		default:
			next = -1
		}
		if next < 0 || next >= len(args) || next <= i {
			break
		}
		i = next
	}
	if i < len(args) {
		return args[i:]
	}
	return args
}

func skipStringEnv(args []string, i int) int {
	for i < len(args) {
		l := args[i]
		if strings.HasPrefix(l, "-") {
			i++
			if (l == "-u" || l == "--unset" || l == "-S" || l == "--split-string") && i < len(args) && !strings.HasPrefix(args[i], "-") {
				i++
			}
			continue
		}
		if strings.Contains(l, "=") {
			i++
			continue
		}
		break
	}
	return i
}

func skipStringFlags(args []string, i int) int {
	for i < len(args) {
		if strings.HasPrefix(args[i], "-") {
			i++
			continue
		}
		break
	}
	return i
}

func hasAny(args []string, words ...string) bool {
	for _, a := range args {
		for _, w := range words {
			if a == w {
				return true
			}
		}
	}
	return false
}

func hasPrefixAny(args []string, prefixes ...string) bool {
	for _, a := range args {
		for _, p := range prefixes {
			if strings.HasPrefix(a, p) {
				return true
			}
		}
	}
	return false
}
