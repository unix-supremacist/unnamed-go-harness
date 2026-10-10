// Package auth owns credential storage (§3.1 of PLAN.md): a JSON blob with
// one entry per provider, held by a pluggable backend (keyring or file).
// Secrets never appear in errors or display output — use MaskKey.
package auth

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const maxKey = 8192

// Credential is one stored provider entry.
type Credential struct {
	Type      string `json:"type"` // api_key | oauth
	Key       string `json:"key,omitempty"`
	Access    string `json:"access,omitempty"`
	Refresh   string `json:"refresh,omitempty"`
	Expires   int64  `json:"expires,omitempty"` // unix ms
	AccountID string `json:"account_id,omitempty"`
}

// Credentials is the whole stored document.
type Credentials struct {
	Version   int                   `json:"version"`
	Providers map[string]Credential `json:"providers"`
}

func newCredentials() *Credentials {
	return &Credentials{Version: 1, Providers: map[string]Credential{}}
}

// Backend persists the credential document. The keyring and file backends
// implement it; tests use an in-memory fake, so no keyring is touched there.
type Backend interface {
	Name() string
	Load() (*Credentials, error)
	Save(*Credentials) error
}

// Store performs read/modify/write operations through one backend.
type Store struct{ backend Backend }

// NewStore wraps a backend.
func NewStore(b Backend) *Store { return &Store{backend: b} }

// Backend reports the backend in use (e.g. "keyring" or "file").
func (s *Store) Backend() Backend { return s.backend }

// Get returns the credential stored for a provider.
func (s *Store) Get(provider string) (Credential, bool, error) {
	c, err := s.backend.Load()
	if err != nil {
		return Credential{}, false, err
	}
	cred, ok := c.Providers[provider]
	return cred, ok, nil
}

// Put validates and stores one credential.
func (s *Store) Put(provider string, cred Credential) error {
	if provider == "" || strings.TrimSpace(provider) != provider {
		return errors.New("auth: invalid provider name")
	}
	switch cred.Type {
	case "api_key":
		if err := checkKey(cred.Key); err != nil {
			return err
		}
	case "oauth":
		if cred.Access == "" || cred.Refresh == "" {
			return errors.New("auth: oauth credential requires access and refresh")
		}
	default:
		return fmt.Errorf("auth: unknown credential type %q", cred.Type)
	}
	c, err := s.backend.Load()
	if err != nil {
		return err
	}
	c.Providers[provider] = cred
	return s.backend.Save(c)
}

// Remove deletes a stored credential; it is an error when absent.
func (s *Store) Remove(provider string) error {
	c, err := s.backend.Load()
	if err != nil {
		return err
	}
	if _, ok := c.Providers[provider]; !ok {
		return fmt.Errorf("auth: no stored credential for %q", provider)
	}
	delete(c.Providers, provider)
	return s.backend.Save(c)
}

// List returns all stored providers sorted by name.
func (s *Store) List() ([]string, map[string]Credential, error) {
	c, err := s.backend.Load()
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, c.Providers, nil
}

// envFor maps the providers that use a static API key to their env var.
// Resolution order at runtime is env var, then store — nothing else.
var envFor = map[string]string{
	"opencode":     "OPENCODE_API_KEY",
	"opencode-go":  "OPENCODE_API_KEY",
	"opencode-zen": "OPENCODE_API_KEY",
	"openrouter":   "OPENROUTER_API_KEY",
}

// SharedCredential is the single stored entry (from `ugh login opencode`)
// that backs every OpenCode-hosted provider.
const SharedCredential = "opencode"

var (
	extraMu   sync.Mutex
	extraKeys []string // config-defined providers, in registration order
)

// sharesOpenCode reports whether a provider falls back to the shared entry.
func sharesOpenCode(p string) bool {
	return p != SharedCredential && envFor[p] == "OPENCODE_API_KEY"
}

// RegisterProvider adds a config-defined static-key provider. An empty env
// defaults to <NAME>_API_KEY. Built-ins are never overridden.
func RegisterProvider(name, env string) {
	extraMu.Lock()
	defer extraMu.Unlock()
	if env == "" {
		env = strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)) + "_API_KEY"
	}
	if _, ok := envFor[name]; ok && !contains(extraKeys, name) {
		return
	}
	if !contains(extraKeys, name) {
		extraKeys = append(extraKeys, name)
	}
	envFor[name] = env
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// KnownAPIKeyProviders lists static-key providers in display order.
func KnownAPIKeyProviders() []string {
	extraMu.Lock()
	defer extraMu.Unlock()
	return append([]string{SharedCredential, "opencode-go", "opencode-zen", "openrouter"}, extraKeys...)
}

// EnvVar returns the environment variable consulted before the store.
func EnvVar(provider string) string {
	extraMu.Lock()
	defer extraMu.Unlock()
	return envFor[provider]
}

// Resolve returns a provider's key: env var first (runtime precedence),
// then the store. Pi's files are never read implicitly.
func Resolve(provider string, lookup func(string) (string, bool), s *Store) (string, error) {
	if env := EnvVar(provider); env != "" && lookup != nil {
		if v, ok := lookup(env); ok && v != "" {
			return v, nil
		}
	}
	cred, ok, err := s.Get(provider)
	if err != nil {
		return "", err
	}
	if !ok && sharesOpenCode(provider) {
		cred, ok, err = s.Get(SharedCredential)
		if err != nil {
			return "", err
		}
	}
	if !ok {
		hint := "run `ugh auth set " + provider + "`"
		if env := envFor[provider]; env != "" {
			hint = "set " + env + " or " + hint
		}
		return "", fmt.Errorf("auth: no credential for %q (%s)", provider, hint)
	}
	if cred.Type != "api_key" {
		return "", fmt.Errorf("auth: credential for %q is %s, not an api_key", provider, cred.Type)
	}
	return cred.Key, nil
}

// MaskKey renders a secret for display: prefix + "…" + last 4, never the
// whole value (e.g. sk-or-…a1b2).
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	r := []rune(key)
	if len(r) < 12 {
		return "…" + string(r[len(r)-4:])
	}
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

// checkKey validates a static key without ever echoing it. Trimmed from the
// old store's helper.
func checkKey(key string) error {
	if len(key) == 0 || len(key) > maxKey || !utf8.ValidString(key) {
		return errors.New("auth: key is empty or exceeds size limit")
	}
	for _, c := range []byte(key) {
		if c < 0x20 || c == 0x7f {
			return errors.New("auth: key contains control bytes")
		}
	}
	return nil
}

// openCodeAliases are the per-provider entries that shadow the shared one.
var openCodeAliases = []string{"opencode-go", "opencode-zen"}

// PutOpenCode stores an OpenCode key as the shared entry and refreshes any
// existing per-provider entries, so a stale opencode-go key (e.g. from the Pi
// import) can't shadow the new one. With onlyIfMissing the shared entry and
// aliases are left alone when already present (importer without --force).
func (s *Store) PutOpenCode(key string, onlyIfMissing bool) error {
	cred := Credential{Type: "api_key", Key: key}
	for _, name := range append([]string{SharedCredential}, openCodeAliases...) {
		_, exists, err := s.Get(name)
		if err != nil {
			return err
		}
		if name != SharedCredential && !exists {
			continue // aliases are only refreshed, never created
		}
		if onlyIfMissing && exists {
			continue
		}
		if err := s.Put(name, cred); err != nil {
			return err
		}
	}
	return nil
}
