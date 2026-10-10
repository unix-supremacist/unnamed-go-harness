// Package provider resolves "provider/model" refs into streaming clients
// and assembles the permission gate shared by headless mode and the TUI.
// Wired: opencode-go + openrouter (chat) and codex (Responses SSE).
package provider

import (
	"context"
	"crypto/rand"
	"fmt"
	"github.com/unix/ugh/internal/session"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/llm/chat"
	"github.com/unix/ugh/internal/llm/codex"
	"github.com/unix/ugh/internal/perm"
	"github.com/unix/ugh/internal/perm/model"
	"github.com/unix/ugh/internal/perm/static"
	"github.com/unix/ugh/internal/usage"
)

// openStore opens the credential store per config.
func openStore() (*auth.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return auth.Open(cfg.Auth.Store)
}

// Resolve resolves a "provider/model" ref into a streaming client. Keys and
// tokens come from the environment or the credential store, never Pi's files.
func Resolve(ref string) (llm.Provider, string, error) {
	prov, model, ok := strings.Cut(ref, "/")
	if !ok || prov == "" || model == "" {
		return nil, "", fmt.Errorf("model %q must be provider/model (e.g. opencode-go/mimo-v2.6-pro)", ref)
	}
	if prov == "codex" {
		return codex.New(codexTokens, nil), model, nil
	}
	if reason := unsupportedRouteReason(prov, model); reason != "" {
		return nil, "", fmt.Errorf("unsupported route %s: %s", ref, reason)
	}
	spec, wired := Lookup(prov)
	if !wired {
		return nil, "", fmt.Errorf("provider %q is not wired (%s)", prov, strings.Join(Names(), ", "))
	}
	key, err := specKey(spec)
	if err != nil {
		return nil, "", err
	}
	c := chat.New(spec.BaseURL, key, nil)
	c.Prov = prov
	c.NoThinking = spec.NoThinking
	c.Headers = spec.Headers
	if spec.Session {
		c.Session = newUUID() // OpenCode requires x-opencode-session
	}
	return c, model, nil
}

// The shared resolver also guards explicit/manual refs, which need not have
// passed through a default card or scope. These are known hard incompatibilities.
func unsupportedRouteReason(prov, model string) string {
	if prov != "opencode-go" {
		return ""
	}
	if strings.TrimPrefix(model, "openai/") == "gpt-6-luna" {
		return "Go Luna requires unsupported Responses transport"
	}
	if strings.HasPrefix(model, "gemini-") || strings.HasPrefix(model, "google/gemini-") {
		return "Gemini never uses Go"
	}
	return ""
}

// specKey is the credential for a provider spec: the Console login token for
// discovered connections, else the static key.
func specKey(s Spec) (string, error) {
	if s.Console {
		return consoleKey()
	}
	return apiKey(s.Name)
}

// apiKey resolves a static key (env first, then store) and registers it with
// the scrubber so it never lands in logs, prompts or sessions.
func apiKey(prov string) (string, error) {
	store, err := openStore()
	if err != nil {
		return "", err
	}
	key, err := auth.Resolve(prov, os.LookupEnv, store)
	if err != nil {
		return "", err
	}
	perm.RegisterSecret(key)
	return key, nil
}

// codexTokens supplies the refreshed OAuth access token per turn.
func codexTokens(ctx context.Context) (string, string, error) {
	store, err := openStore()
	if err != nil {
		return "", "", err
	}
	access, account, err := auth.CodexTokens(ctx, store)
	if err == nil {
		perm.RegisterSecret(access)
	}
	return access, account, err
}

// modelClassifiers lists the model instances the gate wires, in log order.
var modelClassifiers = []string{"jev_go", "jev_or", "clef_or", "respan_or", "clef_flash_or", "luna_or"}

// Gate assembles the permission gate: config policy (optionally overridden
// by mode), the static classifiers, the four model classifiers (§4.3) with
// the untrusted-output check, the given prompter (stdin broker for headless,
// channel broker for the TUI) and the JSONL decision log.
func Gate(mode string, p perm.Prompter) (*perm.Engine, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	pol := cfg.Permissions.Policy()
	if mode != "" {
		switch mode {
		case "manual", "classify", "auto-safe", "auto":
			pol.Mode = mode
		default:
			return nil, fmt.Errorf("-mode must be manual|classify|auto-safe|auto, got %q", mode)
		}
	}
	log, err := perm.NewLog(perm.DefaultLogPath())
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	e := perm.NewEngine(pol, p, log)
	e.Triage = static.Triage
	scratch := ""
	if base, err := session.StateDir(); err == nil {
		scratch = filepath.Join(filepath.Dir(base), "scratch")
	}
	static.SetWorkspace(append(append([]string{}, cfg.Permissions.WorkspaceRoots...), os.TempDir(), scratch)...)
	for _, name := range []string{static.NameNormal, static.NameExtreme} {
		cl, err := static.New(name)
		if err != nil {
			return nil, err
		}
		e.Add(cl)
	}
	for _, name := range modelClassifiers {
		cc, ok := pol.Classifiers[name]
		if !ok || cc.State == "off" {
			continue
		}
		e.Add(model.New(name, cc, decisionKey(cc.KeyEnv), nil))
	}
	if oc, ok := pol.Classifiers[perm.OutputCheckName]; ok && oc.State != "off" {
		e.Output = model.NewOutputJudge(oc, decisionKey(oc.KeyEnv), nil)
	}
	return e, nil
}

// decisionKey resolves a classifier's key_env to the provider credential
// (env first, then store) and adds it to the scrubber's redaction list. A
// failure is returned per call, so an unavailable credential becomes an Err
// verdict (on_error=ignore by default) instead of a startup failure.
func decisionKey(keyEnv string) model.KeyFunc {
	provider := ""
	_ = Specs() // registers config-defined key envs with auth
	for _, p := range auth.KnownAPIKeyProviders() {
		if auth.EnvVar(p) == keyEnv {
			provider = p
		}
	}
	var (
		mu  sync.Mutex
		key string
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if key != "" {
			return key, nil
		}
		if provider == "" {
			return "", fmt.Errorf("permissions: unknown key_env %q", keyEnv)
		}
		store, err := openStore()
		if err != nil {
			return "", err
		}
		k, err := auth.Resolve(provider, os.LookupEnv, store)
		if err != nil {
			return "", err
		}
		perm.RegisterSecret(k)
		key = k
		return key, nil
	}
}

// newUUID returns a random v4 UUID for the OpenCode Go session header.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ugh-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UsageSources wires the usage report to ugh's credentials: one target per
// provider spec that reports usage.
func UsageSources() usage.Sources {
	src := usage.Sources{CodexTokens: codexTokens}
	for _, s := range Specs() {
		s := s
		t := usage.Target{Provider: s.Name, Kind: s.Usage, BaseURL: s.BaseURL, Headers: s.Headers,
			Key: func() (string, error) { return specKey(s) }}
		if s.Console {
			t.Kind = "ugh-spend" // the Console exposes no usage for BYOK connections
		}
		if t.Kind == "" {
			t.Kind = "none"
		}
		src.Targets = append(src.Targets, t)
	}
	return src
}
