// Package config loads ugh's TOML configuration (~/.config/ugh/config.toml)
// over built-in defaults. A missing file means pure defaults. Later
// milestones extend this by adding fields to Config/defaults; keys absent
// from the file keep their default values.
package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/unix/ugh/internal/perm"
)

// Config mirrors the sections of config.toml.
type Config struct {
	Auth        Auth                `toml:"auth"`
	Permissions Permissions         `toml:"permissions"`
	Models      Models              `toml:"models"`
	Routing     Routing             `toml:"routing"`
	Council     Council             `toml:"council"`
	Worktree    Worktree            `toml:"worktree"`
	UI          UI                  `toml:"ui"`
	Pricing     map[string]Price    `toml:"pricing"`
	Agent       Agent               `toml:"agent"`
	Context     Context             `toml:"context"`
	Compaction  Compaction          `toml:"compaction"`
	Tools       Tools               `toml:"tools"`
	Limits      Limits              `toml:"limits"`
	Providers   map[string]Provider `toml:"providers"`
	Flex        Flex                `toml:"flex"`
}

// Agent holds per-agent budgets ([agent]) for long recursive runs.
type Agent struct {
	MaxTurnsRoot   int            `toml:"max_turns_root"`
	MaxTurns       int            `toml:"max_turns"` // children without a role override
	MaxTurnsByRole map[string]int `toml:"max_turns_by_role"`
	// MaxTurnsByDepth raises budgets by tree depth ("1" = lane leads, which
	// orchestrate with many cheap turns); a child gets the larger of its
	// role and depth budgets.
	MaxTurnsByDepth map[string]int `toml:"max_turns_by_depth"`
	// MaxTurnsCap bounds a per-task max_turns requested through spawn.
	MaxTurnsCap int `toml:"max_turns_cap"`
	// AutoCompact compacts an agent's older turns once its prompt passes
	// this fraction of the model card's context (0 disables).
	AutoCompact float64 `toml:"auto_compact"`
	// ChildTimeout is a wall-clock budget per child ("45m"; "0" disables).
	// Past it, the child gets the same graceful turn-out as a turn budget.
	ChildTimeout string `toml:"child_timeout"`
}

// ChildTimeoutDuration parses ChildTimeout; 0 means no limit.
func (a Agent) ChildTimeoutDuration() time.Duration {
	if a.ChildTimeout == "" || a.ChildTimeout == "0" {
		return 0
	}
	d, _ := time.ParseDuration(a.ChildTimeout)
	return d
}

// TurnsFor returns the turn budget for a child with the given role.
func (a Agent) TurnsFor(role string) int {
	if n, ok := a.MaxTurnsByRole[role]; ok && n > 0 {
		return n
	}
	return a.MaxTurns
}

// ChildTurns is a spawned child's budget: an explicit per-task request
// (capped at MaxTurnsCap), else the larger of its role and depth budgets.
func (a Agent) ChildTurns(role string, depth, requested int) int {
	if requested > 0 {
		return min(requested, a.MaxTurnsCap)
	}
	n := a.TurnsFor(role)
	if d := a.MaxTurnsByDepth[strconv.Itoa(depth)]; d > n {
		n = d
	}
	return n
}

// Context configures what each turn puts in front of the model ([context]).
type Context struct {
	// Ladder is "jev" (default: a Jev call decides per request which old tool
	// results stay full, shrink to a short view, or hide) or "off".
	Ladder string `toml:"ladder"`
	// KeepRecent messages at the tail are never touched by the ladder.
	KeepRecent int `toml:"keep_recent"`
	// MinChunkBytes is the smallest tool result the ladder considers.
	MinChunkBytes int `toml:"min_chunk_bytes"`
	// MinSavings skips the Jev call when the eligible chunks could not save
	// at least this many bytes.
	MinSavings int `toml:"min_savings"`
	// Instructions loads AGENTS.md (root and per-directory) into the context.
	Instructions bool `toml:"instructions"`
	// DigestModel, when set, lets big tool output be summarized by this
	// model before it enters the context ("" disables digests).
	DigestModel    string   `toml:"digest_model"`
	DigestTools    []string `toml:"digest_tools"`
	DigestMinBytes int      `toml:"digest_min_bytes"`
}

