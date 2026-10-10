package provider

import (
	"errors"
	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/policy"
	"github.com/unix/ugh/internal/provstate"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type rejectAccessProbe struct{ calls int }

func (r *rejectAccessProbe) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("unexpected network access probe")
}

func setupLocalAccess(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "local-test-key")
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	if err := os.WriteFile(path, []byte("[auth]\nstore='file'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provstate.SetPathForTest(filepath.Join(t.TempDir(), "state.json"))
	return path
}

func TestCredentialPresenceDoesNotConfirmRootAccess(t *testing.T) {
	path := setupLocalAccess(t)
	status, reason := ModelAccess("openrouter/xiaomi/mimo-v2.6-pro")
	if status != policy.AccessUnknown || reason == "" {
		t.Fatalf("credential treated as entitlement: %s %s", status, reason)
	}
	os.WriteFile(path, []byte("[auth]\nstore='file'\n[routing.access]\nopenrouter='granted'\n"), 0600)
	status, _ = ModelAccess("openrouter/xiaomi/mimo-v2.6-pro")
	if status != policy.AccessGranted {
		t.Fatalf("explicit override ignored: %s", status)
	}
	provstate.MarkExhausted("openrouter", "", time.Now().Add(time.Hour), "used window", "test")
	status, _ = ModelAccess("openrouter/xiaomi/mimo-v2.6-pro")
	if status != policy.AccessExhausted {
		t.Fatalf("override bypassed known exhaustion: %s", status)
	}
}

func TestRootScopeNeverRefreshesExpiredCodexCredential(t *testing.T) {
	setupLocalAccess(t)
	store, err := auth.Open("file")
	if err != nil {
		t.Fatal(err)
	}
	cred := auth.Credential{Type: "oauth", Access: "expired-access", Refresh: "refresh-material", AccountID: "test-account", Expires: time.Now().Add(-time.Hour).UnixMilli()}
	if err := store.Put("codex", cred); err != nil {
		t.Fatal(err)
	}
	probe := &rejectAccessProbe{}
	original := http.DefaultTransport
	http.DefaultTransport = probe
	defer func() { http.DefaultTransport = original }()
	status, _ := ModelAccess("codex/gpt-6.1-sol")
	if status != policy.AccessUnknown {
		t.Fatalf("unexpected access: %s", status)
	}
	if probe.calls != 0 {
		t.Fatalf("scope evaluation made %d network requests", probe.calls)
	}
	after, _, _ := store.Get("codex")
	if after.Access != cred.Access || after.Refresh != cred.Refresh {
		t.Fatal("scope changed credentials")
	}
}

func TestExplicitScopeEmptyAndUnrestrictedRoundTrip(t *testing.T) {
	setupLocalAccess(t)
	if err := SaveScoped(nil); err != nil {
		t.Fatal(err)
	}
	if got := Scope(); got.State != policy.ScopeStateCustom || len(got.Refs) != 0 {
		t.Fatalf("explicit empty expanded: %+v", got)
	}
	if err := SaveScoped([]string{"*"}); err != nil {
		t.Fatal(err)
	}
	if got := Scope(); got.State != policy.ScopeStateUnrestricted {
		t.Fatalf("wildcard not unrestricted: %+v", got)
	}
	if err := ResetScoped(); err != nil {
		t.Fatal(err)
	}
	if got := Scope(); got.State != policy.ScopeStateAbsent || len(got.Refs) == 0 {
		t.Fatalf("default reset: %+v", got)
	}
	if err := SaveScoped([]string{"*", "openrouter/x"}); err == nil {
		t.Fatal("ambiguous mixed wildcard accepted")
	}
}

func TestResolveRejectsKnownUnsupportedGoRoutesBeforeCredentials(t *testing.T) {
	for _, ref := range []string{"opencode-go/gpt-6-luna", "opencode-go/gemini-3.8-flash", "opencode-go/google/gemini-3.8-flash"} {
		if _, _, err := Resolve(ref); err == nil {
			t.Fatalf("unsupported route accepted: %s", ref)
		}
	}
}
