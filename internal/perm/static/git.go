package static

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// GitCommand describes a parsed and classified Git invocation.
// It exposes structured details so Phase 3 worktree management and integration
// operations can inspect the target repository, subcommands, and flags without
// maintaining a separate command classifier.
type GitCommand struct {
	// Binary is the executable path or name (e.g. "git", "/usr/bin/git").
	Binary string

	// Subcommand is the git subcommand (e.g. "status", "branch", "worktree", "config").
	Subcommand string

	// SubArgs are the arguments following the subcommand.
	SubArgs []string

	// TargetDir is the effective working directory after applying all -C flags
	// in order against the initial working directory (cwd).
	TargetDir string

	// CDirs contains the raw path arguments passed to -C.
	CDirs []string

	// GitDir is the path specified by --git-dir, if any.
	GitDir string

	// WorkTree is the path specified by --work-tree, if any.
	WorkTree string

	// Namespace is the namespace specified by --namespace, if any.
	Namespace string

	// ConfigEntries are key-value pairs passed via -c flags.
	ConfigEntries []GitConfigEntry

	// ReadOnly indicates this invocation is a genuine read-only listing or inspection.
	ReadOnly bool

	// Mutating indicates this invocation mutates working files, repository state,
	// refs, worktrees, or configuration.
	Mutating bool

	// MutatesRefs indicates this command alters repository refs (branches, HEAD, tags, reflog).
	MutatesRefs bool

	// WorktreeOp indicates this command modifies worktree layout (add/remove/move/prune/lock/unlock).
	WorktreeOp bool

	// Network indicates this command accesses the network (fetch, push, pull, clone, etc.).
	Network bool

	// Dangerous indicates dangerous execution or redirection options were detected
	// (e.g. --exec-path, --ext-diff, dangerous -c config).
	Dangerous bool

	// DangerousReason is a human-readable explanation of why Dangerous is true.
	DangerousReason string

	// OutputFile is the file target of --output / -o if specified.
	OutputFile string

	// Unknown indicates an unrecognized global option, unknown subcommand, or unclassifiable form.
	Unknown bool

	// ClassificationReason is a short diagnostic summary of why the command was classified this way.
	ClassificationReason string
}

// GitConfigEntry holds a key-value pair from a git -c flag.
type GitConfigEntry struct {
	Key   string
	Value string
}