// Compaction configures when and by which model old turns are summarized
// ([compaction]).
type Compaction struct {
	// Model summarizes; "" uses the conversation model.
	Model string `toml:"model"`
	// Threshold is the absolute prompt size (tokens) that triggers
	// compaction; 0 means no absolute limit.
	Threshold int `toml:"threshold"`
	// Thresholds overrides Threshold per active conversation model ref.
	Thresholds map[string]int `toml:"thresholds"`
}

// Tools configures built-in tools ([tools]).
type Tools struct {
	// TimeoutS bounds read/write/edit/ls/grep/find calls; bash keeps its
	// own per-call timeout (at most 600 s).
	TimeoutS int `toml:"timeout_s"`
}

// Limits holds concurrency caps ([limits]).
type Limits struct {
	// Streams caps concurrent model streams per provider; providers not
	// listed use routing.provider_concurrency.
	Streams map[string]int `toml:"streams"`
	// MaxLive caps agents running at once across the whole tree; extra
	// children wait as "queued". Parents waiting on spawn do not count.
	MaxLive int `toml:"max_live"`
	// Reserve (percent) keeps new spawns off a provider once any of its
	// usage windows reaches it; agents already on it continue.
	Reserve map[string]float64 `toml:"reserve"`
}

// Flex is OpenRouter flex-tier routing ([flex]): matching models try the
// cheaper, best-effort flex endpoints first, and a request that errors or
// streams nothing within FirstTokenS is resent on normal routing.
type Flex struct {
	Enabled bool `toml:"enabled"`
	// Root also uses flex for the root agent; off by default, since a
	// human waits on those turns.
	Root bool `toml:"root"`
	// Models are OpenRouter model id prefixes to try flex on.
	Models []string `toml:"models"`
	// Endpoints are OpenRouter provider tags tried in order before normal
	// routing (e.g. "google-ai-studio/flex").
	Endpoints   []string `toml:"endpoints"`
	FirstTokenS int      `toml:"first_token_s"`
}

// StreamsFor returns the stream cap for one provider.
func (c Config) StreamsFor(provider string) int {
	if n := c.Limits.Streams[provider]; n > 0 {
		return n
	}
	return c.Routing.ProviderConcurrency
}

// UI holds interface settings ([ui]): the theme, dark|light|system.
type UI struct {
	Theme string `toml:"theme"`
	// AskTimeout bounds how long an agent's ask_user question waits for an
	// answer ("5m"); "" or "0" waits as long as the user takes.
	AskTimeout string `toml:"ask_timeout"`
}

// Price is one model's honest list price in USD per million tokens ([pricing],
// keyed by provider/model ref or bare model id). Leave a field out when the
// provider does not sell that part separately (e.g. cached tokens); models
// bought through a subscription are never priced here, they display as
// "included". A missing entry simply means the footer shows "cost n/a".
type Price struct {
	In     float64 `toml:"in"`
	Out    float64 `toml:"out"`
	Cached float64 `toml:"cached"`
	// Included marks a subscription-backed model: the footer shows
	// "included" instead of computing a cost.
	Included bool `toml:"included"`
}

type RouterConfig struct {
	BaseURL       string `toml:"base_url"`
	Path          string `toml:"path"`
	Model         string `toml:"model"`
	KeyEnv        string `toml:"key_env"`
	SessionHeader string `toml:"session_header"`
	TimeoutMS     int    `toml:"timeout_ms"`
}

