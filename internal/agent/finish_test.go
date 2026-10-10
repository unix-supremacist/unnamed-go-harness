package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/admission"
	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/perm/static"
)

func jwt(planType string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct","chatgpt_plan_type":"`+planType+`"}}`) + ".sig"
}

func TestPlanTypeDetection(t *testing.T) {
	if got := auth.CodexPlanType(jwt("Pro")); got != "pro" {
		t.Fatalf("claim = %q", got)
	}
	if auth.CodexPlanType("not-a-jwt") != "" {
		t.Fatal("garbage must yield no plan")
	}
	for plan, want := range map[string]int{"plus": 1, "pro": 10, "prolite": 5, "team": 1, "somethingnew": 1, "": 1} {
		if _, n, _ := admission.PresetForPlanType(plan); n != want {
			t.Errorf("%q → %d, want %d", plan, n, want)
		}
	}
	if _, _, known := admission.PresetForPlanType("somethingnew"); known {
		t.Fatal("unknown plan types must not claim to be known")
	}
	// An explicit setting beats detection; auto with no login stays conservative.
	var cfg config.Config
	cfg.Routing.ChatGPTPlan = "25x"
	if _, n := plannedPreset(cfg); n != 25 {
		t.Fatalf("explicit plan ignored: %d", n)
	}
	routingEnv(t)
	cfg.Routing.ChatGPTPlan = "auto"
	if _, n := plannedPreset(cfg); n != 1 {
		t.Fatalf("auto without a login must be basic, got %d", n)
	}
}

func TestAuthFailureRefreshesOnceThenRetriesSameRoute(t *testing.T) {
	routingEnv(t)
	var refreshed int
	old := refreshAuth
	refreshAuth = func(context.Context, string) bool { refreshed++; return true }
	defer func() { refreshAuth = old }()
	p := &funcProvider{script: func(n int, _ llm.Request) []llm.Event {
		if n == 1 {
			return []llm.Event{{Type: llm.EventError, Err: &llm.HTTPError{Provider: "codex", StatusCode: 401, Body: "expired"}}}
		}
		return []llm.Event{say("ok after refresh")}
	}}
	a := New(Config{Provider: p, Model: "gpt-6.1-sol", ProviderName: "codex", Cwd: t.TempDir()})
	final, err := a.Run(context.Background(), "hi")
	if err != nil || final != "ok after refresh" || refreshed != 1 || p.n != 2 {
		t.Fatalf("final=%q err=%v refreshed=%d requests=%d", final, err, refreshed, p.n)
	}
	// A refresh that does not help fails over instead of looping.
	refreshAuth = func(context.Context, string) bool { return false }
	p2 := &funcProvider{script: func(int, llm.Request) []llm.Event {
		return []llm.Event{{Type: llm.EventError, Err: &llm.HTTPError{Provider: "codex", StatusCode: 401}}}
	}}
	a2 := New(Config{Provider: p2, Model: "gpt-6.1-sol", ProviderName: "codex", Cwd: t.TempDir()})
	if _, err := a2.Run(context.Background(), "hi"); err == nil || !errors.Is(err, err) || p2.n != 1 {
		t.Fatalf("err=%v requests=%d", err, p2.n)
	}
}

func TestWriterGitBoundary(t *testing.T) {
	own := "/work/wt"
	for cmd, want := range map[string]bool{
		"git status":                        false,
		"git add . && git commit -m x":      false,
		"git push origin HEAD":              true,
		"git fetch":                         true,
		"git -C /work/other commit -am x":   true,
		"git -C /work/wt commit -am x":      false,
		"git branch -D main":                true,
		"git worktree add ../x":             true,
		"cd /tmp && git config user.name x": true,
		"git remote add o http://x":         true,
		"git diff | head":                   false,
		"echo hi":                           false,
		"git --git-dir=/work/other/.git gc": true,
	} {
		got := len(static.GitOutsideOwnerViolations(cmd, own)) > 0
		if got != want {
			t.Errorf("%q: violation=%v, want %v (%v)", cmd, got, want, static.GitOutsideOwnerViolations(cmd, own))
		}
	}
}

func TestWritersLiveInPersistentDirByDefault(t *testing.T) {
	routingEnv(t)
	r := NewRuntime()
	dir := r.worktreeDir("root/orchestrator-1/worker-2")
	if strings.HasPrefix(dir, "/tmp/") && !strings.Contains(dir, "ugh/worktrees") {
		t.Fatalf("worktree dir %s", dir)
	}
	if !strings.Contains(dir, "ugh/worktrees/") || strings.Contains(strings.TrimPrefix(dir, "/"), "root/orchestrator") {
		t.Fatalf("worktree dir %s", dir)
	}
	r.Config.Worktree.Dir = "/data/wt"
	if got := r.worktreeDir("a/b"); !strings.HasPrefix(got, "/data/wt/") {
		t.Fatalf("configured dir ignored: %s", got)
	}
}