// IsGitBinary reports whether binaryName is "git", "git.exe", or a path to one of them.
func IsGitBinary(binaryName string) bool {
	s := strings.ReplaceAll(binaryName, `\`, "/")
	b := baseName(s)
	b = strings.TrimSuffix(b, ".exe")
	return b == "git"
}

// ParseGitCommand parses command arguments into a classified GitCommand.
// args may include the git binary as args[0] or start directly with git global options / subcommand.
// cwd is the starting working directory used to resolve relative paths.
func ParseGitCommand(args []string, cwd string) (*GitCommand, bool) {
	if len(args) == 0 {
		return nil, false
	}
	binary := "git"
	rest := args
	if IsGitBinary(args[0]) {
		binary = args[0]
		rest = args[1:]
	}

	cmd := &GitCommand{
		Binary:    binary,
		TargetDir: cwd,
	}

	i := 0
	subcommandFound := false

	for i < len(rest) {
		a := rest[i]
		if a == "--" {
			i++
			if i < len(rest) {
				cmd.Subcommand = rest[i]
				cmd.SubArgs = rest[i+1:]
				subcommandFound = true
			}
			break
		}

		if !strings.HasPrefix(a, "-") {
			cmd.Subcommand = a
			cmd.SubArgs = rest[i+1:]
			subcommandFound = true
			break
		}

		// Handle global options
		switch {
		case a == "-C":
			i++
			if i < len(rest) {
				p := rest[i]
				cmd.CDirs = append(cmd.CDirs, p)
				cmd.TargetDir = resolvePath(cmd.TargetDir, expandHome(p))
			} else {
				cmd.Unknown = true
			}
		case strings.HasPrefix(a, "-C") && len(a) > 2:
			p := a[2:]
			cmd.CDirs = append(cmd.CDirs, p)
			cmd.TargetDir = resolvePath(cmd.TargetDir, expandHome(p))

		case a == "--git-dir":
			i++
			if i < len(rest) {
				cmd.GitDir = resolvePath(cmd.TargetDir, expandHome(rest[i]))
			} else {
				cmd.Unknown = true
			}
		case strings.HasPrefix(a, "--git-dir="):
			cmd.GitDir = resolvePath(cmd.TargetDir, expandHome(strings.TrimPrefix(a, "--git-dir=")))

		case a == "--work-tree":
			i++
			if i < len(rest) {
				cmd.WorkTree = resolvePath(cmd.TargetDir, expandHome(rest[i]))
			} else {
				cmd.Unknown = true
			}
		case strings.HasPrefix(a, "--work-tree="):
			cmd.WorkTree = resolvePath(cmd.TargetDir, expandHome(strings.TrimPrefix(a, "--work-tree=")))

		case a == "--namespace":
			i++
			if i < len(rest) {
				cmd.Namespace = rest[i]
			} else {
				cmd.Unknown = true
			}
		case strings.HasPrefix(a, "--namespace="):
			cmd.Namespace = strings.TrimPrefix(a, "--namespace=")

		case a == "-c":
			i++
			if i < len(rest) {
				cmd.parseConfigEntry(rest[i])
			} else {
				cmd.Unknown = true
			}
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			cmd.parseConfigEntry(a[2:])

		case a == "--config-env":
			i++
			if i < len(rest) {
				cmd.Dangerous = true
				cmd.DangerousReason = "dangerous git option (--config-env)"
			} else {
				cmd.Unknown = true
			}
		case strings.HasPrefix(a, "--config-env="):
			cmd.Dangerous = true
			cmd.DangerousReason = "dangerous git option (--config-env)"

		case a == "--exec-path" || strings.HasPrefix(a, "--exec-path="):
			cmd.Dangerous = true
			cmd.DangerousReason = "dangerous git option (--exec-path)"

		case a == "--bare" || a == "-p" || a == "--paginate" || a == "-P" || a == "--no-pager" ||
			a == "--no-replace-objects" || a == "--literal-pathspecs" ||
			a == "--glob-pathspecs" || a == "--noglob-pathspecs" || a == "--icase-pathspecs" ||
			a == "--opt-submodules":
			// Recognized benign global options

		case a == "--super-prefix":
			i++
		case strings.HasPrefix(a, "--super-prefix="):

		case a == "-v" || a == "--version" || a == "-h" || a == "--help" ||
			a == "--html-path" || a == "--man-path" || a == "--info-path":
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git info/help"

		default:
			cmd.Unknown = true
			cmd.ClassificationReason = "unknown git global option: " + a
		}
		i++
	}

	if !subcommandFound {
		if cmd.ReadOnly {
			return cmd, true
		}
		if len(rest) == 0 {
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git"
			return cmd, true
		}
		cmd.Unknown = true
		return cmd, true
	}

	classifySubcommand(cmd)
	return cmd, true
}

func (cmd *GitCommand) parseConfigEntry(entry string) {
	parts := strings.SplitN(entry, "=", 2)
	key := parts[0]
	val := ""
	if len(parts) > 1 {
		val = parts[1]
	}
	cmd.ConfigEntries = append(cmd.ConfigEntries, GitConfigEntry{Key: key, Value: val})

	if isDangerousGitConfig(key, val) {
		cmd.Dangerous = true
		cmd.DangerousReason = "dangerous git config (" + key + ")"
		return
	}
	if !isSafeGitConfigKey(key) {
		cmd.Unknown = true
	}
}

func isDangerousGitConfig(key, val string) bool {
	k := strings.ToLower(key)
	if strings.Contains(k, "sshcommand") ||
		strings.Contains(k, "fsmonitor") ||
		strings.Contains(k, "external") ||
		strings.HasSuffix(k, ".editor") ||
		strings.HasSuffix(k, ".pager") ||
		strings.HasSuffix(k, ".helper") ||
		strings.HasPrefix(k, "alias.") ||
		strings.HasPrefix(k, "protocol.") {
		return true
	}
	if strings.HasPrefix(val, "!") || strings.ContainsAny(val, ";|&$`") {
		return true
	}
	return false
}

