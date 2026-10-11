package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unix/ugh/internal/config"
)

func TestPolicyDefaultRootScope(t *testing.T) {
	cases := []struct {
		name      string
		creds     map[string]bool
		wantRefs  []string
		avoidRefs []string
	}{
		{
			name:      "no credentials",
			creds:     map[string]bool{},
			wantRefs:  nil,
			avoidRefs: []string{"openrouter/xiaomi/mimo-v2.6-pro", "opencode-go/mimo-v2.6-pro", "codex/gpt-6.1-sol"},
		},
		{
			name:  "openrouter only",
			creds: map[string]bool{"openrouter": true},
			wantRefs: []string{
				"openrouter/xiaomi/mimo-v2.6-pro",
				"openrouter/xiaomi/mimo-v2.6-flash",
				"openrouter/openai/gpt-6.1-sol",
				"openrouter/openai/gpt-6-astra",
				"openrouter/openai/gpt-6-luna",
				"openrouter/anthropic/claude-haiku-5.5",
				"openrouter/anthropic/claude-sonnet-5.5",
				"openrouter/anthropic/claude-opus-5.5",
				"openrouter/anthropic/claude-fable-5.1",
			},
			avoidRefs: []string{"opencode-go/mimo-v2.6-pro", "codex/gpt-6.1-sol"},
		},
		{
			name:  "opencode-go only filters unsupported luna",
			creds: map[string]bool{"opencode-go": true},
			wantRefs: []string{
				"opencode-go/mimo-v2.6-pro",
				"opencode-go/mimo-v2.6-flash",
				"opencode-go/claude-haiku-5-5",
			},
			avoidRefs: []string{
				// OpenCode Go Luna requires Responses transport, unsupported on Go chat client!
				"opencode-go/gpt-6-luna",
				"openrouter/xiaomi/mimo-v2.6-pro",
			},
		},
		{
			name:  "opencode-zen uses Zen's own ids and has no MiMo",
			creds: map[string]bool{"opencode-zen": true},
			wantRefs: []string{
				"opencode-zen/gpt-6.1-sol",
				"opencode-zen/gpt-6-astra",
				"opencode-zen/gpt-6-luna",
				"opencode-zen/claude-haiku-5-5",
				"opencode-zen/claude-sonnet-5-5",
				"opencode-zen/claude-opus-5-5",
				"opencode-zen/claude-fable-5-1",
			},
			avoidRefs: []string{
				// Zen lists neither MiMo Pro nor plain MiMo Flash (only a -free variant).
				"opencode-zen/xiaomi/mimo-v2.6-pro",
				"opencode-zen/xiaomi/mimo-v2.6-flash",
			},
		},
		{
			name:  "codex only",
			creds: map[string]bool{"codex": true},
			wantRefs: []string{
				"codex/gpt-6.1-sol",
				"codex/gpt-6-astra",
				"codex/gpt-6-luna",
			},
			avoidRefs: []string{"codex/claude-haiku-5.5", "openrouter/openai/gpt-6-luna"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := FuncAccessEvaluator(func(prov, model string) (AccessStatus, string) {
				if tc.creds[prov] {
					return AccessGranted, "authenticated"
				}
				return AccessNoCredential, "no credential"
			})

			got := DefaultRootScope(eval)

			for _, want := range tc.wantRefs {
				if !contains(got, want) {
					t.Errorf("missing expected root model %s, got: %v", want, got)
				}
			}
			for _, avoid := range tc.avoidRefs {
				if contains(got, avoid) {
					t.Errorf("unexpected root model %s present in: %v", avoid, got)
				}
			}
		})
	}
}

