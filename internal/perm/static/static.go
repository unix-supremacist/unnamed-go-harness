// Package static is the programmatic classifier (PLAN.md §4.4). It parses
// bash with mvdan.cc/sh's syntax package so rules see real argv, walks into
// -c strings, $(...), pipelines and wrapper prefixes, and falls back to token
// heuristics when parsing fails. It never imports os/exec or syscall and can
// therefore never run a command itself (asserted by an import-graph test).
package static

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/unix/ugh/internal/perm"
)

// Classifier names.
const (
	NameNormal  = "static_normal"
	NameExtreme = "static_extreme"
)

// Classifier implements perm.Classifier for one of the two static rules sets.
type Classifier struct{ name string }

// New returns the named static classifier ("static_normal" | "static_extreme").
func New(name string) (*Classifier, error) {
	switch name {
	case NameNormal, NameExtreme:
		return &Classifier{name: name}, nil
	}
	return nil, fmt.Errorf("static: unknown classifier %q", name)
}

func (c *Classifier) Name() string { return c.name }

// Classify judges one call. static_normal flags only ~100%-bad cases;
// static_extreme is a superset of it (§4.4).
func (c *Classifier) Classify(ctx context.Context, call perm.Call) perm.Verdict {
	f := analyze(call)
	v := perm.Verdict{Classifier: c.name}
	if c.name == NameNormal {
		v.Flagged, v.Reason = f.normal != "", f.normal
	} else if f.normal != "" {
		v.Flagged, v.Reason = true, f.normal
	} else {
		v.Flagged, v.Reason = f.extreme != "", f.extreme
	}
	if v.Flagged {
		v.Score = 1
	}
	return v
}

// findings carries the first normal-severity and first extreme-severity
// reasons. The extreme verdict is a superset: it reports normal first.
type findings struct{ normal, extreme string }

func (f *findings) setNormal(r string) {
	if f.normal == "" {
		f.normal = r
	}
}

func (f *findings) setExtreme(r string) {
	if f.extreme == "" {
		f.extreme = r
	}
}

func (f *findings) merge(o findings) {
	if f.normal == "" {
		f.normal = o.normal
	}
	if f.extreme == "" {
		f.extreme = o.extreme
	}
}

// analyze dispatches on the tool. Path-taking tools are judged by target;
// bash by its parsed command; every other tool is not flagged.
func analyze(c perm.Call) findings {
	switch c.Tool {
	case "bash":
		return analyzeCommand(c.Command, c.Cwd, 0)
	case "write", "edit":
		p := stringArg(c.Args, "path")
		if p == "" {
			return findings{}
		}
		return analyzePath(p, c.Cwd)
	}
	return findings{}
}

// ---- path rules ----

// systemDirs are the top-level directories whose contents are never safe to
// delete recursively (§4.4 normal).
var systemDirs = []string{
	"/etc", "/usr", "/bin", "/sbin", "/boot", "/lib", "/lib32", "/lib64",
	"/var", "/home", "/root", "/opt", "/srv", "/run", "/proc", "/sys", "/dev",
}

// protectedWriteDirs are never valid write/edit targets outside the workspace
// (§4.4 normal). /var,/run &co stay out: writing a log there prompts instead.
var protectedWriteDirs = []string{"/etc", "/usr", "/bin", "/sbin", "/boot", "/lib", "/lib32", "/lib64", "/root"}

// sensitive home subdirectories and shell rc files (normal when written
// outside the workspace).
var (
	sensitiveHome = []string{".ssh", ".gnupg"}
	rcFiles       = map[string]bool{
		".bashrc": true, ".bash_profile": true, ".bash_login": true, ".profile": true,
		".zshrc": true, ".zprofile": true, ".zshenv": true, ".inputrc": true,
	}
)

func analyzePath(path, cwd string) findings {
	var f findings
	full := resolvePath(cwd, path)
	if InWorkspace(full, cwd) {
		return f
	}
	if protectedWrite(full) {
		f.setNormal("write to protected path " + full)
	} else {
		f.setExtreme("write outside workspace " + full)
	}
	return f
}

func protectedWrite(full string) bool {
	for _, d := range protectedWriteDirs {
		if full == d || strings.HasPrefix(full, d+"/") {
			return true
		}
	}
	for _, h := range sensitiveHome {
		if strings.Contains(full, "/"+h+"/") || strings.HasSuffix(full, "/"+h) {
			return true
		}
	}
	return rcFiles[filepath.Base(full)]
}

// ---- shared path helpers ----

// resolvePath expands ~ and $HOME, joins relative paths onto cwd and cleans.
func resolvePath(cwd, p string) string {
	home := homeDir()
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case p == "$HOME":
		p = home
	case strings.HasPrefix(p, "$HOME/"):
		p = filepath.Join(home, p[6:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return ""
}

// cutGlob trims a path at its first glob metacharacter so "/*" becomes "/".
func cutGlob(p string) string {
	if i := strings.IndexAny(p, "*?["); i >= 0 {
		return p[:i]
	}
	return p
}

// isRootish reports whether target names "/", $HOME/~/ themselves or a
// top-level system directory (globs cut first).
func isRootish(target, cwd string) bool {
	if target == "" {
		return false
	}
	if strings.HasPrefix(target, "~") {
		rest := strings.TrimPrefix(target, "~")
		return rest == "" || rest == "/" || strings.HasPrefix(rest, "/*")
	}
	if target == "$HOME" || strings.HasPrefix(target, "$HOME/") || strings.HasPrefix(target, "$HOME/*") {
		rest := strings.TrimPrefix(target, "$HOME")
		return rest == "" || rest == "/" || strings.HasPrefix(rest, "/*")
	}
	full := resolvePath(cwd, cutGlob(target))
	if full == "/" {
		return true
	}
	if h := homeDir(); h != "" && full == filepath.Clean(h) {
		return true
	}
	for _, d := range systemDirs {
		if full == d {
			return true
		}
	}
	return false
}

func under(p, dir string) bool {
	if dir == "" {
		return false
	}
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+"/")
}

func stringArg(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil {
		return ""
	}
	return s
}