type Routing struct {
	PickModel           string                  `toml:"pick_model"`
	PreferredRouter     string                  `toml:"preferred_router"` // jev | clef (default: jev)
	FallbackRouter      string                  `toml:"fallback_router"`  // clef | jev (default: clef)
	Routers             map[string]RouterConfig `toml:"routers"`          // routing-specific decision clients
	Tiers               map[string]string       `toml:"tiers"`            // first family per tier: low, high, oracle
	Fallback            map[string]string       `toml:"fallback"`         // family → next family in its tier chain ("" ends it)
	Access              map[string]string       `toml:"access"`           // explicit access overrides per provider or model ref
	ChatGPTPlan         string                  `toml:"chatgpt_plan"`     // auto (detect from the Codex login) | basic | 5x | 10x | 25x
	PolicyVersion       int                     `toml:"policy_version"`   // policy/migration version marker
	ProviderConcurrency int                     `toml:"provider_concurrency"`
	// TrustCredentials is the explicit opt-in that lets automatic delegation
	// use routes whose entitlement is unconfirmed (a credential is present).
	TrustCredentials bool `toml:"trust_credentials"`
	// TraditionalCooldownS bounds how long model-selectable delegation stays
	// exposed after both routers fail, before managed routing is retried.
	TraditionalCooldownS int `toml:"traditional_cooldown_s"`
}

// Worktree configures managed feature-writer worktrees ([worktree]).
type Worktree struct {
	// Dir holds them; default is a persistent dir under the ugh state home
	// (not the temp dir, which may be tmpfs and lose unfinished work).
	Dir string `toml:"dir"`
}

// Council configures the §6 council tool and `ugh eval council`.
type Council struct {
	Aggregator     string   `toml:"aggregator"`
	Panel          []string `toml:"panel"`
	DefaultAngles  int      `toml:"default_angles"`
	MaxSeats       int      `toml:"max_seats"`
	SeatTimeoutS   int      `toml:"seat_timeout_s"`
	MaxReportWords int      `toml:"max_report_words"`
	Single         string   `toml:"single"`
	Oracle         string   `toml:"oracle"`
	// Bounds on one council, checked before any spend: reports given to the
	// synthesizer, estimated total work, and how many seats run at once.
	MaxContextWords  int `toml:"max_context_words"`
	MaxWorkUnits     int `toml:"max_work_units"`
	MaxParallelSeats int `toml:"max_parallel_seats"`
	// QueueDeadlineS bounds how long a seat or coordinator may wait for
	// admission; SeatTimeoutS then bounds execution after admission.
	QueueDeadlineS int `toml:"queue_deadline_s"`
	// ExemptSolLimit is the simultaneous council-coordinator Sol requests per account.
	ExemptSolLimit int `toml:"exempt_sol_limit"`
}

// Auth configures credential storage (§3.1 of PLAN.md).
type Auth struct {
	// Store picks the credential backend: auto | keyring | file.
	Store string `toml:"store"`
}

// Models holds model defaults; refs are "provider/model" (§2).
type Models struct {
	// Default is the model ref used when -m is not given.
	Default string `toml:"default"`
}

// Permissions is the [permissions] section (§4.2): mode, aggregate rule and
// per-classifier settings, decoded straight into perm.Policy.
type Permissions struct {
	Mode            string                           `toml:"mode"` // manual | classify | auto-safe | auto
	Aggregate       string                           `toml:"aggregate"`
	AggregateAction string                           `toml:"aggregate_action"`
	HeadlessPrompt  string                           `toml:"headless_prompt"`
	Classifiers     map[string]perm.ClassifierConfig `toml:"classifiers"`
	// Tiers: model classifiers per triage tier (classify/manual mode).
	Tiers map[string][]string `toml:"tiers"`
	// WorkspaceRoots count as inside the workspace for triage and the
	// static rules, besides each agent's cwd, its git root, the temp dir and
	// the agents' scratch dirs (e.g. a multi-repo workspace root).
	WorkspaceRoots []string `toml:"workspace_roots"`
}