func isSafeGitConfigKey(key string) bool {
	k := strings.ToLower(key)
	prefixes := []string{
		"color.",
		"status.",
		"diff.stat",
		"diff.noprefix",
		"diff.renames",
		"diff.submodule",
		"diff.colormoved",
		"diff.mnemonicprefix",
		"log.",
		"format.pretty",
		"format.numbered",
		"format.signature",
		"core.quotepath",
		"core.autocrlf",
		"core.eol",
		"core.safecrlf",
		"core.ignorecase",
		"core.filemode",
		"core.bare",
		"advice.",
		"i18n.",
		"merge.conflictstyle",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func classifySubcommand(cmd *GitCommand) {
	switch cmd.Subcommand {
	case "branch":
		classifyBranch(cmd)
	case "config":
		classifyConfig(cmd)
	case "remote":
		classifyRemote(cmd)
	case "stash":
		classifyStash(cmd)
	case "worktree":
		classifyWorktree(cmd)
	case "reflog":
		classifyReflog(cmd)
	case "fetch":
		cmd.Network = true
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git fetch"
	case "push", "pull", "clone":
		cmd.Network = true
		cmd.Mutating = true
		if cmd.Subcommand != "clone" {
			cmd.MutatesRefs = true
		}
		cmd.ClassificationReason = "git " + cmd.Subcommand
	case "add", "mv":
		cmd.Mutating = true
		cmd.ClassificationReason = "git " + cmd.Subcommand
	case "commit", "merge", "rebase", "cherry-pick", "revert", "reset", "clean",
		"checkout", "restore", "switch", "tag", "init", "rm":
		cmd.Mutating = true
		if cmd.Subcommand != "clean" && cmd.Subcommand != "init" && cmd.Subcommand != "rm" {
			cmd.MutatesRefs = true
		}
		cmd.ClassificationReason = "git " + cmd.Subcommand
	case "status", "diff", "log", "show", "rev-parse", "ls-files", "ls-tree",
		"blame", "grep", "describe", "shortlog", "cat-file":
		classifyInspection(cmd)
	case "format-patch":
		cmd.Mutating = true
		for i, a := range cmd.SubArgs {
			if (a == "-o" || a == "--output-directory") && i+1 < len(cmd.SubArgs) {
				cmd.OutputFile = cmd.SubArgs[i+1]
			} else if strings.HasPrefix(a, "--output-directory=") {
				cmd.OutputFile = strings.TrimPrefix(a, "--output-directory=")
			}
		}
		cmd.ClassificationReason = "git format-patch"
	case "archive":
		for i, a := range cmd.SubArgs {
			if a == "--remote" || strings.HasPrefix(a, "--remote=") {
				cmd.Network = true
			}
			if strings.HasPrefix(a, "--exec=") || (a == "--exec" && i+1 < len(cmd.SubArgs)) {
				cmd.Dangerous = true
				cmd.DangerousReason = "dangerous git option (archive --exec)"
			}
			if (a == "-o" || a == "--output") && i+1 < len(cmd.SubArgs) {
				cmd.OutputFile = cmd.SubArgs[i+1]
				cmd.Mutating = true
			} else if strings.HasPrefix(a, "--output=") {
				cmd.OutputFile = strings.TrimPrefix(a, "--output=")
				cmd.Mutating = true
			}
		}
		if !cmd.Mutating && !cmd.Dangerous {
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git archive listing"
		}
	case "bundle":
		if len(cmd.SubArgs) > 0 && cmd.SubArgs[0] == "create" {
			cmd.Mutating = true
			if len(cmd.SubArgs) > 1 {
				cmd.OutputFile = cmd.SubArgs[1]
			}
			cmd.ClassificationReason = "git bundle create"
		} else if len(cmd.SubArgs) > 0 && cmd.SubArgs[0] == "verify" {
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git bundle verify"
		} else {
			cmd.Unknown = true
			cmd.ClassificationReason = "git bundle"
		}
	default:
		cmd.Mutating = true
		cmd.Unknown = true
		cmd.ClassificationReason = "unknown git subcommand: " + cmd.Subcommand
	}
}

func classifyBranch(cmd *GitCommand) {
	hasMutateFlag := false
	hasListFlag := false
	hasShowCurrent := false
	unknownFlag := false
	var positional []string

	args := cmd.SubArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-d" || a == "-D" || a == "--delete" ||
			a == "-m" || a == "-M" || a == "--move" ||
			a == "-c" || a == "-C" || a == "--copy" ||
			a == "-u" || strings.HasPrefix(a, "--set-upstream-to") ||
			a == "--unset-upstream" || a == "--edit-description" ||
			a == "-f" || a == "--force":
			hasMutateFlag = true
		case a == "--list" || a == "-l":
			hasListFlag = true
		case a == "--show-current":
			hasShowCurrent = true
		case a == "-a" || a == "--all" ||
			a == "-r" || a == "--remotes" ||
			a == "-v" || a == "-vv" || a == "--verbose" ||
			a == "--color" || a == "--no-color" ||
			a == "--merged" || a == "--no-merged" ||
			a == "--contains" || a == "--no-contains" ||
			a == "--points-at" || strings.HasPrefix(a, "--sort=") ||
			strings.HasPrefix(a, "--format=") || a == "--column" || a == "--no-column" ||
			a == "-q" || a == "--quiet":
			// Recognized listing flags
			if (a == "--contains" || a == "--no-contains" || a == "--merged" || a == "--no-merged" ||
				a == "--points-at" || a == "--sort" || a == "--format" || a == "--column") &&
				i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++ // skip value parameter
			}
		case strings.HasPrefix(a, "-"):
			unknownFlag = true
		default:
			positional = append(positional, a)
		}
	}

	switch {
	case hasMutateFlag:
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git branch mutation"
	case unknownFlag:
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.Unknown = true
		cmd.ClassificationReason = "unknown git branch flag"
	case hasShowCurrent:
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git branch --show-current"
	case hasListFlag:
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git branch --list"
	case len(positional) == 0:
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git branch listing"
	default:
		// Bare positional argument creates or sets branch
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git branch creation"
	}
}

