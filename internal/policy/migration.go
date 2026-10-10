package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/unix/ugh/internal/config"
)

const CurrentPolicyVersion = 1

type ScopeState string

const (
	ScopeStateAbsent       ScopeState = "absent"
	ScopeStateUnrestricted ScopeState = "unrestricted"
	ScopeStateCustom       ScopeState = "custom"
	ScopeStateEmpty        ScopeState = "empty" // ambiguous legacy empty or invalid input, never unrestricted
)

type ScopeDocument struct {
	Version int      `json:"version"`
	Mode    string   `json:"mode"`
	Refs    []string `json:"refs,omitempty"`
}

type ScopedModelsResolution struct {
	State    ScopeState
	Refs     []string
	Warnings []string
}

func ResolveScopedModels(path string, eval AccessEvaluator) ScopedModelsResolution {
	defaults := func() ScopedModelsResolution {
		return ScopedModelsResolution{State: ScopeStateAbsent, Refs: DefaultRootScope(eval)}
	}
	bad := func(reason string) ScopedModelsResolution {
		return ScopedModelsResolution{State: ScopeStateEmpty, Warnings: []string{reason}}
	}
	if path == "" {
		return defaults()
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return defaults()
	}
	if err != nil {
		return bad(fmt.Sprintf("cannot read model scope: %v; select a scope explicitly", err))
	}
	var raw []string
	if err := json.Unmarshal(b, &raw); err == nil {
		if len(raw) == 0 {
			return bad("legacy empty scope is ambiguous; explicitly save a custom empty scope, defaults, or unrestricted scope")
		}
		for _, ref := range raw {
			if ref == "*" {
				return ScopedModelsResolution{State: ScopeStateUnrestricted, Refs: []string{"*"}}
			}
		}
		return ScopedModelsResolution{State: ScopeStateCustom, Refs: raw}
	}
	var doc ScopeDocument
	if err := json.Unmarshal(b, &doc); err != nil || doc.Version != CurrentPolicyVersion {
		return bad("invalid or unsupported model scope; correct scoped-models.json before selecting a model")
	}
	switch doc.Mode {
	case "default":
		return defaults()
	case "unrestricted":
		return ScopedModelsResolution{State: ScopeStateUnrestricted, Refs: []string{"*"}}
	case "custom":
		for _, ref := range doc.Refs {
			if ref == "" || ref == "*" {
				return bad("custom scope contains an invalid ref; use unrestricted mode for all models")
			}
		}
		return ScopedModelsResolution{State: ScopeStateCustom, Refs: doc.Refs}
	default:
		return bad("invalid model scope mode; use default, unrestricted, or custom")
	}
}

// AtomicReplace writes only complete files. Callers serialize read/modify/write
// operations separately; temporary files are unique and never reused after a crash.
func AtomicReplace(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ugh-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// BackupOriginal never overwrites the first pre-migration backup.
func BackupOriginal(path string, data []byte) error {
	backup := path + ".bak"
	if old, err := os.ReadFile(backup); err == nil {
		if bytes.Equal(old, data) {
			return nil
		}
		sum := sha256.Sum256(data)
		backup = fmt.Sprintf("%s.bak.%x", path, sum[:8])
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ugh-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), backup); err != nil {
		if os.IsExist(err) {
			old, readErr := os.ReadFile(backup)
			if readErr == nil && bytes.Equal(old, data) {
				return nil
			}
		}
		return err
	}
	return nil
}

var routingHeader = regexp.MustCompile(`(?m)^\s*\[routing\][ \t]*(?:#[^\r\n]*)?\r?$`)
var versionLine = regexp.MustCompile(`(?m)^([ \t]*(?:policy_version|"policy_version"|'policy_version')[ \t]*=[ \t]*)([+-]?(?:0[xX][0-9a-fA-F_]+|0[oO][0-7_]+|0[bB][01_]+|[0-9][0-9_]*))`)