// Policy returns the permission policy these settings describe.
func (p Permissions) Policy() perm.Policy {
	return perm.Policy{
		Mode: p.Mode, Aggregate: p.Aggregate, AggregateAction: p.AggregateAction,
		HeadlessPrompt: p.HeadlessPrompt, Classifiers: p.Classifiers, Tiers: p.Tiers,
	}
}

// Default returns the built-in configuration.
func Default() Config {
	cfg := Config{
		Auth: Auth{Store: "auto"},
		Routing: Routing{
			PickModel:       "jev",
			PreferredRouter: "jev",
			FallbackRouter:  "clef",
			PolicyVersion:   1,
			ChatGPTPlan:     "auto",
			Routers: map[string]RouterConfig{
				"jev": {
					BaseURL:   "https://openrouter.ai/api/alpha",
					Path:      "/decisions",
					Model:     "typesafe/jev-1.13",
					KeyEnv:    "OPENROUTER_API_KEY",
					TimeoutMS: 3000,
				},
				"clef": {
					BaseURL:   "https://openrouter.ai/api/alpha",
					Path:      "/decisions",
					Model:     "cloudflare/clef",
					KeyEnv:    "OPENROUTER_API_KEY",
					TimeoutMS: 3000,
				},
			},
			// Each tier's chain starts at its Tiers entry and follows Fallback
			// links: low MiMo Flash → Luna → Haiku → DeepSeek Flash, high Sol →
			// MiMo Pro → Sonnet, oracle Astra → Fable.
			Tiers: map[string]string{
				"low":    "mimo-v2.6-flash",
				"high":   "gpt-sol",
				"oracle": "gpt-astra",
			},
			ProviderConcurrency: 4,
		},
	}
	cfg.Council = Council{
		Aggregator: "mimo-v2.6-pro", Panel: []string{"glm-5.3-flash", "deepseek-v4.1-flash", "mimo-v2.6-flash", "qwen3.8-flash", "gpt-luna"},
		DefaultAngles: 2, MaxSeats: 40, SeatTimeoutS: 240, MaxReportWords: 800,
		MaxContextWords: 24000, MaxWorkUnits: 400, MaxParallelSeats: 6, QueueDeadlineS: 600, ExemptSolLimit: 1,
		Single: "mimo-v2.6-pro", Oracle: "gpt-sol",
	}
	cfg.Permissions = defaultPermissions()
	cfg.UI = UI{Theme: "dark"}
	cfg.Tools = Tools{TimeoutS: 120}
	cfg.Context = Context{Ladder: "jev", KeepRecent: 8, MinChunkBytes: 2048, MinSavings: 8 << 10, Instructions: true,
		DigestTools: []string{"bash"}, DigestMinBytes: 16 << 10}
	cfg.Compaction = Compaction{Threshold: 100000}
	// A provider at 100% of a usage window is out until it resets; Codex keeps
	// 10% back for interactive use.
	cfg.Flex = Flex{
		Enabled:     true,
		Models:      []string{"google/gemini-"},
		Endpoints:   []string{"google-ai-studio/flex", "google-vertex/global/flex"},
		FirstTokenS: 30,
	}
	cfg.Limits = Limits{MaxLive: 64, Reserve: map[string]float64{"codex": 90, "opencode-go": 100, "openrouter": 100}}
	cfg.Routing.Fallback = map[string]string{
		"gpt-sol":          "mimo-v2.6-pro",
		"mimo-v2.6-pro":    "claude-sonnet-5.5",
		"mimo-v2.6-flash":  "gpt-luna",
		"gpt-luna":         "claude-haiku-5.5",
		"claude-haiku-5.5": "deepseek-v4.1-flash",
		"gpt-astra":        "claude-fable-5.1",
	}
	cfg.Agent = Agent{
		MaxTurnsRoot: 200, MaxTurns: 80, AutoCompact: 0.7, ChildTimeout: "0",
		MaxTurnsByDepth: map[string]int{"1": 150}, MaxTurnsCap: 300,
		MaxTurnsByRole: map[string]int{"worker": 80, "scout": 60, "reviewer": 40, "planner": 30, "oracle": 30, "log-parser": 20},
	}
	return cfg
}

