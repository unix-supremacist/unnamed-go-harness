package provider

import (
	"strings"
	"testing"
)

func TestParseModelsContextAndSort(t *testing.T) {
	ms, err := parseModels("openrouter", strings.NewReader(
		`{"data":[{"id":"b","context_length":128000},{"id":"a","context_window":32000},{"id":""},{"id":"c"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || ms[0].Ref() != "openrouter/a" || ms[0].Context != 32000 || ms[1].Context != 128000 || ms[2].Context != 0 {
		t.Fatalf("got %+v", ms)
	}
}

func TestScopedRoundTripAndContextCache(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cacheData = nil
	if got := Scoped(); len(got) == 0 || got[0] == "*" { // absent scope: policy defaults, not unrestricted
		t.Fatalf("fresh scope = %v, want the default policy scope", got)
	}
	if err := SaveScoped([]string{"openrouter/a", "codex/gpt-6-sol"}); err != nil {
		t.Fatal(err)
	}
	if got := Scoped(); len(got) != 2 || got[0] != "openrouter/a" {
		t.Fatalf("scope = %v", got)
	}
	if err := SaveScoped(nil); err != nil || len(Scoped()) != 0 {
		t.Fatalf("clear: %v %v", err, Scoped())
	}
	storeModels("openrouter", []Model{{Provider: "openrouter", ID: "a", Context: 1000}})
	if w, ok := ContextWindow("openrouter/a"); !ok || w != 1000 {
		t.Fatalf("ctx = %d %v", w, ok)
	}
	if _, ok := ContextWindow("opencode-go/a"); ok {
		t.Fatal("provider must match")
	}
	if w, ok := ContextWindow("a"); !ok || w != 1000 {
		t.Fatalf("bare ctx = %d %v", w, ok)
	}
}

func TestCompactModelPersists(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if CompactModel() != "" {
		t.Fatal("unexpected default")
	}
	if err := SaveCompactModel("openrouter/x"); err != nil || CompactModel() != "openrouter/x" {
		t.Fatalf("save: %v %q", err, CompactModel())
	}
	if err := SaveCompactModel(""); err != nil || CompactModel() != "" {
		t.Fatalf("clear: %v %q", err, CompactModel())
	}
	if err := SaveCompactModel(""); err != nil {
		t.Fatalf("clearing twice: %v", err)
	}
}