func classifyConfig(cmd *GitCommand) {
	scope := ""
	scopeFile := ""
	action := ""
	var positional []string
	unknownFlag := false

	args := cmd.SubArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--global":
			scope = "global"
		case a == "--system":
			scope = "system"
		case a == "--local":
			scope = "local"
		case a == "--worktree":
			scope = "worktree"
		case a == "-f" || a == "--file":
			scope = "file"
			if i+1 < len(args) {
				i++
				scopeFile = args[i]
			}
		case strings.HasPrefix(a, "--file="):
			scope = "file"
			scopeFile = strings.TrimPrefix(a, "--file=")
		case a == "-l" || a == "--list":
			action = "list"
		case a == "--get":
			action = "get"
		case a == "--get-all":
			action = "get-all"
		case a == "--get-regexp":
			action = "get-regexp"
		case a == "--get-urlmatch":
			action = "get-urlmatch"
		case a == "--add" || a == "--replace-all":
			action = "set"
		case a == "--unset" || a == "--unset-all":
			action = "unset"
		case a == "--rename-section":
			action = "rename-section"
		case a == "--remove-section":
			action = "remove-section"
		case a == "-e" || a == "--edit":
			action = "edit"
		case a == "--show-origin" || a == "--show-scope" || a == "--includes" || a == "--no-includes" ||
			a == "--bool" || a == "--int" || a == "--bool-or-int" || a == "--path" || a == "--expiry-date" ||
			strings.HasPrefix(a, "--type=") || a == "-z" || a == "--null" || a == "--name-only" ||
			a == "-q" || a == "--quiet":
			// Recognized read flags
		case a == "--default":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "--default="):
		case strings.HasPrefix(a, "-"):
			unknownFlag = true
		default:
			// Modern action keywords are recognized only at the first
			// positional argument, never as a legacy key's value.
			if action == "" && len(positional) == 0 && hasAny([]string{a}, "list", "get", "set", "unset", "rename-section", "remove-section", "edit") {
				action = a
			} else {
				positional = append(positional, a)
			}
		}
	}

	// Assign scope target file
	switch scope {
	case "system":
		cmd.GitDir = "/etc/gitconfig"
	case "global":
		cmd.GitDir = filepath.Join(homeDir(), ".gitconfig")
	case "file":
		if scopeFile != "" {
			cmd.GitDir = resolvePath(cmd.TargetDir, expandHome(scopeFile))
		}
	}

	switch {
	case action == "edit":
		cmd.Mutating = true
		cmd.ClassificationReason = "git config edit"
	case action == "set" || action == "unset" || action == "rename-section" || action == "remove-section":
		cmd.Mutating = true
		cmd.ClassificationReason = "git config " + action
	case len(positional) >= 2 && action == "":
		// Setting config: `git config key value`
		cmd.Mutating = true
		cmd.ClassificationReason = "git config set"
	case action == "list" || action == "get" || action == "get-all" || action == "get-regexp" || action == "get-urlmatch":
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git config " + action
	case len(positional) == 1 && action == "" && !unknownFlag:
		// Reading config: `git config key`
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git config get"
	case len(positional) == 0 && action == "" && !unknownFlag:
		// Bare `git config` prints usage
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git config usage"
	default:
		cmd.Mutating = true
		cmd.Unknown = true
		cmd.ClassificationReason = "unknown git config invocation"
	}

	if cmd.Mutating && len(positional) >= 1 {
		key := positional[0]
		val := ""
		if len(positional) >= 2 {
			val = positional[1]
		}
		if isDangerousGitConfig(key, val) {
			cmd.Dangerous = true
			cmd.DangerousReason = "dangerous git config (" + key + ")"
		}
	}
}