func defaultPermissions() Permissions {
	// The plan's defaults: all classifiers on, everything prompts on any
	// flag, static_normal stops (§4.1/§4.2).
	p := perm.DefaultPolicy()
	return Permissions{
		Mode: p.Mode, Aggregate: p.Aggregate, AggregateAction: p.AggregateAction,
		HeadlessPrompt: p.HeadlessPrompt, Classifiers: p.Classifiers, Tiers: p.Tiers,
	}
}

// Path returns the config file location. An absolute XDG_CONFIG_HOME takes
// precedence; otherwise HOME/.config is used.
func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home := os.Getenv("HOME")
		if !filepath.IsAbs(home) {
			return "", errors.New("config: absolute HOME required")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "ugh", "config.toml"), nil
}

// Load decodes the config file over the defaults. A missing file is not an
// error; an invalid one is.
func Load() (Config, error) {
	cfg := Default()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return validate(cfg)
		}
		return cfg, err
	}
	if _, err := toml.DecodeFile(p, &cfg); err != nil {
		return Default(), fmt.Errorf("config: %w", err)
	}
	mergeClassifierDefaults(&cfg)
	if cfg.Permissions.Tiers == nil {
		cfg.Permissions.Tiers = map[string][]string{}
	}
	for tier, panel := range perm.DefaultTiers() {
		if _, ok := cfg.Permissions.Tiers[tier]; !ok {
			cfg.Permissions.Tiers[tier] = panel
		}
	}
	if cfg.Agent.MaxTurnsByRole == nil {
		cfg.Agent.MaxTurnsByRole = map[string]int{}
	}
	for k, v := range Default().Routing.Fallback {
		if cfg.Routing.Fallback == nil {
			cfg.Routing.Fallback = map[string]string{}
		}
		if _, ok := cfg.Routing.Fallback[k]; !ok {
			cfg.Routing.Fallback[k] = v
		}
	}
	if cfg.Limits.Reserve == nil {
		cfg.Limits.Reserve = Default().Limits.Reserve
	}
	if cfg.Agent.MaxTurnsByDepth == nil {
		cfg.Agent.MaxTurnsByDepth = Default().Agent.MaxTurnsByDepth
	}
	for role, n := range Default().Agent.MaxTurnsByRole {
		if _, ok := cfg.Agent.MaxTurnsByRole[role]; !ok {
			cfg.Agent.MaxTurnsByRole[role] = n
		}
	}
	for tier, id := range Default().Routing.Tiers {
		if cfg.Routing.Tiers == nil {
			cfg.Routing.Tiers = map[string]string{}
		}
		if cfg.Routing.Tiers[tier] == "" {
			cfg.Routing.Tiers[tier] = id
		}
	}
	if cfg.Routing.Routers == nil {
		cfg.Routing.Routers = map[string]RouterConfig{}
	}
	for k, v := range Default().Routing.Routers {
		if _, ok := cfg.Routing.Routers[k]; !ok {
			cfg.Routing.Routers[k] = v
		}
	}
	if cfg.Routing.PreferredRouter == "" {
		cfg.Routing.PreferredRouter = Default().Routing.PreferredRouter
	}
	if cfg.Routing.FallbackRouter == "" {
		cfg.Routing.FallbackRouter = Default().Routing.FallbackRouter
	}
	if cfg.Routing.ChatGPTPlan == "" {
		cfg.Routing.ChatGPTPlan = Default().Routing.ChatGPTPlan
	}
	if cfg.Routing.PolicyVersion == 0 {
		cfg.Routing.PolicyVersion = Default().Routing.PolicyVersion
	}
	return validate(cfg)
}

