package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unix/ugh/internal/auth"
	"github.com/unix/ugh/internal/config"
	"github.com/unix/ugh/internal/policy"
	"github.com/unix/ugh/internal/provstate"
)

// Model is one model a provider reported from its /models endpoint.
type Model struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Context  int    `json:"context,omitempty"` // 0 when the provider does not report it
	// Thinking lists the reasoning efforts the provider reports for the model
	// (OpenRouter: reasoning.supported_efforts); empty when it reports none.
	Thinking []string `json:"thinking,omitempty"`
	// ThinkingRequired: reasoning cannot be turned off (reasoning.mandatory).
	ThinkingRequired bool `json:"thinking_required,omitempty"`
}

// Ref is the provider/model ref the rest of ugh uses.
func (m Model) Ref() string { return m.Provider + "/" + m.ID }

// codexModels lists the codex model ids (the Responses API has no listing
// endpoint); the chat providers are queried live.
var codexModels = []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna"}

// ListModels queries every wired provider's /models endpoint concurrently.
// A provider that fails (no credential, offline) falls back to its last cached
// listing and is reported in the returned errors, keyed by provider name.
func ListModels(ctx context.Context) ([]Model, map[string]error) {
	specs := Specs()
	type res struct {
		ms  []Model
		err error
	}
	out := make([]res, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].ms, out[i].err = fetchModels(ctx, s)
		}()
	}
	wg.Wait()
	var all []Model
	errs := map[string]error{}
	for i, s := range specs {
		if out[i].err != nil {
			errs[s.Name] = out[i].err
			all = append(all, cachedModels(s.Name)...)
			continue
		}
		storeModels(s.Name, out[i].ms)
		all = append(all, out[i].ms...)
	}
	for _, id := range codexModels {
		all = append(all, Model{Provider: "codex", ID: id})
	}
	return all, errs
}

func fetchModels(ctx context.Context, spec Spec) ([]Model, error) {
	key, err := specKey(spec)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: HTTP %d", resp.StatusCode)
	}
	return parseModels(spec.Name, resp.Body)
}

