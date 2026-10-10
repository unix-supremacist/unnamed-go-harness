//go:build linux

// bash: bounded shell execution. Copy-and-trim of the process-group runner
// from old/internal/tools/shell_linux.go. Linux only (the demo target).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/unix/ugh/internal/llm"
)

type bashTool struct{}

func (bashTool) Spec() llm.ToolSpec {
	return spec("bash", "Run a shell command in the working directory with /bin/sh -c. $UGH_SCRATCH is your private "+
		"scratch directory: put temp files, logs and git worktrees there instead of /tmp or the repo root.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":         map[string]any{"type": "string"},
				"timeout_seconds": map[string]any{"type": "integer", "description": "default 120, max 600"},
			},
			"required": []string{"command"},
		})
}

type bashArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

const (
	bashCaptureBytes = 1 << 20
	bashStreamBytes  = 14 << 10
	bashStreamLines  = 450
)

type bashCapture struct {
	buf      bytes.Buffer
	total    int64 // every byte the process wrote
	progress *progressTail
}

func (c *bashCapture) Write(p []byte) (int, error) {
	n := len(p)
	c.progress.write(p)
	c.total += int64(n)
	if room := bashCaptureBytes - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, n)])
	}
	return n, nil
}

// discarded is how many bytes never made it into the capture.
func (c *bashCapture) discarded() int64 { return c.total - int64(c.buf.Len()) }

// trimStream bounds one stream for the model view; salvaged boundShellStream.
func trimStream(text string) string {
	const marker = " …[stream truncated]"
	if lines := strings.Split(text, "\n"); len(lines) > bashStreamLines {
		text = strings.Join(lines[:bashStreamLines], "\n") + marker
	}
	if len(text) > bashStreamBytes {
		text = text[:bashStreamBytes-len(marker)] + marker
	}
	return text
}

func (bashTool) Run(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
	var a bashArgs
	if err := decode(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("bash: command must not be empty")
	}
	secs := a.TimeoutSeconds
	if secs <= 0 {
		secs = 120
	}
	if secs > 600 {
		secs = 600
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)
	cmd.Dir = cwd
	if dir := ScratchDir(ctx); dir != "" {
		cmd.Env = append(os.Environ(), "UGH_SCRATCH="+dir)
	}
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	tail := newProgressTail(ctx)
	out, errs := &bashCapture{progress: tail}, &bashCapture{progress: tail}
	cmd.Stdout, cmd.Stderr = out, errs
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("bash: launch failed: %v", err)
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(start).Round(10 * time.Millisecond)
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	timedOut := waitErr != nil && (runCtx.Err() == context.DeadlineExceeded || ctx.Err() != nil)
	status := "ok"
	switch {
	case timedOut:
		status = "timeout"
	case waitErr != nil:
		status = "exit " + fmt.Sprint(exit)
	}
	stdout, stderr := strings.ToValidUTF8(out.buf.String(), ""), strings.ToValidUTF8(errs.buf.String(), "")
	shownOut, shownErr := trimStream(stdout), trimStream(stderr)
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%.1fs)\nstdout:\n%s", status, float64(elapsed)/1e9, shownOut)
	if strings.TrimSpace(shownErr) != "" {
		fmt.Fprintf(&b, "\nstderr:\n%s", shownErr)
	}
	if waitErr != nil && !timedOut {
		fmt.Fprintf(&b, "\nwait: %v", waitErr)
	}
	lost := out.discarded() + errs.discarded()
	if lost > 0 {
		fmt.Fprintf(&b, "\n[%d bytes of output discarded beyond the %d-byte capture limit]", lost, bashCaptureBytes)
	}
	trimmed := shownOut != stdout || shownErr != stderr
	body := truncate(b.String())
	if trimmed || len(body) < b.Len() {
		hint := saveBlob(ctx, "stdout:\n"+stdout+"\nstderr:\n"+stderr)
		return body + "\n" + hint, nil
	}
	return body, nil

}
