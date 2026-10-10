package provstate

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuotaErrorParsesResets(t *testing.T) {
	err := errors.New(`codex: HTTP 429: {"error":{"type":"usage_limit_reached","message":"limit","resets_in_seconds":12524}}`)
	until, reason, ok := QuotaError(err)
	if !ok || reason != "usage_limit_reached" || until.Sub(time.Now()) < 12500*time.Second {
		t.Fatalf("until=%v reason=%q ok=%v", until, reason, ok)
	}
	if _, _, ok := QuotaError(errors.New("chat: HTTP 429: rate limited, slow down")); ok {
		t.Error("a plain rate limit is not a quota exhaustion")
	}
	if _, _, ok := QuotaError(errors.New("chat: HTTP 402: insufficient credits")); !ok {
		t.Error("402")
	}
}

func TestMarkUsableAndExpiry(t *testing.T) {
	SetPathForTest(filepath.Join(t.TempDir(), "providers.json"))
	if !Usable("codex") {
		t.Fatal("fresh state should be usable")
	}
	Mark("codex", State{Kind: "exhausted", Until: time.Now().Add(time.Hour), Reason: "usage_limit_reached"})
	if Usable("codex") || !Exhausted("codex") || !Usable("opencode-go") {
		t.Fatal("codex should be exhausted only")
	}
	// A reserve mark never downgrades an exhaustion.
	Mark("codex", State{Kind: "reserved", Until: time.Now().Add(time.Hour), Reason: "usage 95%"})
	if !Exhausted("codex") || !strings.Contains(Summary(), "codex exhausted until") {
		t.Fatalf("summary = %q", Summary())
	}
	Mark("openrouter", State{Kind: "reserved", Until: time.Now().Add(-time.Second), Reason: "old"})
	if !Usable("openrouter") {
		t.Error("expired marks must not apply")
	}
}

func TestRouteCheckAndInvalidation(t *testing.T) {
	SetPathForTest(filepath.Join(t.TempDir(), "providers.json"))

	// 1. Model rejection only affects that model
	MarkModelRejected("openrouter", "bad-model", time.Now().Add(time.Hour), "404 not found", "api_error")
	st, blocked := CheckRoute("openrouter", "bad-model", "chat", "acct1")
	if !blocked || st.Kind != "rejected" {
		t.Fatalf("expected bad-model to be rejected: %+v %v", st, blocked)
	}
	_, blockedOther := CheckRoute("openrouter", "good-model", "chat", "acct1")
	if blockedOther {
		t.Fatal("good-model should not be blocked")
	}

	// 2. Account-scoped exhaustion
	MarkExhausted("codex", "acct1", time.Now().Add(time.Hour), "quota exceeded", "api_error")
	st, blockedAcct1 := CheckRoute("codex", "gpt-6.1-sol", "responses", "acct1")
	if !blockedAcct1 || st.Kind != "exhausted" {
		t.Fatalf("acct1 codex should be exhausted: %+v %v", st, blockedAcct1)
	}
	_, blockedAcct2 := CheckRoute("codex", "gpt-6.1-sol", "responses", "acct2")
	if blockedAcct2 {
		t.Fatal("acct2 codex should not be blocked by acct1 exhaustion")
	}

	// 3. InvalidateAccount clears acct1
	InvalidateAccount("acct1")
	_, blockedAfterInvalidate := CheckRoute("codex", "gpt-6.1-sol", "responses", "acct1")
	if blockedAfterInvalidate {
		t.Fatal("acct1 codex should be unblocked after account invalidation")
	}
	// Model rejection remains unaffected
	_, blockedModelStill := CheckRoute("openrouter", "bad-model", "chat", "acct1")
	if !blockedModelStill {
		t.Fatal("bad-model should still be rejected")
	}

	// 4. Outage affects all models on provider
	MarkOutage("opencode-go", time.Now().Add(time.Hour), "503 gateway", "api_error")
	st, blockedOutage := CheckRoute("opencode-go", "mimo-v2.6-flash", "chat", "")
	if !blockedOutage || st.Kind != "outage" {
		t.Fatalf("opencode-go should be in outage: %+v %v", st, blockedOutage)
	}
}
