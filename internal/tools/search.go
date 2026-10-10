// ls, grep, find: bounded directory listing and search tools.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/unix/ugh/internal/llm"
)

const (
	maxResults     = 200
	maxListResults = 1000
	excludedNames  = ".git node_modules vendor .venv __pycache__ target dist build .cache .gradle .idea .worktrees"
)

func excluded(name string) bool {
	for _, x := range strings.Fields(excludedNames) {
		if name == x {
			return true
		}
	}
	return false
}

// ---- ls ----

type lsTool struct{}

func (lsTool) Spec() llm.ToolSpec {
	return spec("ls", "List the immediate entries of a directory (default: working directory) with types.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		})
}

type lsArgs struct {
	Path string `json:"path"`
}

func (lsTool) Run(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
	var a lsArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	dir := resolve(cwd, a.Path)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("ls %s: %v", a.Path, err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var b strings.Builder
	n := 0
	for _, e := range ents {
		if n >= maxListResults {
			fmt.Fprintf(&b, "… [%d more entries not shown]\n", len(ents)-n)
			break
		}
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		} else if e.Type()&fs.ModeSymlink != 0 {
			kind = "symlink"
		}
		fmt.Fprintf(&b, "%s %s\n", kind, e.Name())
		n++
	}
	if b.Len() == 0 {
		return fmt.Sprintf("%s: empty directory", a.Path), nil
	}
	return b.String(), nil
}

// ---- grep ----

type grepTool struct{}

func (grepTool) Spec() llm.ToolSpec {
	return spec("grep", "Search project text files recursively with a regular expression (ripgrep when installed: .gitignore, hidden and binary files are skipped; at most 200 matches, 50 per file). Search the narrowest directory you can.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Go regexp"},
				"path":    map[string]any{"type": "string", "description": "directory to search (default: cwd)"},
			},
			"required": []string{"pattern"},
		})
}

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (grepTool) Run(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
	var a grepArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("grep: bad pattern: %v", err)
	}
	root := resolve(cwd, a.Path)
	if out, ok := ripgrep(ctx, root, a.Path, a.Pattern); ok {
		return out, nil
	}
	var b strings.Builder
	count := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil || count >= maxResults {
			if count >= maxResults {
				return fs.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			if p != root && excluded(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		// Regular files only: opening a FIFO or device blocks forever.
		if !d.Type().IsRegular() || !isSearchable(d.Name()) {
			return nil
		}
		data, rerr := limitReadFile(p)
		if rerr != nil {
			return nil
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		for i, ln := range lines {
			if re.MatchString(ln) {
				rel, _ := filepath.Rel(root, p)
				if len(ln) > 300 {
					ln = ln[:300]
				}
				fmt.Fprintf(&b, "%s:%d:%s\n", path.Join(a.Path, filepath.ToSlash(rel)), i+1, ln)
				count++
				if count >= maxResults {
					break
				}
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return "", fmt.Errorf("grep: %v after %d matches; narrow the path or pattern", ctx.Err(), count)
	}
	if err != nil && count == 0 {
		return "", fmt.Errorf("grep: %v", err)
	}
	if b.Len() == 0 {
		return "no matches", nil
	}
	if count >= maxResults {
		fmt.Fprintf(&b, "…[stopped at %d matches]", count)
	}
	return b.String(), nil
}

func isSearchable(name string) bool {
	if excluded(name) {
		return false
	}
	return true
}

func limitReadFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := limitRead(f, 1<<20)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("binary skipped") // signal to caller
	}
	return data, nil
}

// ---- find ----

type findTool struct{}

func (findTool) Spec() llm.ToolSpec {
	return spec("find", "Find files whose path or name matches a glob (e.g. \"*.go\", \"src/**/*.ts\").",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string", "description": "glob pattern"},
				"path":    map[string]any{"type": "string"},
			},
			"required": []string{"pattern"},
		})
}

type findArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (findTool) Run(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
	var a findArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	root := resolve(cwd, a.Path)
	var b strings.Builder
	count := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && excluded(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if matchGlob(a.Pattern, rel) {
			fmt.Fprintln(&b, rel)
			count++
			if count >= maxResults {
				return fs.SkipAll
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return "", fmt.Errorf("find: %v after %d results; narrow the path or pattern", ctx.Err(), count)
	}
	if err != nil && count == 0 {
		return "", fmt.Errorf("find: %v", err)
	}
	if b.Len() == 0 {
		return "no matches", nil
	}
	if count >= maxResults {
		fmt.Fprintf(&b, "…[stopped at %d results]", count)
	}
	return b.String(), nil
}

// matchGlob matches rel against a shell-like glob. Patterns without a
// separator match the base name; "**/" spans directories.
func matchGlob(pattern, rel string) bool {
	pattern = strings.TrimPrefix(pattern, "./")
	if !strings.Contains(pattern, "/") {
		ok, err := filepath.Match(pattern, path.Base(rel))
		return err == nil && ok
	}
	// Translate a leading "**/" into "match any depth" by trying each suffix.
	if strings.HasPrefix(pattern, "**/") {
		tail := strings.TrimPrefix(pattern, "**/")
		parts := strings.Split(rel, "/")
		for i := range parts {
			ok, err := patternMatch(strings.Join(parts[i:], "/"), tail)
			if err == nil && ok {
				return true
			}
		}
		return false
	}
	ok, err := patternMatch(rel, pattern)
	return err == nil && ok
}

func patternMatch(name, pattern string) (bool, error) {
	return filepath.Match(pattern, name)
}

// useRipgrep lets tests exercise the Go walker.
var useRipgrep = true

// ripgrep searches with rg when it is installed: it honours .gitignore,
// skips hidden and binary files, and is far faster on large trees. ok is
// false when rg is unavailable or failed, so the Go walker takes over.
func ripgrep(ctx context.Context, root, shown, pattern string) (string, bool) {
	if !useRipgrep {
		return "", false
	}
	bin, err := exec.LookPath("rg")
	if err != nil {
		return "", false
	}
	args := []string{"--line-number", "--no-heading", "--color", "never", "--max-columns", "300",
		"--max-columns-preview", "--max-filesize", "1M", "--max-count", "50"}
	for _, x := range strings.Fields(excludedNames) {
		args = append(args, "--glob", "!"+x)
	}
	args = append(args, "--regexp", pattern, ".")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", false
	}
	if err := cmd.Start(); err != nil {
		return "", false
	}
	var b strings.Builder
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	count := 0
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "./")
		if shown != "" {
			line = path.Join(shown, line)
		}
		b.WriteString(line + "\n")
		if count++; count >= maxResults {
			break
		}
	}
	_ = cmd.Process.Kill()
	werr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return fmt.Sprintf("grep: %v after %d matches; narrow the path or pattern\n%s", ctx.Err(), count, b.String()), true
	case count >= maxResults:
		fmt.Fprintf(&b, "…[stopped at %d matches]", count)
		return b.String(), true
	case count > 0:
		return b.String(), true
	}
	// rg exits 1 for "no matches" and 2 for errors (e.g. a regex it rejects).
	if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return "no matches", true
	}
	return "", false
}
