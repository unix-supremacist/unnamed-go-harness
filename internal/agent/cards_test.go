package agent

import (
	"strings"
	"testing"

	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/provider"
)

func TestClampLevel(t *testing.T) {
	glm := []string{"off", "low", "high", "max"}
	for in, want := range map[string]string{"off": "off", "minimal": "low", "low": "low", "medium": "low", "high": "high", "xhigh": "high", "max": "max", "": ""} {
		if got := clampLevel(glm, in); got != want {
			t.Errorf("glm %s → %s, want %s", in, got, want)
		}
	}
	if got := clampLevel([]string{"off"}, "high"); got != "off" {
		t.Errorf("no-knob model → %s", got)
	}
	if got := clampLevel([]string{"off", "high", "max"}, "minimal"); got != "high" {
		t.Errorf("only stronger levels → %s", got)
	}
}

func TestBuiltinCardsCoverDefaults(t *testing.T) {
	for _, id := range roleIDs {
		if c, err := readCard("roles", id); err != nil || c.Prompt == "" || c.Body == "" || len(c.Tools) == 0 {
			t.Errorf("role %s = %+v %v", id, c, err)
		}
	}
	d := config.Default()
	ids := append([]string{d.Council.Aggregator, d.Council.Single, d.Council.Oracle}, d.Council.Panel...)
	for _, id := range d.Routing.Tiers {
		ids = append(ids, id)
	}
	for _, id := range append(ids, modelIDs...) {
		if c, err := readCard("models", id); err != nil || len(usableRoutes(c.Routes)) == 0 || c.Context == 0 {
			t.Errorf("model %s = %+v %v", id, c, err)
		}
	}
}

// setCard replaces a built-in card for one test.
func setCard(t *testing.T, kind, id string, c card) {
	t.Helper()
	table := modelCardTable
	if kind == "roles" {
		table = roleCardTable
	}
	old, had := table[id]
	table[id] = c
	t.Cleanup(func() {
		if had {
			table[id] = old
		} else {
			delete(table, id)
		}
	})
}

func TestModelThinkingPrefersProviderListing(t *testing.T) {
	provider.SetModelsForTest(t, "openrouter", []provider.Model{
		{Provider: "openrouter", ID: "z-ai/glm-5.3-flash", Thinking: []string{"max", "high", "low"}, ThinkingRequired: true},
		{Provider: "openrouter", ID: "openai/gpt-6-luna", Thinking: []string{"high", "minimal"}},
	})
	if got, known := ModelThinking("openrouter/z-ai/glm-5.3-flash"); !known || strings.Join(got, ",") != "low,high,max" {
		t.Errorf("mandatory reasoning = %v %v (no off)", got, known)
	}
	if got, _ := ModelThinking("openrouter/openai/gpt-6-luna"); strings.Join(got, ",") != "off,minimal,high" {
		t.Errorf("optional reasoning = %v", got)
	}
	if got, known := ModelThinking("codex/gpt-6-luna"); !known || got[0] != "off" || len(got) != 7 {
		t.Errorf("card fallback = %v %v", got, known)
	}
	if _, known := ModelThinking("openrouter/xiaomi/mimo-v2.6-pro"); known {
		t.Error("unreported levels must stay unknown")
	}
	if got := ClampThinking("openrouter/z-ai/glm-5.3-flash", "off"); got != "low" {
		t.Errorf("off on mandatory reasoning → %s", got)
	}
}