func TestCheckEligibility(t *testing.T) {
	cases := []struct {
		name         string
		req          EligibilityRequest
		wantEligible bool
		wantStatus   AccessStatus
		reasonSubstr string
	}{
		{
			name: "root opus eligible with granted access",
			req: EligibilityRequest{
				Purpose:   PurposeRoot,
				Family:    FamilyClaudeOpus,
				Provider:  "openrouter",
				ModelID:   "anthropic/claude-opus-5.5",
				Transport: TransportChat,
				Access:    AccessGranted,
			},
			wantEligible: true,
			wantStatus:   AccessGranted,
		},
		{
			name: "delegated opus ineligible (root only)",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierHigh,
				Family:    FamilyClaudeOpus,
				Provider:  "openrouter",
				ModelID:   "anthropic/claude-opus-5.5",
				Transport: TransportChat,
				Access:    AccessGranted,
			},
			wantEligible: false,
			wantStatus:   AccessDenied,
			reasonSubstr: "root-only",
		},
		{
			name: "delegated glm ineligible (council only)",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierLow,
				Family:    FamilyGLMFlash,
				Provider:  "opencode-go",
				ModelID:   "glm-5.3-flash",
				Transport: TransportChat,
				Access:    AccessGranted,
			},
			wantEligible: false,
			wantStatus:   AccessDenied,
			reasonSubstr: "council-only",
		},
		{
			name: "council panel glm eligible",
			req: EligibilityRequest{
				Purpose:   PurposeCouncilPanel,
				Family:    FamilyGLMFlash,
				Provider:  "opencode-go",
				ModelID:   "glm-5.3-flash",
				Transport: TransportChat,
				Access:    AccessGranted,
			},
			wantEligible: true,
			wantStatus:   AccessGranted,
		},
		{
			name: "gemini on opencode-go rejected absolutely",
			req: EligibilityRequest{
				Purpose:   PurposeCouncilPanel,
				Family:    FamilyGeminiFlash,
				Provider:  "opencode-go",
				ModelID:   "gemini-3.8-flash",
				Transport: TransportChat,
				Access:    AccessGranted,
			},
			wantEligible: false,
			wantStatus:   AccessDenied,
			reasonSubstr: "never route through OpenCode Go",
		},
		{
			name: "delegated sol on openrouter ineligible (codex only for delegated)",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierHigh,
				Family:    FamilyGPTSol,
				Provider:  "openrouter",
				ModelID:   "openai/gpt-6.1-sol",
				Transport: TransportChat,
				Access:    AccessGranted,
			},
			wantEligible: false,
			wantStatus:   AccessDenied,
			reasonSubstr: "Codex only",
		},
		{
			name: "delegated sol on codex eligible",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierHigh,
				Family:    FamilyGPTSol,
				Provider:  "codex",
				ModelID:   "gpt-6.1-sol",
				Transport: TransportResponses,
				Access:    AccessGranted,
			},
			wantEligible: true,
			wantStatus:   AccessGranted,
		},
		{
			name: "delegated astra worker role ineligible (oracle only)",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierOracle,
				Family:    FamilyGPTAstra,
				Provider:  "codex",
				ModelID:   "gpt-6-astra",
				Transport: TransportResponses,
				Access:    AccessGranted,
			},
			wantEligible: false,
			wantStatus:   AccessDenied,
			reasonSubstr: "reserved for role oracle",
		},
		{
			name: "delegated astra oracle role eligible",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "oracle",
				Tier:      TierOracle,
				Family:    FamilyGPTAstra,
				Provider:  "codex",
				ModelID:   "gpt-6-astra",
				Transport: TransportResponses,
				Access:    AccessGranted,
			},
			wantEligible: true,
			wantStatus:   AccessGranted,
		},
		{
			name: "unsupported transport on opencode-go luna rejected",
			req: EligibilityRequest{
				Purpose:   PurposeRoot,
				Family:    FamilyGPTLuna,
				Provider:  "opencode-go",
				ModelID:   "gpt-6-luna",
				Transport: TransportResponses,
				Access:    AccessGranted,
			},
			wantEligible: false,
			wantStatus:   AccessUnsupported,
			reasonSubstr: "unsupported on provider opencode-go",
		},
		{
			name: "unknown access in root is allowed for exploration",
			req: EligibilityRequest{
				Purpose:   PurposeRoot,
				Family:    FamilyMiMoPro,
				Provider:  "openrouter",
				ModelID:   "xiaomi/mimo-v2.6-pro",
				Transport: TransportChat,
				Access:    AccessUnknown,
			},
			wantEligible: true,
			wantStatus:   AccessUnknown,
			reasonSubstr: "permitted for manual/explicit exploration",
		},
		{
			name: "unknown access in automatic delegation is ineligible",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierLow,
				Family:    FamilyMiMoFlash,
				Provider:  "opencode-go",
				ModelID:   "mimo-v2.6-flash",
				Transport: TransportChat,
				Access:    AccessUnknown,
				IsManual:  false,
			},
			wantEligible: false,
			wantStatus:   AccessUnknown,
			reasonSubstr: "automatic delegation requires confirmed eligibility",
		},
		{
			name: "unknown access in delegation with manual opt-in is eligible",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierLow,
				Family:    FamilyMiMoFlash,
				Provider:  "opencode-go",
				ModelID:   "mimo-v2.6-flash",
				Transport: TransportChat,
				Access:    AccessUnknown,
				IsManual:  true,
			},
			wantEligible: true,
			wantStatus:   AccessUnknown,
		},
		{
			name: "exhausted provider rejected",
			req: EligibilityRequest{
				Purpose:   PurposeDelegated,
				Role:      "worker",
				Tier:      TierLow,
				Family:    FamilyMiMoFlash,
				Provider:  "opencode-go",
				ModelID:   "mimo-v2.6-flash",
				Transport: TransportChat,
				Access:    AccessExhausted,
			},
			wantEligible: false,
			wantStatus:   AccessExhausted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := CheckEligibility(tc.req)
			if res.Eligible != tc.wantEligible {
				t.Fatalf("eligible=%v, want %v (reason: %s)", res.Eligible, tc.wantEligible, res.Reason)
			}
			if res.Status != tc.wantStatus {
				t.Fatalf("status=%v, want %v", res.Status, tc.wantStatus)
			}
			if tc.reasonSubstr != "" && !strings.Contains(res.Reason, tc.reasonSubstr) {
				t.Fatalf("reason=%q, want substring %q", res.Reason, tc.reasonSubstr)
			}
		})
	}
}

