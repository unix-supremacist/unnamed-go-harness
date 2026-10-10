package static

import (
	"os"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// §4.4 rules live here: a structural walk over the parsed bash AST that
// unwraps wrapper prefixes (sudo/env/nice/xargs/...), walks into `sh -c`
// strings, command substitutions, pipelines and find -exec, then applies the
// normal (stop) and extreme (prompt) rule sets.

var (
	forkBombRx = regexp.MustCompile(`(?:\w+|:)\s*\(\s*\)\s*\{[^{}]*\|[^{}]*&`)
	blockDevRx = regexp.MustCompile(`^/dev/(sd|hd|vd|xvd|nvme|mmcblk|loop|dm-|md\d|disk/by-)`)
	sqlBadRx   = regexp.MustCompile(`(?i)\b(drop|truncate|delete)\b`)
	sqlWhereRx = regexp.MustCompile(`(?i)\bwhere\b`)
	remoteRx   = regexp.MustCompile(`^[^/:][^:]*:`)
)

// word is one shell word: its literal text plus whether it can expand into
// something else (variable, command substitution, arithmetic, glob).
type word struct {
	lit string
	dyn bool
}

type analyzer struct {
	cwd   string
	depth int
	f     *findings
	subs  [][]*syntax.Stmt
}

// analyzeCommand parses cmd and applies the rules. Parse failures fall back
// to token heuristics and always flag as extreme ("unparseable").
func analyzeCommand(cmd, cwd string, depth int) findings {
	var f findings
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return f
	}
	if forkBombRx.MatchString(cmd) {
		f.setNormal("fork bomb")
	}
	if depth > 8 {
		return f
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "cmd")
	if err != nil {
		// Unparseable commands are high tier in triage, so the full model
		// panel judges them; the token heuristics still catch rm -rf & co.
		heuristic(&f, cmd)
		return f
	}
	a := &analyzer{cwd: cwd, depth: depth, f: &f}
	syntax.Walk(file, func(n syntax.Node) bool {
		switch t := n.(type) {
		case *syntax.CmdSubst:
			a.subs = append(a.subs, t.Stmts)
		case *syntax.ProcSubst:
			a.subs = append(a.subs, t.Stmts)
		}
		return true
	})
	a.stmts(file.Stmts)
	for _, s := range a.subs {
		a.stmts(s)
	}
	return f
}

