// Package tools implements the built-in tools: read, write, edit, ls, grep,
// find and bash. Salvage-and-trim of old/internal/tools: same iron behaviors
// (bounded output, sane timeouts) without the staging/permission layers.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/llm"
)

// Tool is one built-in tool. Paths are resolved against cwd; output is a
// model-facing string, and errors carry the failure reason.
type Tool interface {
	Spec() llm.ToolSpec
	Run(ctx context.Context, cwd string, raw json.RawMessage) (string, error)
}

// Registry holds the built-in tools by name.
type Registry struct {
	order []string
	m     map[string]Tool
}

// All returns the built-in toolset in a stable order.
func All() []Tool {
	return []Tool{readTool{}, writeTool{}, editTool{}, lsTool{}, grepTool{}, findTool{}, bashTool{}}
}

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{m: map[string]Tool{}}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

func (r *Registry) Add(t Tool) {
	name := t.Spec().Name
	if _, dup := r.m[name]; !dup {
		r.order = append(r.order, name)
	}
	r.m[name] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.m[name]
	return t, ok
}

// Specs returns the model-facing declarations in registration order.
func (r *Registry) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.m[n].Spec())
	}
	return out
}

// ---- helpers shared by the implementations ----

func spec(name, desc string, params map[string]any) llm.ToolSpec {
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return llm.ToolSpec{Name: name, Description: desc, Parameters: params}
}

// decode unmarshals arguments into dst and rejects malformed JSON.
func decode(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

// resolve joins a tool path argument onto cwd. Empty means cwd itself.
// Absolute paths are honored (the agent may name files outside cwd).
func resolve(cwd, p string) string {
	if p == "" {
		return cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// maxOutputBytes caps every tool's model-visible output.
const maxOutputBytes = 24 << 10

// truncate cuts output to the byte cap, preserving pure output when space
// remains for the marker.
func truncate(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	const marker = "\n…[truncated]"
	return s[:maxOutputBytes-len(marker)] + marker
}

// limitRead reads up to n bytes of f.
func limitRead(f io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(f, n))
}

type scratchKey struct{}

// WithScratch carries an agent's private scratch directory to its tools;
// bash exports it as $UGH_SCRATCH (created on first use).
func WithScratch(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, scratchKey{}, dir)
}

// ScratchDir returns the scratch directory in ctx, creating it.
func ScratchDir(ctx context.Context) string {
	dir, _ := ctx.Value(scratchKey{}).(string)
	if dir == "" {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

type blobKey struct{}

// Blob tells a tool where to keep the full text of an output it had to trim:
// <Dir>/<Alias>.txt. Recall says whether the agent offers a recall tool for it.
type Blob struct {
	Dir, Alias string
	Recall     bool
}

// WithBlob carries the blob destination for the call about to run.
func WithBlob(ctx context.Context, b Blob) context.Context {
	return context.WithValue(ctx, blobKey{}, b)
}

// saveBlob writes text to the blob destination in ctx and returns the hint to
// append to the trimmed result. Without a destination it says nothing was kept.
func saveBlob(ctx context.Context, text string) string {
	b, _ := ctx.Value(blobKey{}).(Blob)
	size := fmt.Sprintf("%.0f KiB", float64(len(text))/1024)
	if b.Dir == "" || b.Alias == "" {
		return fmt.Sprintf("[%s of output trimmed; the full text was not kept (no session)]", size)
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return fmt.Sprintf("[%s of output trimmed; saving the full text failed: %v]", size, err)
	}
	if err := os.WriteFile(filepath.Join(b.Dir, b.Alias+".txt"), []byte(text), 0o600); err != nil {
		return fmt.Sprintf("[%s of output trimmed; saving the full text failed: %v]", size, err)
	}
	if b.Recall {
		return fmt.Sprintf("[full output: %s in %s — recall(%q, …) to page]", size, b.Alias, b.Alias)
	}
	return fmt.Sprintf("[full output: %s kept as %s]", size, b.Alias)
}

type progressKey struct{}

// WithProgress carries a callback that long-running tools (bash) call with the
// last few lines of output so far, at most every progressEvery. The callback is
// bound to one call by whoever installs it; tools stay free of any agent import.
func WithProgress(ctx context.Context, fn func(tail string)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

const (
	progressEvery = 200 * time.Millisecond
	progressLines = 5
	progressBytes = 2 << 10
)

// progressTail keeps the last progressLines lines written and reports them,
// throttled.
type progressTail struct {
	mu      sync.Mutex
	fn      func(string)
	lines   []string
	partial string
	last    time.Time
}

func newProgressTail(ctx context.Context) *progressTail {
	fn, _ := ctx.Value(progressKey{}).(func(string))
	if fn == nil {
		return nil
	}
	return &progressTail{fn: fn}
}

func (p *progressTail) write(b []byte) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	text := p.partial + strings.ToValidUTF8(string(b), "")
	parts := strings.Split(text, "\n")
	p.partial = parts[len(parts)-1]
	if len(p.partial) > progressBytes {
		p.partial = p.partial[len(p.partial)-progressBytes:]
	}
	for _, l := range parts[:len(parts)-1] {
		if len(l) > progressBytes/progressLines {
			l = l[:progressBytes/progressLines] + "…"
		}
		p.lines = append(p.lines, l)
	}
	if len(p.lines) > progressLines {
		p.lines = p.lines[len(p.lines)-progressLines:]
	}
	if time.Since(p.last) < progressEvery {
		return
	}
	p.last = time.Now()
	tail := p.lines
	if p.partial != "" {
		tail = append(append([]string(nil), tail...), p.partial)
		if len(tail) > progressLines {
			tail = tail[len(tail)-progressLines:]
		}
	}
	p.fn(strings.Join(tail, "\n"))
}
