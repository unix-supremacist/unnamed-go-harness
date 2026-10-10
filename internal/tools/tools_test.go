package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// run runs one tool by name against a temp cwd.
func run(t *testing.T, name, cwd string, args string) (string, error) {
	t.Helper()
	tool, ok := NewRegistry(All()...).Get(name)
	if !ok {
		t.Fatalf("no tool %q", name)
	}
	return tool.Run(context.Background(), cwd, json.RawMessage(args))
}

func TestToolSpecsAndRegistry(t *testing.T) {
	r := NewRegistry(All()...)
	want := []string{"read", "write", "edit", "ls", "grep", "find", "bash"}
	specs := r.Specs()
	if len(specs) != len(want) {
		t.Fatalf("specs = %d, want %d", len(specs), len(want))
	}
	for i, s := range specs {
		if s.Name != want[i] {
			t.Errorf("spec[%d] = %q, want %q", i, s.Name, want[i])
		}
		if s.Parameters["type"] != "object" {
			t.Errorf("%s parameters = %+v", s.Name, s.Parameters)
		}
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("unknown tool resolved")
	}
}

func TestWriteReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "write", dir, `{"path":"sub/a.txt","content":"one\ntwo\nthree"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := run(t, "read", dir, `{"path":"sub/a.txt"}`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"three", "1  one", "3  three"} {
		if !strings.Contains(got, want) {
			t.Errorf("read output missing %q:\n%s", want, got)
		}
	}
	// offset/limit window
	got, err = run(t, "read", dir, `{"path":"sub/a.txt","offset":2,"limit":1}`)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if !strings.Contains(got, "2  two") || strings.Contains(got, "three") {
		t.Errorf("windowed read = %q", got)
	}
	// Absolute path is honored.
	abs := filepath.Join(dir, "sub", "a.txt")
	if _, err := run(t, "read", dir, `{"path":`+jsonString(abs)+`}`); err != nil {
		t.Errorf("absolute read: %v", err)
	}
	if _, err := run(t, "read", dir, `{"path":"missing"}`); err == nil {
		t.Error("missing file did not error")
	}
	if _, err := run(t, "read", dir, `{"path":123}`); err == nil {
		t.Error("bad arguments did not error")
	}
}

func TestEditUnique(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "write", dir, `{"path":"e.txt","content":"alpha beta gamma"}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := run(t, "edit", dir, `{"path":"e.txt","old_text":"beta","new_text":"BETA"}`); err != nil {
		t.Fatalf("edit: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "e.txt"))
	if string(data) != "alpha BETA gamma" {
		t.Errorf("content = %q", data)
	}
	if _, err := run(t, "edit", dir, `{"path":"e.txt","old_text":"zzz","new_text":"x"}`); err == nil {
		t.Error("absent old_text did not error")
	}
	// Ambiguous match must not change the file.
	if _, err := run(t, "edit", dir, `{"path":"e.txt","old_text":"a","new_text":"x"}`); err == nil {
		t.Error("ambiguous old_text did not error")
	}
	data, _ = os.ReadFile(filepath.Join(dir, "e.txt"))
	if string(data) != "alpha BETA gamma" {
		t.Errorf("ambiguous edit modified the file: %q", data)
	}
}

func TestLsGrepFind(t *testing.T) {
	dir := t.TempDir()
	must := func(name, args string) string {
		t.Helper()
		out, err := run(t, name, dir, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	must("write", `{"path":"src/main.go","content":"package main\nfunc main() { println(\"hi\") }\n"}`)
	must("write", `{"path":"src/util.go","content":"package main\n"}`)
	must("write", `{"path":"README.md","content":"# hi\n"}`)

	ls := must("ls", `{}`)
	if !strings.Contains(ls, "dir src") || !strings.Contains(ls, "file README.md") {
		t.Errorf("ls = %q", ls)
	}

	grep := must("grep", `{"pattern":"func main"}`)
	if !strings.Contains(grep, "main.go:2") {
		t.Errorf("grep = %q", grep)
	}
	if out := must("grep", `{"pattern":"no-such-symbol"}`); out != "no matches" {
		t.Errorf("grep no match = %q", out)
	}

	find := must("find", `{"pattern":"*.go"}`)
	if !strings.Contains(find, "src/main.go") || strings.Contains(find, "README.md") {
		t.Errorf("find = %q", find)
	}
	if out := must("find", `{"pattern":"**/*.md"}`); !strings.Contains(out, "README.md") {
		t.Errorf("find ** = %q", out)
	}
}

func TestBash(t *testing.T) {
	dir := t.TempDir()
	out, err := run(t, "bash", dir, `{"command":"echo hello"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "ok") {
		t.Errorf("bash output = %q", out)
	}
	// Non-zero exit is reported without an error return (the model sees it).
	out, err = run(t, "bash", dir, `{"command":"echo boom >&2; exit 3"}`)
	if err != nil {
		t.Fatalf("bash exit: %v", err)
	}
	if !strings.Contains(out, "exit 3") || !strings.Contains(out, "boom") {
		t.Errorf("bash exit output = %q", out)
	}
	if _, err := run(t, "bash", dir, `{"command":"  "}`); err == nil {
		t.Error("empty command did not error")
	}
	// The cwd is the working directory.
	if out, err := run(t, "bash", dir, `{"command":"pwd"}`); err != nil || !strings.Contains(out, dir) {
		t.Errorf("bash pwd = %q err=%v", out, err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
	big := strings.Repeat("x", maxOutputBytes+100)
	got := truncate(big)
	if len(got) > maxOutputBytes || !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("truncate(len %d) = %d bytes suffix=%q", len(big), len(got), got[len(got)-12:])
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestBashExportsScratch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch", "root")
	out, err := bashTool{}.Run(WithScratch(context.Background(), dir), t.TempDir(), json.RawMessage(`{"command":"echo $UGH_SCRATCH; test -d $UGH_SCRATCH && echo exists"}`))
	if err != nil || !strings.Contains(out, dir) || !strings.Contains(out, "exists") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	t.Setenv("UGH_SCRATCH", "")
	out, _ = bashTool{}.Run(context.Background(), t.TempDir(), json.RawMessage(`{"command":"echo [$UGH_SCRATCH]"}`))
	if !strings.Contains(out, "[]") {
		t.Fatalf("scratch leaked without one configured: %q", out)
	}
}

func TestGrepSkipsFIFOsAndHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello cmd_map\n"), 0o644)
	if err := syscall.Mkfifo(filepath.Join(dir, "server.pipe"), 0o644); err != nil {
		t.Skip("no mkfifo:", err)
	}
	for _, rg := range []bool{false, true} {
		useRipgrep = rg
		done := make(chan string, 1)
		go func() {
			out, _ := grepTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"cmd_map"}`))
			done <- out
		}()
		select {
		case out := <-done:
			if !strings.Contains(out, "a.txt:1:hello cmd_map") {
				t.Errorf("rg=%v out=%q", rg, out)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("rg=%v: grep hung on a FIFO", rg)
		}
	}
	useRipgrep = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	useRipgrep = false
	_, err := grepTool{}.Run(ctx, dir, json.RawMessage(`{"pattern":"x"}`))
	useRipgrep = true
	if err == nil || !strings.Contains(err.Error(), "narrow the path") {
		t.Errorf("cancelled grep: %v", err)
	}
}

func TestRipgrepHonoursGitignore(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("run/\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "run"), 0o755)
	os.WriteFile(filepath.Join(dir, "run", "latest.log"), []byte("needle\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src.go"), []byte("needle\n"), 0o644)
	out, _ := grepTool{}.Run(context.Background(), dir, json.RawMessage(`{"pattern":"needle"}`))
	if !strings.Contains(out, "src.go:1:needle") || strings.Contains(out, "latest.log") {
		t.Errorf("out = %q", out)
	}
}

func TestBashBlobKeepsTrimmedOutput(t *testing.T) {
	blobs := filepath.Join(t.TempDir(), "s.blobs")
	ctx := WithBlob(context.Background(), Blob{Dir: blobs, Alias: "c7", Recall: true})
	cmd := `{"command":"seq 1 2000; echo DECISIVE-ERROR"}`
	out, err := bashTool{}.Run(ctx, t.TempDir(), json.RawMessage(cmd))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "DECISIVE-ERROR") || !strings.Contains(out, `recall("c7"`) {
		t.Fatalf("expected trimmed output with a recall hint, got tail %q", out[max(0, len(out)-200):])
	}
	full, err := os.ReadFile(filepath.Join(blobs, "c7.txt"))
	if err != nil || !strings.Contains(string(full), "DECISIVE-ERROR") {
		t.Fatalf("blob missing the tail: %v", err)
	}
	// Untrimmed output writes nothing.
	ctx = WithBlob(context.Background(), Blob{Dir: blobs, Alias: "c8"})
	out, _ = bashTool{}.Run(ctx, t.TempDir(), json.RawMessage(`{"command":"echo hi"}`))
	if _, err := os.Stat(filepath.Join(blobs, "c8.txt")); err == nil || strings.Contains(out, "full output") {
		t.Fatalf("small output must not create a blob: %q", out)
	}
}

func TestBashReportsDiscardedBytesHonestly(t *testing.T) {
	blobs := filepath.Join(t.TempDir(), "s.blobs")
	ctx := WithBlob(context.Background(), Blob{Dir: blobs, Alias: "c1"})
	out, err := bashTool{}.Run(ctx, t.TempDir(), json.RawMessage(`{"command":"head -c 2097152 /dev/zero | tr '\\0' x"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("[%d bytes of output discarded", 2097152-bashCaptureBytes)
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q in tail %q", want, out[max(0, len(out)-300):])
	}
	// No destination: say so instead of implying the text exists.
	out, _ = bashTool{}.Run(context.Background(), t.TempDir(), json.RawMessage(`{"command":"seq 1 2000"}`))
	if !strings.Contains(out, "not kept (no session)") {
		t.Fatalf("expected honest no-session note, got tail %q", out[max(0, len(out)-200):])
	}
}

func TestBashProgressTailIsThrottledAndLeavesResultUnchanged(t *testing.T) {
	var mu sync.Mutex
	var tails []string
	ctx := WithProgress(context.Background(), func(tail string) { mu.Lock(); tails = append(tails, tail); mu.Unlock() })
	cmd := `{"command":"for i in 1 2 3 4 5 6 7 8; do echo line$i; done; sleep 0.45; for i in 9 10 11; do echo line$i; done; sleep 0.3; echo last"}`
	with, err := bashTool{}.Run(ctx, t.TempDir(), json.RawMessage(cmd))
	if err != nil {
		t.Fatal(err)
	}
	without, _ := bashTool{}.Run(context.Background(), t.TempDir(), json.RawMessage(cmd))
	norm := func(s string) string { return s[strings.Index(s, "\n"):] } // drop the timing line
	if norm(with) != norm(without) {
		t.Fatalf("progress changed the result:\n%s\n---\n%s", with, without)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tails) < 2 || len(tails) > 8 {
		t.Fatalf("throttled updates = %d: %q", len(tails), tails)
	}
	for _, tl := range tails {
		if n := strings.Count(tl, "\n") + 1; n > 5 {
			t.Fatalf("tail has %d lines: %q", n, tl)
		}
	}
	if last := tails[len(tails)-1]; !strings.Contains(last, "line11") {
		t.Fatalf("last tail = %q", last)
	}
}
