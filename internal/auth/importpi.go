package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ImportOptions configures `ugh auth import pi` (§3.1 of PLAN.md).
type ImportOptions struct {
	Path  string   // Pi's auth.json; default ~/.pi/agent/auth.json
	Only  []string // restrict the import set (ugh names; openai-codex accepted)
	Codex bool     // import the openai-codex OAuth entry as codex
	Force bool     // overwrite existing store entries
}

// ImportResult is the display-safe outcome of an import.
type ImportResult struct {
	Imported []string // "provider masked-key" lines
	Skipped  []string // provider names only, never values
	Notes    []string // e.g. "run `ugh login codex` for an independent session"
	Warnings []string
}

// piCredential mirrors Pi's auth.json entry shape.
type piCredential struct {
	Type      string `json:"type"`
	Key       string `json:"key"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId"`
}

// defaultImportSet are the static providers ugh uses (§3.1 step 2).
var defaultImportSet = []string{"opencode-go", "openrouter"}

// piName maps a ugh provider to its Pi entry name.
func piName(ugh string) string {
	if ugh == "codex" {
		return "openai-codex"
	}
	return ugh
}

// ughName maps a Pi entry name to ugh's provider name.
func ughName(pi string) string {
	if pi == "openai-codex" {
		return "codex"
	}
	return pi
}

// ImportPi performs a one-shot explicit import of Pi's auth.json. It only
// reads Pi's file; default imports are static API keys for opencode-go and
// openrouter, and codex OAuth requires the explicit Codex flag (OpenAI
// rotates refresh tokens — a shared token would log Pi out).
func ImportPi(s *Store, opt ImportOptions) (ImportResult, error) {
	var res ImportResult
	path := opt.Path
	if path == "" {
		home := os.Getenv("HOME")
		if !filepath.IsAbs(home) {
			return res, fmt.Errorf("auth: cannot locate Pi auth.json: absolute HOME required (or pass --path)")
		}
		path = filepath.Join(home, ".pi", "agent", "auth.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return res, fmt.Errorf("auth: read Pi auth.json: %w", err)
	}
	var entries map[string]piCredential
	if err := json.Unmarshal(raw, &entries); err != nil {
		return res, fmt.Errorf("auth: Pi auth.json is not valid JSON")
	}

	// Build the import set: defaults (plus codex on request), optionally
	// narrowed by --only.
	set := append([]string{}, defaultImportSet...)
	if opt.Codex {
		set = append(set, "codex")
	}
	if len(opt.Only) > 0 {
		set = nil
		for _, n := range opt.Only {
			name := ughName(strings.TrimSpace(n))
			switch name {
			case "opencode-go", "openrouter":
				set = append(set, name)
			case "codex":
				if !opt.Codex {
					return res, fmt.Errorf("auth: importing codex requires --codex (Pi and ugh would share a rotating refresh token)")
				}
				set = append(set, "codex")
			default:
				return res, fmt.Errorf("auth: unknown provider %q in --only (supported: opencode-go, openrouter, codex)", n)
			}
		}
	}

	imported := map[string]bool{}
	for _, name := range set {
		pi := piName(name)
		entry, ok := entries[pi]
		if !ok {
			continue
		}
		cred, ok := importCredential(name, entry)
		if !ok {
			res.Skipped = append(res.Skipped, pi)
			continue
		}
		if _, exists, err := s.Get(name); err != nil {
			return res, err
		} else if exists && !opt.Force {
			res.Skipped = append(res.Skipped, pi)
			continue
		}
		if err := s.Put(name, cred); err != nil {
			return res, err
		}
		if name == "opencode-go" {
			// Keep the shared OpenCode entry in step (Zen, etc. reuse it).
			if err := s.PutOpenCode(cred.Key, !opt.Force); err != nil {
				return res, err
			}
		}
		imported[pi] = true
		masked := MaskKey(cred.Key)
		if masked == "" {
			masked = MaskKey(cred.Access)
		}
		res.Imported = append(res.Imported, fmt.Sprintf("%s %s", name, masked))
	}

	// Everything else in Pi's file is reported by name only.
	for pi := range entries {
		if !imported[pi] {
			res.Skipped = append(res.Skipped, pi)
		}
	}
	sort.Strings(res.Skipped)

	if _, ok := entries["openai-codex"]; ok && !opt.Codex {
		res.Notes = append(res.Notes, "codex: not imported (OpenAI rotates refresh tokens); run `ugh login codex` for an independent session")
	}
	if imported["openai-codex"] {
		res.Warnings = append(res.Warnings, "codex: imported shared OAuth — Pi will need to re-login after ugh's first refresh")
	}
	return res, nil
}

// importCredential converts one Pi entry; false means skip it (wrong type,
// empty value, or oauth without --codex already handled by the import set).
func importCredential(name string, e piCredential) (Credential, bool) {
	switch {
	case name == "codex":
		if e.Type != "oauth" || e.Access == "" || e.Refresh == "" {
			return Credential{}, false
		}
		return Credential{Type: "oauth", Access: e.Access, Refresh: e.Refresh, Expires: e.Expires, AccountID: e.AccountID}, true
	case e.Type == "api_key":
		if checkKey(e.Key) != nil {
			return Credential{}, false
		}
		return Credential{Type: "api_key", Key: e.Key}, true
	default:
		return Credential{}, false
	}
}