func TestDelegatedFallbackChain(t *testing.T) {
	evalAllGranted := FuncAccessEvaluator(func(prov, model string) (AccessStatus, string) {
		return AccessGranted, ""
	})

	// The default chains from config: [routing.tiers] heads linked by [routing.fallback].
	high := []Family{FamilyGPTSol, FamilyMiMoPro, FamilyClaudeSonnet}
	low := []Family{FamilyMiMoFlash, FamilyGPTLuna, FamilyClaudeHaiku, FamilyDeepSeekFlash}
	oracle := []Family{FamilyGPTAstra, FamilyClaudeFable}

	highRoutes := DelegatedFallbackChain(high, "worker", evalAllGranted)
	if len(highRoutes) == 0 {
		t.Fatal("high routes empty")
	}
	// First candidate in High tier must be Codex Sol!
	if highRoutes[0].Family != FamilyGPTSol || highRoutes[0].Provider != "codex" {
		t.Fatalf("high tier #1 should be codex Sol, got: %+v", highRoutes[0])
	}
	// MiMo Pro should follow Sol (OpenRouter -> Go).
	if len(highRoutes) < 3 {
		t.Fatalf("expected at least 3 high routes, got: %d", len(highRoutes))
	}
	if highRoutes[1].Family != FamilyMiMoPro || highRoutes[1].Provider != "openrouter" {
		t.Fatalf("high tier #2 should be openrouter MiMo Pro, got: %+v", highRoutes[1])
	}
	if highRoutes[2].Family != FamilyMiMoPro || highRoutes[2].Provider != "opencode-go" {
		t.Fatalf("high tier #3 should be opencode-go MiMo Pro, got: %+v", highRoutes[2])
	}

	lowRoutes := DelegatedFallbackChain(low, "worker", evalAllGranted)
	if len(lowRoutes) == 0 {
		t.Fatal("low routes empty")
	}
	// MiMo Flash first (Go -> Zen -> OpenRouter).
	if lowRoutes[0].Family != FamilyMiMoFlash || lowRoutes[0].Provider != "opencode-go" {
		t.Fatalf("low tier #1 should be opencode-go MiMo Flash, got: %+v", lowRoutes[0])
	}

	// An empty chain (e.g. the removed medium tier) has no routes.
	if r := DelegatedFallbackChain(nil, "worker", evalAllGranted); r != nil {
		t.Fatalf("empty chain should be nil, got: %+v", r)
	}

	// Oracle tier with oracle role has Astra (Codex) followed by Fable.
	oracleRoutes := DelegatedFallbackChain(oracle, "oracle", evalAllGranted)
	if len(oracleRoutes) == 0 {
		t.Fatal("oracle routes empty")
	}
	if oracleRoutes[0].Family != FamilyGPTAstra || oracleRoutes[0].Provider != "codex" {
		t.Fatalf("oracle tier #1 must be codex Astra, got: %+v", oracleRoutes[0])
	}

	// Oracle tier with non-oracle role drops Astra.
	workerOracleRoutes := DelegatedFallbackChain(oracle, "worker", evalAllGranted)
	for _, r := range workerOracleRoutes {
		if r.Family == FamilyGPTAstra {
			t.Fatalf("worker role must not receive Astra route: %+v", r)
		}
	}
}