func classifyRemote(cmd *GitCommand) {
	var positional []string
	unknownFlag := false

	for _, a := range cmd.SubArgs {
		switch {
		case a == "-v" || a == "--verbose" || a == "-q" || a == "--quiet":
		case strings.HasPrefix(a, "-"):
			unknownFlag = true
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) == 0 && !unknownFlag {
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git remote listing"
		return
	}

	if len(positional) == 0 {
		cmd.Mutating = true
		cmd.Unknown = true
		return
	}

	sub := positional[0]
	switch sub {
	case "get-url":
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git remote get-url"
	case "show":
		if len(positional) > 1 {
			// `git remote show origin` queries remote over network
			cmd.Network = true
			cmd.ClassificationReason = "git remote show"
		} else {
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git remote show"
		}
	case "add", "rename", "remove", "rm", "set-head", "set-branches", "set-url":
		cmd.Mutating = true
		cmd.ClassificationReason = "git remote " + sub
	case "prune":
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git remote prune"
	case "update":
		cmd.Network = true
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git remote update"
	default:
		cmd.Mutating = true
		cmd.Unknown = true
		cmd.ClassificationReason = "unknown git remote subcommand: " + sub
	}
}

func classifyStash(cmd *GitCommand) {
	var positional []string
	for _, a := range cmd.SubArgs {
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
		}
	}

	if len(positional) == 0 {
		// Bare `git stash` creates a stash commit and modifies the working directory
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git stash"
		return
	}

	switch positional[0] {
	case "list":
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git stash list"
	case "show":
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git stash show"
	case "push", "save", "pop", "apply", "drop", "clear", "branch", "create", "store":
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git stash " + positional[0]
	default:
		cmd.Mutating = true
		cmd.Unknown = true
		cmd.ClassificationReason = "unknown git stash subcommand: " + positional[0]
	}
}

func classifyWorktree(cmd *GitCommand) {
	var positional []string
	for _, a := range cmd.SubArgs {
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
		}
	}

	if len(positional) == 0 {
		// Bare `git worktree` prints usage
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git worktree"
		return
	}

	switch positional[0] {
	case "list":
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git worktree list"
	case "add", "remove", "move", "prune", "lock", "unlock", "repair":
		cmd.Mutating = true
		cmd.WorktreeOp = true
		cmd.ClassificationReason = "git worktree " + positional[0]
		if positional[0] == "add" && len(positional) > 1 {
			// Record the worktree path as WorkTree target
			cmd.WorkTree = resolvePath(cmd.TargetDir, expandHome(positional[1]))
		}
	default:
		cmd.Mutating = true
		cmd.WorktreeOp = true
		cmd.Unknown = true
		cmd.ClassificationReason = "unknown git worktree subcommand: " + positional[0]
	}
}