// mergeClassifierDefaults restores per-classifier fields the TOML decoder
// drops when a classifier table appears in the file (decoding into a map
// entry replaces the whole entry). Keys absent from the file keep their
// defaults, as everywhere else in this package; explicit values always win.
func mergeClassifierDefaults(cfg *Config) {
	def := defaultPermissions().Classifiers
	for name, c := range cfg.Permissions.Classifiers {
		d, ok := def[name]
		if !ok {
			continue
		}
		_, otiSet := c.Thresholds["off_task_or_injected"]
		if c.Questions == nil {
			c.Questions = d.Questions
		}
		if c.State == "" {
			c.State = d.State
		}
		if c.OnFlag == "" {
			c.OnFlag = d.OnFlag
		}
		if c.OnError == "" {
			c.OnError = d.OnError
		}
		if len(c.Tools) == 0 {
			c.Tools = d.Tools
		}
		if c.TimeoutMS == 0 {
			c.TimeoutMS = d.TimeoutMS
		}
		if c.BaseURL == "" {
			c.BaseURL = d.BaseURL
		}
		if c.Path == "" {
			c.Path = d.Path
		}
		if c.Model == "" {
			c.Model = d.Model
		}
		if c.KeyEnv == "" {
			c.KeyEnv = d.KeyEnv
		}
		if c.SessionHeader == "" {
			c.SessionHeader = d.SessionHeader
		}
		if c.StateEncoding == "" {
			c.StateEncoding = d.StateEncoding
		}
		if len(d.Thresholds) > 0 {
			if c.Thresholds == nil {
				c.Thresholds = map[string]float64{}
			}
			for k, v := range d.Thresholds {
				if _, ok := c.Thresholds[k]; !ok {
					c.Thresholds[k] = v
				}
			}
		}
		// off_task_or_injected stays min(off_task, injected) unless set
		// explicitly, recomputed when either pair member was overridden.
		if !otiSet {
			if ot, ok := c.Thresholds["off_task"]; ok {
				if inj, ok := c.Thresholds["injected"]; ok {
					c.Thresholds["off_task_or_injected"] = math.Min(ot, inj)
				}
			}
		}
		cfg.Permissions.Classifiers[name] = c
	}
}