// parseModels decodes a /models body. Providers name the context window
// differently (OpenRouter: context_length); take whichever is present.
func parseModels(prov string, r io.Reader) ([]Model, error) {
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			ContextWindow int    `json:"context_window"`
			MaxContext    int    `json:"max_context_length"`
			Reasoning     *struct {
				Mandatory        bool     `json:"mandatory"`
				SupportedEfforts []string `json:"supported_efforts"`
			} `json:"reasoning"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		win := m.ContextLength
		if win == 0 {
			win = m.ContextWindow
		}
		if win == 0 {
			win = m.MaxContext
		}
		mod := Model{Provider: prov, ID: m.ID, Context: win}
		if r := m.Reasoning; r != nil && len(r.SupportedEfforts) > 0 {
			mod.Thinking, mod.ThinkingRequired = r.SupportedEfforts, r.Mandatory
		}
		out = append(out, mod)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- cache (last known listings, for context windows and offline fallback) ----

var (
	cacheMu   sync.Mutex
	cacheData map[string][]Model
)

func cachePath() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "ugh", "models.json")
}

func loadCacheLocked() {
	if cacheData != nil {
		return
	}
	cacheData = map[string][]Model{}
	if p := cachePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &cacheData)
		}
	}
}

func cachedModels(prov string) []Model {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	loadCacheLocked()
	return append([]Model(nil), cacheData[prov]...)
}

func storeModels(prov string, ms []Model) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	loadCacheLocked()
	cacheData[prov] = ms
	p := cachePath()
	if p == "" {
		return
	}
	if b, err := json.Marshal(cacheData); err == nil && os.MkdirAll(filepath.Dir(p), 0o700) == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// Thinking reports the reasoning efforts the last provider listing gave for
// a provider/model ref, and whether reasoning is mandatory. ok is false when
// the provider reported none. Like ContextWindow it never touches the network.
func Thinking(ref string) (efforts []string, required, ok bool) {
	prov, id, qualified := strings.Cut(ref, "/")
	if !qualified {
		return nil, false, false
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	loadCacheLocked()
	for _, m := range cacheData[prov] {
		if m.ID == id && len(m.Thinking) > 0 {
			return append([]string(nil), m.Thinking...), m.ThinkingRequired, true
		}
	}
	return nil, false, false
}

// RefreshModelsIfStale re-lists every provider's models when the cached
// listing is missing or older than maxAge, so context windows and thinking
// levels stay current without opening the model picker.
func RefreshModelsIfStale(ctx context.Context, maxAge time.Duration) {
	if p := cachePath(); p != "" {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) < maxAge {
			return
		}
	}
	ListModels(ctx)
}

// ContextWindow reports a model's context window from the last provider
// listing, for a provider/model ref or a bare model id. It never touches the
// network, so it is safe on the hot path.
func ContextWindow(ref string) (int, bool) {
	prov, id, qualified := strings.Cut(ref, "/")
	if !qualified {
		id = ref
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	loadCacheLocked()
	for p, ms := range cacheData {
		if qualified && p != prov {
			continue
		}
		for _, m := range ms {
			if m.ID == id && m.Context > 0 {
				return m.Context, true
			}
		}
	}
	return 0, false
}

// ---- scoped models (/scoped-models): the subset /model and ctrl+p cycle through ----

func scopedPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "ugh", "scoped-models.json")
}

// accessEvaluator inspects local credentials/config only. Discovery and stored
// credentials do not prove entitlement, and opening a picker must not refresh tokens.
func accessEvaluator() policy.AccessEvaluator {
	cfg, cfgErr := config.Load()
	if cfgErr == nil {
		_ = mergeSpecs(append([]Spec(nil), builtins...), cfg.Providers)
	}
	return policy.FuncAccessEvaluator(func(prov, model string) (policy.AccessStatus, string) {
		if cfgErr != nil {
			return policy.AccessDenied, cfgErr.Error()
		}
		store, err := auth.Open(cfg.Auth.Store)
		if err != nil {
			return policy.AccessNoCredential, err.Error()
		}
		account := ""
		if prov == "codex" {
			cred, ok, err := store.Get("codex")
			if err != nil || !ok || cred.Type != "oauth" || (cred.Access == "" && cred.Refresh == "") {
				return policy.AccessNoCredential, "Codex OAuth credential required"
			}
			account = cred.AccountID
		} else {
			if _, err := auth.Resolve(prov, os.LookupEnv, store); err != nil {
				return policy.AccessNoCredential, err.Error()
			}
		}
		transport := string(policy.TransportChat)
		if prov == "codex" {
			transport = string(policy.TransportResponses)
		}
		if st, bad := provstate.CheckRoute(prov, model, transport, account); bad {
			switch st.Kind {
			case "exhausted":
				return policy.AccessExhausted, st.Reason
			case "reserved":
				return policy.AccessReserved, st.Reason
			default:
				return policy.AccessDenied, st.Kind + ": " + st.Reason
			}
		}
		override := cfg.Routing.Access[prov+"/"+model]
		if override == "" {
			override = cfg.Routing.Access[prov]
		}
		switch override {
		case "granted":
			return policy.AccessGranted, "explicit user access override (not a detected entitlement)"
		case "denied", "unavailable":
			return policy.AccessDenied, "explicit user access restriction"
		}
		return policy.AccessUnknown, "credential present; entitlement not confirmed"
	})
}

// ModelAccess is advisory local access information for a root-picker ref.
func ModelAccess(ref string) (policy.AccessStatus, string) {
	prov, id, _ := strings.Cut(ref, "/")
	if reason := unsupportedRouteReason(prov, id); reason != "" {
		return policy.AccessUnsupported, reason
	}
	return accessEvaluator().CheckAccess(prov, id)
}

// Scoped returns the active scoped provider/model refs.
// If no scoped-models file exists, it returns the policy default root scope.
// If the file is empty, it warns about ambiguity and returns nil.
func Scope() policy.ScopedModelsResolution {
	return policy.ResolveScopedModels(scopedPath(), accessEvaluator())
}

// DefaultRootScope is the policy's default root model order for the current
// credentials (OpenRouter first, MiMo Pro first), ignoring any saved scope.
func DefaultRootScope() []string {
	return policy.DefaultRootScope(accessEvaluator())
}

func Scoped() []string {
	res := Scope()
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "%s\n", w)
	}
	if res.State == policy.ScopeStateUnrestricted {
		return []string{"*"}
	}
	return res.Refs
}

// SaveScoped persists an explicit custom scope, including an explicitly empty one.
// A sole "*" selects unrestricted mode. ResetScoped restores policy defaults.
func SaveScoped(refs []string) error {
	p := scopedPath()
	if p == "" {
		return fmt.Errorf("no config dir")
	}
	doc := policy.ScopeDocument{Version: policy.CurrentPolicyVersion, Mode: "custom", Refs: refs}
	if len(refs) == 1 && refs[0] == "*" {
		doc.Mode, doc.Refs = "unrestricted", nil
	} else {
		for _, ref := range refs {
			if ref == "" || ref == "*" {
				return fmt.Errorf("custom scope must contain model refs, not a wildcard")
			}
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadFile(p); err == nil {
		if err := policy.BackupOriginal(p, old); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return policy.AtomicReplace(p, b)
}

func ResetScoped() error {
	p := scopedPath()
	if p == "" {
		return fmt.Errorf("no config dir")
	}
	b, err := json.Marshal(policy.ScopeDocument{Version: policy.CurrentPolicyVersion, Mode: "default"})
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(p); err == nil {
		if err := policy.BackupOriginal(p, old); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return policy.AtomicReplace(p, b)
}

func compactModelPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "ugh", "compact-model.json")
}

// CompactModel is the summarizer chosen with /compact-model ("" when none is
// saved, so [compaction] model and then the conversation model apply).
func CompactModel() string {
	b, err := os.ReadFile(compactModelPath())
	if err != nil {
		return ""
	}
	var doc struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return doc.Model
}

// SaveCompactModel persists the /compact-model choice; "" clears it.
func SaveCompactModel(ref string) error {
	p := compactModelPath()
	if p == "" {
		return fmt.Errorf("no config dir")
	}
	if ref == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(map[string]string{"model": ref})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return policy.AtomicReplace(p, b)
}

// SetModelsForTest replaces the in-memory listing cache with one provider's
// models until the test ends (tests only).
func SetModelsForTest(t interface{ Cleanup(func()) }, prov string, ms []Model) {
	cacheMu.Lock()
	cacheData = map[string][]Model{prov: ms}
	cacheMu.Unlock()
	t.Cleanup(func() {
		cacheMu.Lock()
		cacheData = nil
		cacheMu.Unlock()
	})
}
