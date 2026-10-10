package auth

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBackend is the in-memory backend used by tests; nothing here touches a
// keyring.
type fakeBackend struct {
	name  string
	doc   *Credentials
	saves int
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Load() (*Credentials, error) {
	if f.doc == nil {
		return newCredentials(), nil
	}
	return f.doc, nil
}
func (f *fakeBackend) Save(c *Credentials) error {
	f.doc = c
	f.saves++
	return nil
}

func TestStorePutGetRemove(t *testing.T) {
	b := &fakeBackend{name: "fake"}
	s := NewStore(b)

	if _, ok, err := s.Get("openrouter"); err != nil || ok {
		t.Fatalf("empty get: ok=%v err=%v", ok, err)
	}
	want := Credential{Type: "api_key", Key: "sk-or-v1-abcdef0123456789"}
	if err := s.Put("openrouter", want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := s.Get("openrouter")
	if err != nil || !ok || got != want {
		t.Fatalf("get: got=%+v ok=%v err=%v", got, ok, err)
	}
	// Invalid keys are rejected without echoing the value.
	if err := s.Put("openrouter", Credential{Type: "api_key", Key: "bad\nkey"}); err == nil {
		t.Error("control-byte key accepted")
	}
	if err := s.Put("openrouter", Credential{Type: "api_key"}); err == nil {
		t.Error("empty key accepted")
	}
	if err := s.Put("openrouter", Credential{Type: "mystery", Key: "x"}); err == nil {
		t.Error("unknown credential type accepted")
	}

	if err := s.Remove("openrouter"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok, _ := s.Get("openrouter"); ok {
		t.Error("credential survived remove")
	}
	if err := s.Remove("openrouter"); err == nil {
		t.Error("removing absent credential succeeded")
	}
	// Only the successful Put and the Remove persist.
	if b.saves != 2 {
		t.Errorf("saves = %d, want 2", b.saves)
	}
}

func TestResolveEnvPrecedence(t *testing.T) {
	s := NewStore(&fakeBackend{name: "fake", doc: &Credentials{
		Version: 1,
		Providers: map[string]Credential{
			"opencode-go": {Type: "api_key", Key: "store-key-0123456789"},
			"codex":       {Type: "oauth", Access: "tok", Refresh: "ref"},
		},
	}})
	lookup := func(k string) (string, bool) {
		if k == "OPENCODE_API_KEY" {
			return "env-key-9876543210", true
		}
		return "", false
	}

	// Env wins at runtime, without touching the store.
	v, err := Resolve("opencode-go", lookup, s)
	if err != nil || v != "env-key-9876543210" {
		t.Fatalf("env precedence: v=%q err=%v", v, err)
	}
	// No env → store value.
	v, err = Resolve("opencode-go", func(string) (string, bool) { return "", false }, s)
	if err != nil || v != "store-key-0123456789" {
		t.Fatalf("store fallback: v=%q err=%v", v, err)
	}
	// Unknown provider: actionable error, no secret material.
	if _, err := Resolve("openrouter", func(string) (string, bool) { return "", false }, s); err == nil {
		t.Fatal("missing credential resolved")
	}
	// OAuth credentials are not api keys.
	if _, err := Resolve("codex", func(string) (string, bool) { return "", false }, s); err == nil {
		t.Fatal("oauth credential resolved as api key")
	}
}

func TestMaskKey(t *testing.T) {
	key := "sk-or-v1-abcdefghijklmnop"
	m := MaskKey(key)
	if strings.Contains(m, "fghijklmno") {
		t.Errorf("mask leaks key interior: %q", m)
	}
	if !strings.HasSuffix(m, "mnop") || !strings.Contains(m, "…") {
		t.Errorf("mask %q should end with last 4 and contain …", m)
	}
	if MaskKey("") != "" {
		t.Error("empty key should mask to empty")
	}
	if s := MaskKey("short"); strings.Contains(s, "hort") && !strings.HasSuffix(s, "hort") {
		t.Errorf("short mask %q leaks", s)
	}
}

func TestOpenBackendSelection(t *testing.T) {
	old := probeKeyring
	defer func() { probeKeyring = old }()
	path := filepath.Join(t.TempDir(), AppDir, FileName)

	probeKeyring = func() error { return errors.New("no secret service") }
	s, err := open("auto", path)
	if err != nil || s.Backend().Name() != "file" {
		t.Fatalf("auto fallback: backend=%q err=%v", s.Backend().Name(), err)
	}
	if _, err := open("keyring", path); err == nil {
		t.Error("keyring backend opened while unavailable")
	}

	probeKeyring = func() error { return nil }
	s, err = open("auto", path)
	if err != nil || s.Backend().Name() != "keyring" {
		t.Fatalf("auto keyring: backend=%q err=%v", s.Backend().Name(), err)
	}
	if _, err := open("bogus", path); err == nil {
		t.Error("unknown backend mode accepted")
	}
}
