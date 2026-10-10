package agent

import (
	"testing"

	"github.com/unix/ugh/internal/config"
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
