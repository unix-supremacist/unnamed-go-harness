package provider

import (
	"testing"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
)

func TestMergeSpecsCustomAndOverride(t *testing.T) {
	out := mergeSpecs(append([]Spec(nil), builtins...), map[string]config.Provider{
		"acme": {BaseURL: "https://api.acme.test/v1", NoThinking: []string{"glm-"}},
	})
	found := false
	for _, s := range out {
		if s.Name == "acme" && s.BaseURL == "https://api.acme.test/v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("custom spec missing")
	}
	if got := auth.EnvVar("acme"); got != "ACME_API_KEY" {
		t.Fatalf("env = %q", got)
	}
}

func TestBuiltinNames(t *testing.T) {
	for _, n := range []string{"opencode-go", "opencode-zen", "openrouter"} {
		if _, ok := Lookup(n); !ok {
			t.Errorf("%s not wired", n)
		}
	}
}

func TestParseConsoleConfig(t *testing.T) {
	body := []byte(`{"providers":{
	 "opencode":{"name":"OpenCode","settings":{"baseURL":"https://opencode.ai/inference/openai/v1"}},
	 "conn_1":{"name":"OpenRouter","settings":{"baseURL":"https://opencode.ai/inference/custom/conn_1/"},"headers":{"x-a":"b"}}}}`)
	got, err := parseConsoleConfig(body, "org_9")
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
	s := got[0]
	if s.Name != "opencode-openrouter" || s.BaseURL != "https://opencode.ai/inference/custom/conn_1" ||
		!s.Console || s.Headers["x-opencode-org-id"] != "org_9" || s.Headers["x-a"] != "b" {
		t.Fatalf("spec = %+v", s)
	}
}