func classifyReflog(cmd *GitCommand) {
	var positional []string
	hasMutatingFlag := false

	for _, a := range cmd.SubArgs {
		switch {
		case a == "--expire" || strings.HasPrefix(a, "--expire=") || a == "--expire-unreachable" ||
			a == "--all" || a == "--updateref" || a == "--rewrite":
			hasMutatingFlag = true
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) == 0 {
		if hasMutatingFlag {
			cmd.Mutating = true
			cmd.MutatesRefs = true
			cmd.ClassificationReason = "git reflog expire"
		} else {
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git reflog listing"
		}
		return
	}

	switch positional[0] {
	case "show", "exists":
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git reflog " + positional[0]
	case "delete", "drop":
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git reflog delete"
	case "expire":
		cmd.Mutating = true
		cmd.MutatesRefs = true
		cmd.ClassificationReason = "git reflog expire"
	default:
		// `git reflog <ref>` is equivalent to `git reflog show <ref>`
		if hasMutatingFlag {
			cmd.Mutating = true
			cmd.MutatesRefs = true
			cmd.ClassificationReason = "git reflog expire"
		} else {
			cmd.ReadOnly = true
			cmd.ClassificationReason = "git reflog show"
		}
	}
}

func classifyInspection(cmd *GitCommand) {
	for i, a := range cmd.SubArgs {
		if a == "--ext-diff" || a == "--textconv" || a == "--filters" ||
			a == "--open-files-in-pager" || strings.HasPrefix(a, "--open-files-in-pager=") ||
			cmd.Subcommand == "grep" && (a == "-O" || strings.HasPrefix(a, "-O")) {
			cmd.Dangerous = true
			cmd.DangerousReason = "git option can run external code: " + a
		}
		if (a == "--output" || a == "-o") && i+1 < len(cmd.SubArgs) {
			cmd.OutputFile = cmd.SubArgs[i+1]
			cmd.Mutating = true
		} else if strings.HasPrefix(a, "--output=") {
			cmd.OutputFile = strings.TrimPrefix(a, "--output=")
			cmd.Mutating = true
		}
	}
	if !cmd.Mutating && !cmd.Dangerous {
		cmd.ReadOnly = true
		cmd.ClassificationReason = "git " + cmd.Subcommand
	}
}

// ClassifyGit determines the triage risk tier and reason for a git command invocation.
func ClassifyGit(args []string, cwd string) (tier, reason string, isGit bool) {
	if len(args) == 0 {
		return "", "", false
	}
	if !IsGitBinary(args[0]) && !strings.HasPrefix(args[0], "-") {
		return "", "", false
	}
	cmd, ok := ParseGitCommand(args, cwd)
	if !ok || cmd == nil {
		return "", "", false
	}
	tier, reason = evaluateGitTier(cmd, cwd)
	return tier, reason, true
}

func evaluateGitTier(cmd *GitCommand, initialCwd string) (tier, reason string) {
	tier = Trivial
	reason = "read-only"

	raise := func(t, r string) {
		if tierRank[t] > tierRank[tier] || (reason == "read-only" && tierRank[t] == tierRank[tier]) {
			tier, reason = t, r
		}
	}

	// 1. Sensitive checks for TargetDir, GitDir, WorkTree
	for _, p := range []string{cmd.TargetDir, cmd.GitDir, cmd.WorkTree} {
		if p != "" && Sensitive(p) {
			raise(High, "sensitive path "+p)
		}
	}

	// 2. Sensitive checks for arguments
	for _, a := range cmd.SubArgs {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if strings.HasPrefix(a, "/") || strings.HasPrefix(a, "~") || strings.HasPrefix(a, "$HOME") {
			full := resolvePath(cmd.TargetDir, expandHome(a))
			if Sensitive(full) {
				raise(High, "sensitive path "+full)
			} else if (cmd.Mutating || cmd.Network) && !InWorkspace(full, initialCwd) {
				raise(High, "write outside workspace "+full)
			}
		}
	}

	// 3. Dangerous exec/output/config
	if cmd.Dangerous {
		r := cmd.DangerousReason
		if r == "" {
			r = "dangerous git option"
		}
		raise(High, r)
	}

	// 4. Output file
	if cmd.OutputFile != "" {
		full := resolvePath(cmd.TargetDir, expandHome(cmd.OutputFile))
		switch {
		case Sensitive(full):
			raise(High, "sensitive path "+full)
		case !InWorkspace(full, initialCwd):
			raise(High, "write outside workspace "+full)
		default:
			raise(Middle, "git "+cmd.Subcommand+" --output")
		}
	}

	// 5. Workspace containment for TargetDir, GitDir, WorkTree
	if cmd.Mutating || cmd.Network {
		for _, p := range []string{cmd.TargetDir, cmd.GitDir, cmd.WorkTree} {
			if p != "" && !InWorkspace(p, initialCwd) {
				raise(High, "write outside workspace "+p)
			}
		}
	} else if cmd.ReadOnly {
		for _, p := range []string{cmd.TargetDir, cmd.GitDir, cmd.WorkTree} {
			if p != "" && !InWorkspace(p, initialCwd) {
				raise(Low, "read outside workspace "+p)
			}
		}
	}

	// 6. Operation tier
	switch {
	case cmd.Network:
		raise(High, "git "+cmd.Subcommand)
	case cmd.Mutating:
		sub := cmd.Subcommand
		if sub == "add" || sub == "mv" {
			raise(Low, "git "+sub)
		} else {
			raise(Middle, "git "+sub)
		}
	case cmd.Unknown:
		raise(Middle, "git "+cmd.Subcommand)
	case cmd.ReadOnly || cmd.Subcommand == "":
		// remains Trivial ("read-only") unless raised above
	default:
		raise(Middle, "git "+cmd.Subcommand)
	}

	return tier, reason
}

// GitOutsideOwnerViolations lists git invocations in a bash command that an
// owned-checkout writer may not perform: shared-ref or remote mutation,
// network access, worktree management, and anything that targets a directory
// outside owned (via -C, --git-dir or --work-tree). Unparseable commands and
// non-literal git invocations are reported too, since they cannot be vouched
// for. Operations inside the owned checkout (add, commit, checkout of its own
// branch, status, diff) are not violations.
func GitOutsideOwnerViolations(cmd, owned string) []string {
	f, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		return []string{"unparseable command"}
	}
	var out []string
	owned = filepath.Clean(owned)
	syntax.Walk(f, func(n syntax.Node) bool {
		c, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		var raw []string
		for _, w := range c.Args {
			lit, _ := partsLit(w.Parts)
			raw = append(raw, lit)
		}
		args := unwrapCommand(raw)
		if len(args) > 0 && (args[0] == "cd" || args[0] == "pushd") {
			// Later git calls would run elsewhere; only stay inside the checkout.
			dir := ""
			if len(args) > 1 {
				dir = args[1]
			}
			if dir == "" || dir == "-" {
				out = append(out, args[0]+" to a non-literal or previous directory")
			} else if t := filepath.Clean(resolvePath(owned, expandHome(dir))); t != owned && !strings.HasPrefix(t, owned+string(filepath.Separator)) {
				out = append(out, args[0]+" "+t+" leaves the owned checkout")
			}
			return true
		}
		if len(args) == 0 || !IsGitBinary(args[0]) {
			return true
		}
		for _, a := range args {
			if a == "" {
				out = append(out, "git with a non-literal argument")
				return true
			}
		}
		g, ok := ParseGitCommand(args, owned)
		if !ok || g == nil {
			out = append(out, "unrecognized git invocation")
			return true
		}
		switch {
		case g.MutatesRefs && g.Network, g.Network:
			out = append(out, "git "+g.Subcommand+" reaches the network")
		case g.WorktreeOp:
			out = append(out, "git "+g.Subcommand+" manages worktrees")
		case (g.MutatesRefs || g.Mutating) && sharedRefSubcommand[g.Subcommand]:
			out = append(out, "git "+g.Subcommand+" changes shared refs")
		}
		for _, t := range []string{g.TargetDir, g.GitDir, g.WorkTree} {
			if t == "" {
				continue
			}
			t = filepath.Clean(t)
			if t != owned && !strings.HasPrefix(t, owned+string(filepath.Separator)) {
				out = append(out, "git targets "+t+" outside the owned checkout")
			}
		}
		return true
	})
	return out
}

// sharedRefSubcommand: mutating subcommands that edit refs shared by every
// worktree of the repository (branch/tag/remote/config/notes/stash lists).
var sharedRefSubcommand = map[string]bool{"branch": true, "tag": true, "remote": true, "config": true, "update-ref": true,
	"symbolic-ref": true, "notes": true, "gc": true, "prune": true, "reflog": true, "replace": true}
