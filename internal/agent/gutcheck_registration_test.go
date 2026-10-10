package agent

import (
	"context"
	"github.com/unix/ugh/internal/tools"
	"testing"
)

func TestGutcheckRegisteredForRootOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := New(Config{Tools: tools.NewRegistry(), Spawn: func(context.Context, *Agent, []Task) string { return "" }})
	if _, ok := root.cfg.Tools.Get("gutcheck"); !ok {
		t.Fatal("root missing gutcheck")
	}
	child := New(Config{Tools: tools.NewRegistry()})
	if _, ok := child.cfg.Tools.Get("gutcheck"); ok {
		t.Fatal("gutcheck added to unprivileged child")
	}
}