// heuristic is the token fallback for unparseable input: rm with recursive or
// force flags on a root-ish target still counts as normal.
func heuristic(f *findings, cmd string) {
	repl := strings.NewReplacer(";", " ", "|", " ", "&", " ", "\n", " ", "\t", " ", `"`, " ", `'`, " ", `\`, " ")
	toks := strings.Fields(repl.Replace(cmd))
	hasRM, hasFlag, hasTarget := false, false, false
	for _, t := range toks {
		if strings.HasSuffix(t, "/rm") || t == "rm" {
			hasRM = true
			continue
		}
		switch t {
		case "-r", "-f", "-rf", "-fr", "--recursive", "--force":
			hasFlag = true
		case "/", "/*", "~", "~/", "$HOME", "/etc", "/usr", "/bin", "/boot", "/var", "/home", "/root", "/opt", "/srv", "/lib":
			hasTarget = true
		}
	}
	if hasRM && hasFlag && hasTarget {
		f.setNormal("unparseable rm on a system path")
	}
}

// ---- structural walk ----

func (a *analyzer) stmts(list []*syntax.Stmt) {
	for _, s := range list {
		if s != nil {
			a.stmt(s)
		}
	}
}

func (a *analyzer) stmt(s *syntax.Stmt) {
	switch c := s.Cmd.(type) {
	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			a.pipeline(c)
		} else {
			a.stmt(c.X)
			a.stmt(c.Y)
		}
	case *syntax.CallExpr:
		a.call(c)
	case *syntax.Block:
		a.stmts(c.Stmts)
	case *syntax.Subshell:
		a.stmts(c.Stmts)
	case *syntax.IfClause:
		a.stmts(c.Cond)
		a.stmts(c.Then)
		for e := c.Else; e != nil; e = e.Else {
			a.stmts(e.Cond)
			a.stmts(e.Then)
		}
	case *syntax.WhileClause:
		a.stmts(c.Cond)
		a.stmts(c.Do)
	case *syntax.ForClause:
		if wi, ok := c.Loop.(*syntax.WordIter); ok {
			for _, it := range wi.Items {
				a.wordOf(it)
			}
		}
		a.stmts(c.Do)
	case *syntax.CaseClause:
		a.wordOf(c.Word)
		for _, it := range c.Items {
			for _, p := range it.Patterns {
				a.wordOf(p)
			}
			a.stmts(it.Stmts)
		}
	case *syntax.FuncDecl:
		if c.Body != nil {
			a.stmt(c.Body)
		}
	case *syntax.TimeClause:
		if c.Stmt != nil {
			a.stmt(c.Stmt)
		}
	case *syntax.CoprocClause:
		if c.Stmt != nil {
			a.stmt(c.Stmt)
		}
	}
	a.redirects(s.Redirs)
}

// pipeline analyzes each element and applies the remote-script rule:
// curl|wget … | sh/bash/python is an extreme class (§4.4).
func (a *analyzer) pipeline(b *syntax.BinaryCmd) {
	var elems []*syntax.Stmt
	flattenPipe(b, &elems)
	for _, e := range elems {
		a.stmt(e)
	}
	if len(elems) < 2 {
		return
	}
	last := stmtName(elems[len(elems)-1])
	if !shellInterpreters[last] {
		return
	}
	for _, e := range elems[:len(elems)-1] {
		src := stmtName(e)
		switch {
		case src == "":
		case networkCmds[src]:
			// Remote content run unread: curl … | sh, wget -O- | python3.
			a.f.setExtreme("remote content (" + src + ") piped into " + last)
			return
		case shells[last]:
			// A shell runs whatever it is fed. echo/printf text is judged
			// as the command it is (echo rm -rf / | sh); other input to a
			// shell is opaque, so it prompts.
			if src == "echo" || src == "printf" {
				if text := literalArgs(e); text != "" {
					a.f.merge(analyzeCommand(text, a.cwd, a.depth+1))
				}
			}
			a.f.setExtreme("input piped into " + last)
			return
		}
		// Local input into a language interpreter (cat x | python3) is
		// ordinary scripting: triage and the model panel judge it.
	}
}

// shells execute their stdin as commands (unlike python & co, whose
// piped input is usually a script the agent just wrote).
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true}

// literalArgs joins a simple command's literal arguments (after the name).
func literalArgs(s *syntax.Stmt) string {
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Args) < 2 {
		return ""
	}
	var parts []string
	for _, w := range c.Args[1:] {
		lit, _ := partsLit(w.Parts)
		parts = append(parts, lit)
	}
	return strings.Join(parts, " ")
}

func flattenPipe(b *syntax.BinaryCmd, out *[]*syntax.Stmt) {
	if b.Op == syntax.Pipe || b.Op == syntax.PipeAll {
		if inner, ok := b.X.Cmd.(*syntax.BinaryCmd); ok && (inner.Op == syntax.Pipe || inner.Op == syntax.PipeAll) {
			flattenPipe(inner, out)
		} else {
			*out = append(*out, b.X)
		}
		*out = append(*out, b.Y)
		return
	}
	*out = append(*out, b.X, b.Y)
}

func stmtName(s *syntax.Stmt) string {
	if s == nil {
		return ""
	}
	if c, ok := s.Cmd.(*syntax.CallExpr); ok && len(c.Args) > 0 {
		w, _ := partsLit(c.Args[0].Parts)
		return baseName(w)
	}
	return ""
}

// redirects applies the redirection rules (block device → normal, system
// path / truncating an existing non-workspace file → extreme).
func (a *analyzer) redirects(rs []*syntax.Redirect) {
	for _, r := range rs {
		if r == nil || r.Word == nil {
			continue
		}
		switch r.Op {
		case syntax.RdrOut, syntax.RdrClob, syntax.RdrAll, syntax.AppOut, syntax.AppAll:
		default:
			continue
		}
		w := a.wordOf(r.Word)
		t := w.lit
		if t == "" || t == "/dev/null" || t == "/dev/stdout" || t == "/dev/stderr" || strings.HasPrefix(t, "/dev/fd/") {
			continue
		}
		if blockDevRx.MatchString(t) {
			a.f.setNormal("redirection to raw block device " + t)
			continue
		}
		if systemPath(t) {
			a.f.setExtreme("redirection to system path " + t)
			continue
		}
		full := resolvePath(a.cwd, t)
		truncates := r.Op == syntax.RdrOut || r.Op == syntax.RdrClob || r.Op == syntax.RdrAll
		if truncates && !InWorkspace(full, a.cwd) && exists(full) {
			a.f.setExtreme("truncates existing file outside workspace " + t)
		}
	}
}

// ---- simple commands ----

func (a *analyzer) call(c *syntax.CallExpr) {
	if len(c.Args) == 0 {
		return
	}
	args := make([]word, 0, len(c.Args))
	for _, w := range c.Args {
		args = append(args, a.wordOf(w))
	}
	// Unwrap wrapper prefixes, analyzing what they lead to.
	i := 0
	for i < len(args) {
		if args[i].dyn && args[i].lit == "" {
			break
		}
		var next int
		switch base := baseName(args[i].lit); base {
		case "sudo", "doas":
			a.f.setExtreme("privilege escalation (" + base + ")")
			next = skipFlags(args, i+1, sudoValueFlags)
		case "env":
			next = skipEnv(args, i+1)
		case "timeout":
			j := i + 1
			if j < len(args) && !strings.HasPrefix(args[j].lit, "-") {
				j++
			}
			next = skipFlags(args, j, timeoutValueFlags)
		case "nice":
			next = skipFlags(args, i+1, niceValueFlags)
		case "stdbuf":
			next = skipFlags(args, i+1, stdbufValueFlags)
		case "ionice", "chrt":
			next = skipFlags(args, i+1, ioniceValueFlags)
		case "time", "command", "builtin", "exec", "nohup", "setsid", "capsh", "busybox":
			next = skipFlags(args, i+1, nil)
		case "xargs":
			j := i + 1
			for j < len(args) && (strings.HasPrefix(args[j].lit, "-") || allDigits(args[j].lit)) {
				j++
			}
			next = j
		default:
			next = -1
		}
		if next < 0 || next <= i {
			break
		}
		i = next
	}
	if i >= len(args) {
		return
	}
	a.argv(args[i], args[i+1:])
}

// argv applies the per-command rules to one (possibly prefix-stripped) command.
func (a *analyzer) argv(name word, rest []word) {
	if name.dyn {
		a.unresolved(name, rest)
		return
	}
	base := baseName(name.lit)
	if base == "" {
		a.unresolved(name, rest)
		return
	}
	if len(rest) == 1 && isHelpFlag(rest[0].lit) {
		return // "--help"-only invocations do nothing
	}
	if strings.HasPrefix(base, "mkfs") {
		a.f.setNormal("disk format (" + base + ")")
		return
	}
	switch base {
	case "rm":
		a.ruleRM(rest)
	case "wipefs":
		if anyTargetPrefix(rest, "/dev/") {
			a.f.setNormal("wipefs on a device")
		} else {
			a.f.setExtreme("wipefs")
		}
	case "dd":
		a.ruleDD(rest)
	case "shred":
		if anyTargetPrefix(rest, "/dev/") {
			a.f.setNormal("shred on a device")
		} else {
			a.f.setExtreme("shred")
		}
	case "chmod", "chown":
		a.ruleChmod(base, rest)
	case "mv":
		if t := firstNonFlag(rest); t == "/" || strings.HasPrefix(t, "/*") {
			a.f.setNormal("mv from root " + t)
		}
	case "git":
		a.ruleGit(rest)
	case "find":
		a.ruleFind(rest)
	case "kill":
		if hasFlagExact(rest, "-9") || hasFlagContaining(rest, "KILL") {
			a.f.setExtreme("kill -9")
		}
	case "pkill", "killall":
		a.f.setExtreme(base)
	case "truncate":
		a.f.setExtreme("truncate")
	case "sudo", "doas":
		a.f.setExtreme("privilege escalation (" + base + ")")
		j := skipFlags(rest, 0, sudoValueFlags)
		if j < len(rest) {
			a.argv(rest[j], rest[j+1:])
		}
	case "apt", "apt-get", "aptitude", "dnf", "yum", "zypper", "apk", "pacman", "emerge", "xbps-install", "xbps-remove":
		a.rulePkg(base, rest, sysPkgSubs)
	case "pip", "pip3", "npm", "pnpm", "yarn", "bun", "brew", "cargo", "gem", "conda":
		a.rulePkg(base, rest, langPkgSubs)
	case "docker", "podman":
		a.ruleContainer(rest)
	case "psql", "mysql", "mariadb", "sqlite3":
		a.ruleSQL(rest)
	case "scp":
		a.f.setExtreme("scp to a remote host")
	case "rsync":
		for _, w := range rest {
			if w.dyn || strings.HasPrefix(w.lit, "-") {
				continue
			}
			if strings.HasPrefix(w.lit, "::") || strings.Contains(w.lit, "@") || remoteRx.MatchString(w.lit) {
				a.f.setExtreme("rsync to a remote host")
				return
			}
		}
	case "nc", "netcat", "ncat":
		if hasFlagExact(rest, "-e") || hasFlagExact(rest, "-c") || hasFlagExact(rest, "--exec") || hasFlagExact(rest, "--sh-exec") {
			a.f.setExtreme("netcat with exec")
		}
	case "curl":
		a.ruleCurl(rest)
	case "wget":
		if hasFlagPrefix(rest, "--post-file") || hasFlagPrefix(rest, "--upload-file") {
			a.f.setExtreme("wget upload")
		}
	case "sh", "bash", "zsh", "dash", "ksh", "ash":
		a.ruleShellC(rest)
	case "eval":
		if s := joinLits(rest); s != "" {
			a.f.merge(analyzeCommand(s, a.cwd, a.depth+1))
		}
	}
}

// unresolved handles command names we cannot resolve statically.
func (a *analyzer) unresolved(name word, rest []word) {
	// An unresolvable command name with a -c string (e.g. "$SHELL" -c "...")
	// is still a shell invocation: judge the string itself.
	for i, w := range rest {
		if w.lit == "-c" && i+1 < len(rest) && !rest[i+1].dyn && rest[i+1].lit != "" {
			a.f.merge(analyzeCommand(rest[i+1].lit, a.cwd, a.depth+1))
			break
		}
	}
	all := append([]word{name}, rest...)
	for _, w := range all {
		if b := baseName(w.lit); b != "" && dangerousWord(b) {
			a.f.setExtreme("hidden dangerous command " + b)
			break
		}
	}
	if hasRecForce(all) && hasRootish(all, a.cwd) {
		a.f.setExtreme("unresolved command with destructive flags")
	}
}

// ---- rule sets ----

func (a *analyzer) ruleRM(args []word) {
	rec, force, unknown := false, false, false
	var targets []string
	for _, w := range args {
		l := w.lit
		switch {
		case l == "--recursive":
			rec = true
			continue
		case l == "--force":
			force = true
			continue
		case len(l) > 1 && strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--"):
			if w.dyn {
				unknown = true
			}
			for _, ch := range l[1:] {
				switch ch {
				case 'r', 'R':
					rec = true
				case 'f':
					force = true
				}
			}
			continue
		}
		if w.dyn {
			unknown = true // could expand into flags
		}
		if l != "" {
			targets = append(targets, l)
		}
	}
	for _, t := range targets {
		if !isRootish(t, a.cwd) {
			if rec && blockDevRx.MatchString(cutGlob(t)) {
				a.f.setNormal("recursive rm on block device " + t)
			}
			continue
		}
		if rec || unknown {
			a.f.setNormal("recursive rm on " + t)
		} else {
			a.f.setExtreme("rm on " + t)
		}
	}
	if rec || force {
		a.f.setExtreme("rm with recursive/force flags")
	}
}

func (a *analyzer) ruleDD(args []word) {
	for _, w := range args {
		v, ok := strings.CutPrefix(w.lit, "of=")
		if !ok {
			continue
		}
		if blockDevRx.MatchString(v) {
			a.f.setNormal("dd onto raw device " + v)
			return
		}
		if strings.HasPrefix(v, "/dev/") || w.dyn {
			a.f.setExtreme("dd onto " + v)
			return
		}
	}
}

func (a *analyzer) ruleChmod(base string, args []word) {
	rec := hasFlagExact(args, "-R") || hasFlagPrefix(args, "--recursive") ||
		hasShortFlagWith(args, 'R')
	perm777 := false
	for _, w := range args {
		if w.lit == "777" || w.lit == "0777" {
			perm777 = true
		}
	}
	for _, t := range nonFlags(args) {
		if isRootish(t, a.cwd) && rec {
			a.f.setNormal(base + " -R on " + t)
		}
	}
	if perm777 {
		a.f.setExtreme(base + " 777")
	}
	if rec && !perm777 {
		// recursive chmod outside the workspace is at least an extreme class
		for _, t := range nonFlags(args) {
			if !InWorkspace(resolvePath(a.cwd, t), a.cwd) {
				a.f.setExtreme(base + " -R outside workspace")
				break
			}
		}
	}
}

func (a *analyzer) ruleGit(args []word) {
	nf := nonFlags(args)
	if len(nf) == 0 {
		return
	}
	switch nf[0] {
	case "clean":
		if hasFlagPrefix(args, "--force") || hasShortFlagWith(args, 'f') {
			a.f.setExtreme("git clean -f")
		}
	case "reset":
		if hasFlagExact(args, "--hard") {
			a.f.setExtreme("git reset --hard")
		}
	case "push":
		if hasFlagContaining(args, "force") || hasShortFlagWith(args, 'f') {
			a.f.setExtreme("git push --force")
		}
	case "checkout":
		for _, t := range nf[1:] {
			if t == "." || t == "./" {
				a.f.setExtreme("git checkout -- .")
				return
			}
		}
	case "branch":
		if hasFlagExact(args, "-D") || hasShortFlagWith(args, 'D') {
			a.f.setExtreme("git branch -D")
		}
	}
}

func (a *analyzer) ruleFind(args []word) {
	for i, w := range args {
		switch w.lit {
		case "-delete":
			a.f.setExtreme("find -delete")
		case "-exec", "-execdir":
			j := i + 1
			var sub []word
			for j < len(args) && args[j].lit != ";" && args[j].lit != "+" {
				sub = append(sub, args[j])
				j++
			}
			if len(sub) > 0 {
				a.argv(sub[0], sub[1:])
			}
		}
	}
}

func (a *analyzer) rulePkg(base string, args []word, subs map[string]bool) {
	if base == "pacman" {
		if hasFlagPrefix(args, "-S") || hasFlagPrefix(args, "-R") {
			a.f.setExtreme("pacman install/remove")
			return
		}
	}
	if nf := nonFlags(args); len(nf) > 0 && subs[nf[0]] {
		a.f.setExtreme(base + " " + nf[0])
	}
}

func (a *analyzer) ruleContainer(args []word) {
	nf := nonFlags(args)
	if len(nf) == 0 {
		return
	}
	if nf[0] == "system" && len(nf) > 1 && nf[1] == "prune" {
		a.f.setExtreme("container system prune")
		return
	}
	if nf[0] == "rm" && hasShortFlagWith(args, 'f') {
		a.f.setExtreme("container rm -f")
	}
}

func (a *analyzer) ruleSQL(args []word) {
	joined := joinLits(args)
	if sqlBadRx.MatchString(joined) && !sqlWhereRx.MatchString(joined) {
		a.f.setExtreme("SQL mutation without WHERE")
	}
}

func (a *analyzer) ruleCurl(args []word) {
	for i, w := range args {
		l := w.lit
		next := ""
		if i+1 < len(args) && !strings.HasPrefix(args[i+1].lit, "-") {
			next = args[i+1].lit
		}
		switch {
		case l == "-d" || l == "--data" || l == "--data-binary" || l == "--data-ascii" || l == "--data-raw":
			if strings.HasPrefix(next, "@") {
				a.f.setExtreme("curl -d @file")
				return
			}
		case strings.HasPrefix(l, "-d") && len(l) > 2 && l[2] == '@':
			a.f.setExtreme("curl -d @file")
			return
		case strings.HasPrefix(l, "--data"):
			if _, v, ok := strings.Cut(l, "="); ok && strings.HasPrefix(v, "@") {
				a.f.setExtreme("curl -d @file")
				return
			}
		case l == "-T":
			if next != "" || len(l) > 2 {
				a.f.setExtreme("curl upload")
				return
			}
		case strings.HasPrefix(l, "-T") && len(l) > 2:
			a.f.setExtreme("curl upload")
			return
		case l == "--upload-file" || strings.HasPrefix(l, "--upload-file="):
			a.f.setExtreme("curl upload")
			return
		case l == "-F" || strings.HasPrefix(l, "-F"):
			v := next
			if len(l) > 2 {
				v = l[2:]
			}
			if strings.Contains(v, "=@") {
				a.f.setExtreme("curl -F @file")
				return
			}
		}
	}
}

func (a *analyzer) ruleShellC(args []word) {
	for i, w := range args {
		if w.lit == "-c" && i+1 < len(args) {
			a.f.merge(analyzeCommand(args[i+1].lit, a.cwd, a.depth+1))
		}
	}
}

// ---- word helpers ----

// wordOf renders one shell word; partsLit returns literal text plus whether
// the word can expand into something unresolvable.
func (a *analyzer) wordOf(w *syntax.Word) word {
	if w == nil {
		return word{}
	}
	lit, dyn := partsLit(w.Parts)
	return word{lit: lit, dyn: dyn}
}

func partsLit(parts []syntax.WordPart) (string, bool) {
	var b strings.Builder
	dyn := false
	for _, p := range parts {
		switch t := p.(type) {
		case *syntax.Lit:
			b.WriteString(t.Value)
		case *syntax.SglQuoted:
			b.WriteString(t.Value)
		case *syntax.DblQuoted:
			s, d := partsLit(t.Parts)
			b.WriteString(s)
			dyn = dyn || d
		case *syntax.ParamExp:
			dyn = true
			if t.Param != nil {
				b.WriteString("$" + t.Param.Value)
			} else {
				b.WriteString("$")
			}
		case *syntax.CmdSubst, *syntax.ProcSubst, *syntax.ArithmExp, *syntax.ExtGlob, *syntax.BraceExp:
			dyn = true
		}
	}
	return b.String(), dyn
}

func skipFlags(args []word, i int, values map[string]bool) int {
	for i < len(args) {
		l := args[i].lit
		if !strings.HasPrefix(l, "-") || l == "-" || l == "--" {
			break
		}
		i++
		if values[l] && i < len(args) && !strings.HasPrefix(args[i].lit, "-") {
			i++
		}
	}
	return i
}

func skipEnv(args []word, i int) int {
	for i < len(args) {
		l := args[i].lit
		if strings.HasPrefix(l, "-") {
			i++
			if envValueFlags[l] && i < len(args) && !strings.HasPrefix(args[i].lit, "-") {
				i++
			}
			continue
		}
		if !args[i].dyn && strings.Contains(l, "=") {
			i++
			continue
		}
		break
	}
	return i
}

// flags whose value is a separate word, per prefix.
var (
	sudoValueFlags    = map[string]bool{"-u": true, "--user": true, "-g": true, "--group": true, "-p": true, "--prompt": true, "-C": true}
	envValueFlags     = map[string]bool{"-u": true, "--unset": true, "--split-string": true, "-S": true}
	timeoutValueFlags = map[string]bool{"-k": true, "--kill-after": true, "-s": true, "--signal": true}
	niceValueFlags    = map[string]bool{"-n": true, "--adjustment": true}
	stdbufValueFlags  = map[string]bool{"-o": true, "--output": true}
	ioniceValueFlags  = map[string]bool{"-c": true, "-n": true, "-p": true, "--class": true, "--priority": true, "--pid": true}
)

// command vocabularies.
var (
	shellInterpreters = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "ash": true,
		"python": true, "python3": true, "python2": true, "perl": true, "ruby": true, "php": true, "node": true,
	}
	sysPkgSubs = map[string]bool{
		"install": true, "uninstall": true, "remove": true, "purge": true, "erase": true,
		"add": true, "del": true, "autoremove": true, "upgrade": true,
		"dist-upgrade": true, "full-upgrade": true,
	}
	langPkgSubs = map[string]bool{
		"install": true, "uninstall": true, "remove": true, "add": true,
		"update": true, "upgrade": true, "dedupe": true,
	}
)

func dangerousWord(b string) bool {
	switch b {
	case "rm", "dd", "shred", "wipefs", "chmod", "chown", "kill", "mv":
		return true
	}
	return strings.HasPrefix(b, "mkfs")
}

// ---- generic word-slice helpers ----

func nonFlags(ws []word) []string {
	var out []string
	for _, w := range ws {
		if w.dyn || w.lit == "" || strings.HasPrefix(w.lit, "-") {
			continue
		}
		out = append(out, w.lit)
	}
	return out
}

func firstNonFlag(ws []word) string {
	if nf := nonFlags(ws); len(nf) > 0 {
		return nf[0]
	}
	return ""
}

func hasFlagExact(ws []word, f string) bool {
	for _, w := range ws {
		if w.lit == f {
			return true
		}
	}
	return false
}

func hasFlagPrefix(ws []word, p string) bool {
	for _, w := range ws {
		if strings.HasPrefix(w.lit, p) {
			return true
		}
	}
	return false
}

func hasFlagContaining(ws []word, s string) bool {
	for _, w := range ws {
		if strings.HasPrefix(w.lit, "-") && strings.Contains(w.lit, s) {
			return true
		}
	}
	return false
}

// hasShortFlagWith reports a single-dash flag containing c ("-rf", "-D", …).
func hasShortFlagWith(ws []word, c rune) bool {
	for _, w := range ws {
		l := w.lit
		if len(l) > 1 && strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--") {
			for _, r := range l[1:] {
				if r == c {
					return true
				}
			}
		}
	}
	return false
}

func anyTargetPrefix(ws []word, p string) bool {
	for _, w := range ws {
		if !strings.HasPrefix(w.lit, "-") && strings.HasPrefix(w.lit, p) {
			return true
		}
	}
	return false
}

func hasRecForce(ws []word) bool {
	for _, w := range ws {
		l := w.lit
		if l == "--recursive" || l == "--force" {
			return true
		}
		if len(l) > 2 && strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "--") {
			hasR, hasF := false, false
			for _, r := range l[1:] {
				if r == 'r' {
					hasR = true
				}
				if r == 'f' {
					hasF = true
				}
			}
			if hasR || hasF {
				return true
			}
		}
	}
	return false
}

func hasRootish(ws []word, cwd string) bool {
	for _, w := range ws {
		if w.lit == "" || strings.HasPrefix(w.lit, "-") {
			continue
		}
		if isRootish(w.lit, cwd) {
			return true
		}
	}
	return false
}

func joinLits(ws []word) string {
	var out []string
	for _, w := range ws {
		if !w.dyn {
			out = append(out, w.lit)
		}
	}
	return strings.Join(out, " ")
}

func baseName(s string) string {
	s = strings.ReplaceAll(s, `\`, "") // \rm → rm
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// isHelpFlag reports informational flags that make a command a no-op.
func isHelpFlag(l string) bool {
	return l == "-h" || l == "--help" || l == "--version"
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// systemPath reports absolute paths directly under a top-level system dir.
func systemPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	full := resolvePath("/", cutGlob(p))
	for _, d := range []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/lib", "/lib32", "/lib64", "/root"} {
		if full == d || strings.HasPrefix(full, d+"/") {
			return true
		}
	}
	return false
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