// MigrateConfig preserves the original TOML text, including unknown keys,
// comments and saved values equal to old defaults. Only a version marker changes.
// Legacy values remain intact and receive warnings, not guessed replacements.
func MigrateConfig(cfg *config.Config, path string) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	var warnings []string
	if v := cfg.Routing.Tiers["medium"]; v != "" {
		warnings = append(warnings, fmt.Sprintf("legacy routing.tiers.medium=%q needs an explicit low/high mapping; no mapping was inferred", v))
	}
	keys := make([]string, 0, len(cfg.Routing.Fallback))
	for k := range cfg.Routing.Fallback {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.Contains(cfg.Routing.Fallback[k], "glm") && k != "glm-5.3-flash" {
			warnings = append(warnings, fmt.Sprintf("obsolete GLM fallback %s -> %s retained; choose a supported low/high fallback explicitly", k, cfg.Routing.Fallback[k]))
		}
	}
	ids := make([]string, 0, len(cfg.Routing.Tiers)+2*len(keys))
	for _, tier := range []string{"low", "high", "oracle"} {
		ids = append(ids, cfg.Routing.Tiers[tier])
	}
	for _, k := range keys {
		ids = append(ids, k, cfg.Routing.Fallback[k])
	}
	for _, id := range ids {
		if _, ok := FamilyByID(id); id != "" && !ok {
			warnings = append(warnings, fmt.Sprintf("routing: unknown model family %q in routing.tiers/routing.fallback is skipped", id))
		}
	}
	if path == "" {
		return warnings, nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return warnings, nil
	} else if err != nil {
		return warnings, err
	}
	lock, err := os.OpenFile(path+".migration.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return warnings, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return warnings, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	original, err := os.ReadFile(path)
	if err != nil {
		return warnings, err
	}
	var parsed config.Config
	meta, err := toml.Decode(string(original), &parsed)
	if err != nil {
		return warnings, err
	}
	if meta.IsDefined("routing", "policy_version") && parsed.Routing.PolicyVersion >= CurrentPolicyVersion {
		cfg.Routing.PolicyVersion = parsed.Routing.PolicyVersion
		return warnings, nil
	}
	text := string(original)
	loc := routingHeader.FindStringIndex(text)
	marker := fmt.Sprintf("policy_version = %d", CurrentPolicyVersion)
	if loc != nil {
		end := len(text)
		if next := regexp.MustCompile(`(?m)^\s*\[`).FindStringIndex(text[loc[1]:]); next != nil {
			end = loc[1] + next[0]
		}
		section := text[loc[1]:end]
		if versionLine.MatchString(section) {
			section = versionLine.ReplaceAllString(section, "${1}"+fmt.Sprint(CurrentPolicyVersion))
		} else {
			section = "\n" + marker + section
		}
		text = text[:loc[1]] + section + text[end:]
	} else if meta.IsDefined("routing") {
		warnings = append(warnings, "inline or quoted routing table retained; add routing.policy_version=1 manually (automatic migration skipped)")
		return warnings, nil
	} else {
		text += "\n[routing]\n" + marker + "\n"
	}
	var before, check map[string]any
	_, beforeErr := toml.Decode(string(original), &before)
	checkMeta, checkErr := toml.Decode(text, &check)
	stripMarker := func(m map[string]any) {
		if routing, ok := m["routing"].(map[string]any); ok {
			delete(routing, "policy_version")
			if len(routing) == 0 {
				delete(m, "routing")
			}
		}
	}
	markerOK := false
	definedMarker := checkMeta.IsDefined("routing", "policy_version")
	if routing, ok := check["routing"].(map[string]any); ok {
		markerOK = routing["policy_version"] == int64(CurrentPolicyVersion)
	}
	stripMarker(before)
	stripMarker(check)
	if beforeErr != nil || checkErr != nil || !definedMarker || !markerOK || !reflect.DeepEqual(before, check) {
		warnings = append(warnings, "routing migration could not preserve all values; original retained, add routing.policy_version=1 manually")
		return warnings, nil
	}
	if err := BackupOriginal(path, original); err != nil {
		return warnings, err
	}
	if err := AtomicReplace(path, []byte(text)); err != nil {
		return warnings, err
	}
	cfg.Routing.PolicyVersion = CurrentPolicyVersion
	return warnings, nil
}