func TestCouncilPanelAndCoordinatorFamilies(t *testing.T) {
	panel := CouncilPanelFamilies()
	wantPanel := []Family{
		FamilyGPTLuna,
		FamilyMiMoFlash,
		FamilyDeepSeekFlash,
		FamilyGLMFlash,
		FamilyQwen,
		FamilyClaudeHaiku,
		FamilyGeminiFlash,
	}
	if len(panel) != len(wantPanel) {
		t.Fatalf("council panel count=%d, want %d", len(panel), len(wantPanel))
	}
	for i, f := range wantPanel {
		if panel[i] != f {
			t.Errorf("panel[%d]=%s, want %s", i, panel[i], f)
		}
	}

	coord := CouncilCoordinatorChain()
	wantCoord := []Family{
		FamilyMiMoPro,
		FamilyGPTSol,
		FamilyClaudeSonnet,
	}
	if len(coord) != len(wantCoord) {
		t.Fatalf("council coord count=%d, want %d", len(coord), len(wantCoord))
	}
	for i, f := range wantCoord {
		if coord[i] != f {
			t.Errorf("coord[%d]=%s, want %s", i, coord[i], f)
		}
	}
}

func TestScopedModelsResolution(t *testing.T) {
	dir := t.TempDir()

	// 1. Absent file.
	res := ResolveScopedModels(filepath.Join(dir, "nonexistent.json"), nil)
	if res.State != ScopeStateAbsent {
		t.Fatalf("absent file state=%s, want %s", res.State, ScopeStateAbsent)
	}

	// 2. Empty array file.
	emptyPath := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	resEmpty := ResolveScopedModels(emptyPath, nil)
	if resEmpty.State != ScopeStateEmpty {
		t.Fatalf("empty file state=%s, want %s", resEmpty.State, ScopeStateEmpty)
	}
	if len(resEmpty.Warnings) == 0 || !strings.Contains(resEmpty.Warnings[0], "legacy empty scope is ambiguous") {
		t.Fatalf("expected warning for empty scope, got: %v", resEmpty.Warnings)
	}

	// 3. Unrestricted file.
	unrestPath := filepath.Join(dir, "unrestricted.json")
	if err := os.WriteFile(unrestPath, []byte(`["*"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	resUnrest := ResolveScopedModels(unrestPath, nil)
	if resUnrest.State != ScopeStateUnrestricted {
		t.Fatalf("unrestricted state=%s, want %s", resUnrest.State, ScopeStateUnrestricted)
	}

	// 4. Custom list file.
	customPath := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(customPath, []byte(`["openrouter/xiaomi/mimo-v2.6-pro", "codex/gpt-6.1-sol"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	resCustom := ResolveScopedModels(customPath, nil)
	if resCustom.State != ScopeStateCustom || len(resCustom.Refs) != 2 {
		t.Fatalf("custom state=%s refs=%v", resCustom.State, resCustom.Refs)
	}
}

func TestMigrateConfig(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "config.toml")

	// Create legacy config with tiers.medium and obsolete GLM fallback.
	legacy := `[routing]
pick_model = "table"
[routing.tiers]
low = "mimo-v2.6-flash"
medium = "glm-5.3-flash"
high = "mimo-v2.6-pro"
[routing.fallback]
gpt-luna = "glm-5.3-flash"
`
	if err := os.WriteFile(confPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Routing.Tiers["medium"] = "glm-5.3-flash"
	cfg.Routing.Fallback["gpt-luna"] = "glm-5.3-flash"
	cfg.Routing.PolicyVersion = 0

	warns, err := MigrateConfig(&cfg, confPath)
	if err != nil {
		t.Fatalf("migration err: %v", err)
	}

	// Check warnings emitted.
	hasMedWarn := false
	hasGLMWarn := false
	for _, w := range warns {
		if strings.Contains(w, "legacy routing.tiers.medium") {
			hasMedWarn = true
		}
		if strings.Contains(w, "obsolete GLM fallback") {
			hasGLMWarn = true
		}
	}
	if !hasMedWarn {
		t.Errorf("expected warning for medium tier in %v", warns)
	}
	if !hasGLMWarn {
		t.Errorf("expected warning for GLM fallback in %v", warns)
	}

	// Check backup file exists.
	if _, err := os.Stat(confPath + ".bak"); err != nil {
		t.Errorf("backup file missing: %v", err)
	}

	// Check policy version updated.
	if cfg.Routing.PolicyVersion != CurrentPolicyVersion {
		t.Errorf("policy version = %d, want %d", cfg.Routing.PolicyVersion, CurrentPolicyVersion)
	}

	// Check idempotency: second migration does not overwrite or fail.
	warns2, err := MigrateConfig(&cfg, confPath)
	if err != nil {
		t.Fatalf("second migration failed: %v", err)
	}
	// Still emits warnings if legacy fields are still present in memory.
	_ = warns2
}
