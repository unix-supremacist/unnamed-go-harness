package provider

import (
	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
)

// Spec describes one Chat Completions provider.
type Spec struct {
	Name       string
	BaseURL    string
	Headers    map[string]string
	Session    bool     // send x-opencode-session
	NoThinking []string // model id prefixes that reject `reasoning`
	Usage      string   // opencode | openrouter | none
	Console    bool     // key is the OpenCode Console login token
}

// builtins are the providers ugh ships with. BYOK connections made in the
// OpenCode Console (e.g. opencode-openrouter) are discovered at runtime; see
// console.go.
var builtins = []Spec{
	{Name: "opencode-go", BaseURL: "https://opencode.ai/zen/go/v1", Session: true, Usage: "opencode"},
	{Name: "opencode-zen", BaseURL: "https://opencode.ai/zen/v1", Session: true, Usage: "none"},
	{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", Usage: "openrouter"},
}

// Specs merges config-defined providers over the built-ins and registers
// their key env vars with auth. Config load errors fall back to built-ins.
func Specs() []Spec {
	out := append([]Spec(nil), builtins...)
	out = append(out, consoleProviders()...)
	cfg, err := config.Load()
	if err != nil {
		return out
	}
	return mergeSpecs(out, cfg.Providers)
}

func mergeSpecs(out []Spec, custom map[string]config.Provider) []Spec {
	for name, p := range custom {
		s := Spec{Name: name, BaseURL: p.BaseURL, Headers: p.Headers, Session: p.SessionHeader,
			NoThinking: p.NoThinking, Usage: p.Usage}
		replaced := false
		for i := range out {
			if out[i].Name == name {
				out[i], replaced = s, true
			}
		}
		if !replaced {
			out = append(out, s)
		}
		auth.RegisterProvider(name, p.KeyEnv)
	}
	return out
}

// Lookup finds a spec by provider name.
func Lookup(name string) (Spec, bool) {
	for _, s := range Specs() {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// Names lists wired provider names (plus codex) for error text.
func Names() []string {
	var n []string
	for _, s := range Specs() {
		n = append(n, s.Name)
	}
	return append(n, "codex")
}
