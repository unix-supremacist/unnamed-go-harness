package agent

import (
	"context"

	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/perm/model"
	"github.com/unix/ugh/internal/provider"
)

// ContextConfig is everything that shapes what a turn puts in front of the
// model: the visibility ladder, AGENTS.md instructions, big-output digests and
// compaction. The zero value is "all off", so tests and one-shot agents get no
// context machinery by default. Spawn, resume, the TUI and the CLI copy it
// explicitly; nothing reads it from a global.
type ContextConfig struct {
	Ladder         string // "jev" | "off"
	KeepRecent     int
	MinChunkBytes  int
	MinSavings     int
	Instructions   bool
	DigestModel    string
	DigestTools    []string
	DigestMinBytes int

	CompactModel      string // summarizer ref; "" = the conversation model
	CompactThreshold  int    // absolute prompt tokens; 0 = no absolute limit
	CompactThresholds map[string]int

	// AskUser puts an ask_user question in front of the user and waits for
	// the answer (ctx cancellation abandons it). nil means headless: the
	// tool is not offered. Every agent of a tree shares the one UI.
	// Jev answers the ladder's and digest's visibility questions; nil uses
	// the configured decisions endpoint (routing.routers.jev). Tests set a fake.
	Jev model.CostAsker

	AskUser func(ctx context.Context, req AskRequest) (AskAnswer, error)
}

// ContextConfigFrom builds the agent-facing view of the user's configuration.
// A summarizer saved with /compact-model overrides [compaction] model.
func ContextConfigFrom(c config.Config) ContextConfig {
	cc := ContextConfig{
		Ladder: c.Context.Ladder, KeepRecent: c.Context.KeepRecent, MinChunkBytes: c.Context.MinChunkBytes,
		MinSavings: c.Context.MinSavings, Instructions: c.Context.Instructions,
		DigestModel: c.Context.DigestModel, DigestTools: append([]string(nil), c.Context.DigestTools...),
		DigestMinBytes: c.Context.DigestMinBytes,
		CompactModel:   c.Compaction.Model, CompactThreshold: c.Compaction.Threshold,
	}
	if len(c.Compaction.Thresholds) > 0 {
		cc.CompactThresholds = make(map[string]int, len(c.Compaction.Thresholds))
		for k, v := range c.Compaction.Thresholds {
			cc.CompactThresholds[k] = v
		}
	}
	if m := provider.CompactModel(); m != "" {
		cc.CompactModel = m
	}
	return cc
}

func (c ContextConfig) ladderOn() bool { return c.Ladder == "jev" }

// DefaultSystem is the system prompt of the interactive and headless CLI when
// none is given. One definition, shared by the TUI and `ugh -p`.
const DefaultSystem = "You are ugh, a terse coding agent working in one directory. " +
	"Use the provided tools to inspect files, make edits and run commands; prefer " +
	"tools over guessing. When the task is done, reply with a short summary."