func validate(cfg Config) (Config, error) {
	switch cfg.UI.Theme {
	case "", "dark", "light", "system":
		if cfg.UI.Theme == "" {
			cfg.UI.Theme = Default().UI.Theme
		}
	default:
		return Default(), fmt.Errorf("config: ui.theme must be dark|light|system, got %q", cfg.UI.Theme)
	}
	for ref, p := range cfg.Pricing {
		if p.In < 0 || p.Out < 0 || p.Cached < 0 {
			return Default(), fmt.Errorf("config: pricing %q must not be negative", ref)
		}
	}
	switch cfg.Auth.Store {
	case "auto", "keyring", "file":
	default:
		return Default(), fmt.Errorf("config: auth.store must be auto|keyring|file, got %q", cfg.Auth.Store)
	}
	if cfg.Models.Default != "" && !strings.Contains(cfg.Models.Default, "/") {
		return Default(), fmt.Errorf("config: models.default must be provider/model, got %q", cfg.Models.Default)
	}
	if cfg.Routing.PickModel != "jev" && cfg.Routing.PickModel != "clef" && cfg.Routing.PickModel != "table" {
		return Default(), fmt.Errorf("config: routing.pick_model must be jev|clef|table")
	}
	switch cfg.Routing.PreferredRouter {
	case "", "jev", "clef":
	default:
		return Default(), fmt.Errorf("config: routing.preferred_router must be jev|clef, got %q", cfg.Routing.PreferredRouter)
	}
	switch cfg.Routing.FallbackRouter {
	case "", "clef", "jev":
	default:
		return Default(), fmt.Errorf("config: routing.fallback_router must be jev|clef, got %q", cfg.Routing.FallbackRouter)
	}
	switch cfg.Routing.ChatGPTPlan {
	case "", "auto", "basic", "5x", "10x", "25x":
	default:
		return Default(), fmt.Errorf("config: routing.chatgpt_plan must be auto|basic|5x|10x|25x, got %q", cfg.Routing.ChatGPTPlan)
	}
	if cfg.Routing.ProviderConcurrency < 1 {
		return Default(), fmt.Errorf("config: routing.provider_concurrency must be positive")
	}
	if cfg.Agent.MaxTurnsRoot < 1 || cfg.Agent.MaxTurns < 1 {
		return Default(), fmt.Errorf("config: agent.max_turns_root and agent.max_turns must be positive")
	}
	if t := cfg.Agent.ChildTimeout; t != "" && t != "0" {
		if d, err := time.ParseDuration(t); err != nil || d < 0 {
			return Default(), fmt.Errorf("config: agent.child_timeout must be a duration like \"45m\" or \"0\", got %q", t)
		}
	}
	if t := cfg.UI.AskTimeout; t != "" && t != "0" {
		if d, err := time.ParseDuration(t); err != nil || d < 0 {
			return Default(), fmt.Errorf("config: ui.ask_timeout must be a duration like \"5m\" or \"0\", got %q", t)
		}
	}
	if cfg.Tools.TimeoutS < 1 {
		return Default(), fmt.Errorf("config: tools.timeout_s must be positive")
	}
	if cfg.Limits.MaxLive < 1 {
		return Default(), fmt.Errorf("config: limits.max_live must be positive")
	}
	for prov, n := range cfg.Limits.Streams {
		if n < 1 {
			return Default(), fmt.Errorf("config: limits.streams.%s must be positive", prov)
		}
	}
	if cfg.Agent.AutoCompact < 0 || cfg.Agent.AutoCompact >= 1 {
		return Default(), fmt.Errorf("config: agent.auto_compact must be in [0, 1), got %v", cfg.Agent.AutoCompact)
	}
	if l := cfg.Context.Ladder; l != "jev" && l != "off" {
		return Default(), fmt.Errorf(`config: context.ladder must be "jev" or "off", got %q`, l)
	}
	if cfg.Context.KeepRecent < 0 || cfg.Context.MinChunkBytes < 0 || cfg.Context.MinSavings < 0 || cfg.Context.DigestMinBytes < 0 {
		return Default(), fmt.Errorf("config: context.keep_recent, min_chunk_bytes, min_savings and digest_min_bytes must not be negative")
	}
	if cfg.Compaction.Threshold < 0 {
		return Default(), fmt.Errorf("config: compaction.threshold must not be negative (0 = no absolute limit)")
	}
	for ref, n := range cfg.Compaction.Thresholds {
		if n < 0 {
			return Default(), fmt.Errorf("config: compaction.thresholds.%q must not be negative", ref)
		}
	}
	if cfg.Agent.MaxTurnsCap < 1 {
		return Default(), fmt.Errorf("config: agent.max_turns_cap must be positive")
	}
	for d, n := range cfg.Agent.MaxTurnsByDepth {
		if i, err := strconv.Atoi(d); err != nil || i < 1 || n < 1 {
			return Default(), fmt.Errorf("config: agent.max_turns_by_depth needs depth ≥ 1 = positive budget, got %s = %d", d, n)
		}
	}
	for role, n := range cfg.Agent.MaxTurnsByRole {
		if n < 1 {
			return Default(), fmt.Errorf("config: agent.max_turns_by_role.%s must be positive", role)
		}
	}
	for name, p := range cfg.Providers {
		if err := validateProvider(name, p); err != nil {
			return Default(), err
		}
	}
	if err := validatePermissions(cfg.Permissions); err != nil {
		return Default(), err
	}
	return cfg, nil
}

var providerName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

