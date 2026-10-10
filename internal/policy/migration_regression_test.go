package policy

import (
	"bytes"
	"encoding/json"
	"github.com/unix/ugh/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationPreservesRawAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# user comment\nunknown = 'keep'\n[routing] # routing comment\npick_model = 'table' # saved choice\npolicy_version = 0 # old marker\n[routing.tiers]\nhigh = 'mimo-v2.6-pro' # explicitly saved old default\n[future]\nvalue = 'keep me'\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if _, err := MigrateConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Replace(original, "policy_version = 0", "policy_version = 1", 1)
	if string(got) != want {
		t.Fatalf("unexpected migration:\n%s", got)
	}
	backup, _ := os.ReadFile(path + ".bak")
	if string(backup) != original {
		t.Fatal("backup does not preserve original")
	}
	if _, err := MigrateConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if !bytes.Equal(got, again) {
		t.Fatal("migration is not idempotent")
	}
}

func TestMigrationDetectsAbsentVersionDespiteNewDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "[routing]\npick_model = 'table'\n[routing.tiers]\nhigh = 'codex/gpt-6-sol'\n"
	os.WriteFile(path, []byte(original), 0600)
	cfg := config.Default() // in-memory version is already current
	if _, err := MigrateConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "policy_version = 1") || !strings.Contains(string(got), "codex/gpt-6-sol") {
		t.Fatalf("%s", got)
	}
}

func TestScopeManifestDistinguishesEmptyDefaultAndUnrestricted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scope.json")
	eval := FuncAccessEvaluator(func(string, string) (AccessStatus, string) { return AccessGranted, "test" })
	for _, tc := range []struct {
		mode  string
		state ScopeState
		empty bool
	}{
		{"custom", ScopeStateCustom, true}, {"default", ScopeStateAbsent, false}, {"unrestricted", ScopeStateUnrestricted, false},
	} {
		b, _ := json.Marshal(ScopeDocument{Version: 1, Mode: tc.mode})
		os.WriteFile(path, b, 0600)
		got := ResolveScopedModels(path, eval)
		if got.State != tc.state || (len(got.Refs) == 0) != tc.empty {
			t.Fatalf("%s: %+v", tc.mode, got)
		}
	}
	for _, b := range []string{"[]", "null", "broken", `{"version":99,"mode":"default"}`} {
		os.WriteFile(path, []byte(b), 0600)
		got := ResolveScopedModels(path, eval)
		if len(got.Refs) != 0 || len(got.Warnings) == 0 {
			t.Fatalf("unsafe expansion for %s: %+v", b, got)
		}
	}
}

func TestEligibilityRejectsUnknownPurposeAndUnauthorizedSynthesis(t *testing.T) {
	for _, purpose := range []Purpose{"", "bogus"} {
		if got := CheckEligibility(EligibilityRequest{Purpose: purpose, Family: FamilyMiMoPro, Provider: "openrouter", Access: AccessGranted}); got.Eligible {
			t.Fatalf("unknown purpose accepted: %s", purpose)
		}
	}
	for _, provider := range []string{"openrouter", "opencode-zen"} {
		got := CheckEligibility(EligibilityRequest{Purpose: PurposeCouncilSynth, Family: FamilyGPTSol, Provider: provider, Access: AccessGranted})
		if got.Eligible {
			t.Fatalf("non-Codex Sol synthesis accepted: %s", provider)
		}
	}
	req := EligibilityRequest{Purpose: PurposeCouncilSynth, Family: FamilyGPTAstra, Provider: "codex", Transport: TransportResponses, Access: AccessGranted, OracleLed: true, ApprovalRequired: true}
	if CheckEligibility(req).Eligible {
		t.Fatal("missing approval accepted")
	}
	req.Approved = true
	if !CheckEligibility(req).Eligible {
		t.Fatal("trusted oracle-led synthesis rejected")
	}
	var untrusted EligibilityRequest
	raw, _ := json.Marshal(map[string]any{"family": FamilyGPTAstra, "provider": "codex", "purpose": PurposeCouncilSynth, "transport": TransportResponses, "access": AccessGranted, "OracleLed": true, "Approved": true})
	json.Unmarshal(raw, &untrusted)
	if untrusted.OracleLed || untrusted.Approved || CheckEligibility(untrusted).Eligible {
		t.Fatal("JSON granted workflow authority")
	}
}

func TestBackupDoesNotTrustPartialInterruptedBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path+".bak", []byte("partial"), 0600)
	original := []byte("complete original")
	if err := BackupOriginal(path, original); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(path + ".bak.*")
	if len(entries) != 1 {
		t.Fatalf("backups: %v", entries)
	}
	b, _ := os.ReadFile(entries[0])
	if !bytes.Equal(b, original) {
		t.Fatal("original was not backed up")
	}
	old, _ := os.ReadFile(path + ".bak")
	if string(old) != "partial" {
		t.Fatal("existing backup overwritten")
	}
}