// validateProvider checks one [providers.<name>] entry: a clean name that does
// not shadow codex, and an HTTPS (or loopback) base URL.
func validateProvider(name string, p Provider) error {
	if !providerName.MatchString(name) || name == "codex" {
		return fmt.Errorf("config: providers.%s: invalid name (lowercase [a-z0-9._-], not codex)", name)
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("config: providers.%s: name must not contain /", name)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("config: providers.%s.base_url must be an absolute URL", name)
	}
	host := u.Hostname()
	loop := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return fmt.Errorf("config: providers.%s.base_url must be https (or http on localhost)", name)
	}
	switch p.Usage {
	case "", "none", "opencode", "openrouter":
	default:
		return fmt.Errorf("config: providers.%s.usage must be opencode|openrouter|none", name)
	}
	if want := map[string]string{"opencode": "opencode.ai", "openrouter": "openrouter.ai"}[p.Usage]; want != "" && !loop && host != want {
		return fmt.Errorf("config: providers.%s.usage = %q only works against %s or localhost, not %s", name, p.Usage, want, host)
	}
	if p.KeyEnv != "" && !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(p.KeyEnv) {
		return fmt.Errorf("config: providers.%s.key_env must be an env var name", name)
	}
	return nil
}

// Provider is a config-defined OpenAI-compatible (Chat Completions) endpoint.
type Provider struct {
	BaseURL       string            `toml:"base_url"`
	KeyEnv        string            `toml:"key_env"`
	Headers       map[string]string `toml:"headers"`
	SessionHeader bool              `toml:"session_header"`
	NoThinking    []string          `toml:"no_thinking"` // model id prefixes
	Usage         string            `toml:"usage"`       // opencode | openrouter | none
}

func validatePermissions(p Permissions) error {
	if p.Mode != "manual" && p.Mode != "classify" && p.Mode != "auto-safe" && p.Mode != "auto" {
		return fmt.Errorf("config: permissions.mode must be manual|classify|auto-safe|auto, got %q", p.Mode)
	}
	if p.HeadlessPrompt != "deny" && p.HeadlessPrompt != "stop" && p.HeadlessPrompt != "allow" {
		return fmt.Errorf("config: permissions.headless_prompt must be deny|stop|allow, got %q", p.HeadlessPrompt)
	}
	if p.AggregateAction != "max" && p.AggregateAction != "prompt" && p.AggregateAction != "stop" {
		return fmt.Errorf("config: permissions.aggregate_action must be max|prompt|stop, got %q", p.AggregateAction)
	}
	if !validAggregate(p.Aggregate) {
		return fmt.Errorf("config: permissions.aggregate must be any|all|n:k, got %q", p.Aggregate)
	}
	for tier, panel := range p.Tiers {
		switch tier {
		case "trivial", "low", "middle", "high":
		default:
			return fmt.Errorf("config: permissions.tiers.%s: tiers are trivial|low|middle|high", tier)
		}
		for _, name := range panel {
			if _, ok := p.Classifiers[name]; !ok {
				return fmt.Errorf("config: permissions.tiers.%s names unknown classifier %q", tier, name)
			}
		}
	}
	for name, c := range p.Classifiers {
		switch c.State {
		case "", "on", "log", "off":
		default:
			return fmt.Errorf("config: classifier %s state must be on|log|off, got %q", name, c.State)
		}
		switch c.OnFlag {
		case "", "prompt", "stop":
		default:
			return fmt.Errorf("config: classifier %s on_flag must be prompt|stop, got %q", name, c.OnFlag)
		}
		switch c.StateEncoding {
		case "", "json", "string":
		default:
			return fmt.Errorf("config: classifier %s state_encoding must be json|string, got %q", name, c.StateEncoding)
		}
		for q, qq := range c.Questions {
			switch qq.Type {
			case "", "choice", "score", "noul":
			default:
				return fmt.Errorf("config: classifier %s question %s type must be choice|score|noul, got %q", name, q, qq.Type)
			}
		}
	}
	return nil
}

func validAggregate(s string) bool {
	switch s {
	case "", "any", "all":
		return true
	}
	n, _, ok := strings.Cut(s, ":")
	if !ok {
		return false
	}
	i, err := strconv.Atoi(strings.TrimSpace(n))
	return err == nil && i > 0
}
