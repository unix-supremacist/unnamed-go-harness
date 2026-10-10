package policy

import (
	"fmt"
)

// Purpose defines the trusted execution intent.
type Purpose string

const (
	PurposeRoot         Purpose = "root"
	PurposeDelegated    Purpose = "delegated"
	PurposeCouncilPanel Purpose = "council_panel"
	PurposeCouncilCoord Purpose = "council_coord"
	PurposeCouncilSynth Purpose = "council_synth"
)

// Tier defines the ordinary delegation tiers and oracle workflow tier.
// Note: legacy "medium" tier is removed.
type Tier string

const (
	TierLow    Tier = "low"
	TierHigh   Tier = "high"
	TierOracle Tier = "oracle"
)

// Family identifies a model family across providers.
type Family string

const (
	FamilyMiMoPro       Family = "mimo-v2.6-pro"
	FamilyMiMoFlash     Family = "mimo-v2.6-flash"
	FamilyGPTSol        Family = "gpt-sol"
	FamilyGPTAstra      Family = "gpt-astra"
	FamilyGPTLuna       Family = "gpt-luna"
	FamilyClaudeHaiku   Family = "claude-haiku"
	FamilyClaudeSonnet  Family = "claude-sonnet"
	FamilyClaudeOpus    Family = "claude-opus"
	FamilyClaudeFable   Family = "claude-fable"
	FamilyDeepSeekFlash Family = "deepseek-flash"
	FamilyGLMFlash      Family = "glm-flash"
	FamilyQwen          Family = "qwen"
	FamilyGeminiFlash   Family = "gemini-flash"
)

// Transport identifies the protocol transport needed by a route.
type Transport string

const (
	TransportChat      Transport = "chat"
	TransportResponses Transport = "responses"
)

// AccessStatus distinguishes confirmed entitlement, unknown authorization,
// absence, explicit overrides, and transient restrictions.
type AccessStatus string

const (
	AccessGranted      AccessStatus = "granted"       // Confirmed entitlement or explicit user override
	AccessDenied       AccessStatus = "denied"        // Confirmed rejected (401/403 or explicit user denial)
	AccessUnknown      AccessStatus = "unknown"       // Credential exists, but access is not confirmed
	AccessNoCredential AccessStatus = "no_credential" // Credential absent
	AccessUnsupported  AccessStatus = "unsupported"   // Transport or endpoint not supported by client
	AccessExhausted    AccessStatus = "exhausted"     // Temporary quota limit reached
	AccessReserved     AccessStatus = "reserved"      // Usage reserve threshold crossed
)

// Route is a concrete provider endpoint model reference.
type Route struct {
	Family    Family    `json:"family"`
	Provider  string    `json:"provider"`
	ModelID   string    `json:"model_id"`
	Transport Transport `json:"transport"`
}

// Ref returns the provider/model ref string.
func (r Route) Ref() string {
	return r.Provider + "/" + r.ModelID
}

// RouteKey returns the provider:model format used in card routes.
func (r Route) RouteKey() string {
	return r.Provider + ":" + r.ModelID
}

// FamilyInfo defines the policy rules for one model family.
type FamilyInfo struct {
	Family             Family
	DisplayName        string
	CardID             string
	DelegatedTier      Tier     // Low, High, Oracle, or empty if not delegable
	ProviderOrder      []string // Provider preference order
	RootProviders      []string // Providers in default root scope
	CouncilPanel       bool     // Eligible for council panel seats
	CouncilCoord       bool     // Eligible for council coordinator
	CouncilSynth       bool     // Eligible for exceptional council synthesis
	CouncilOnly        bool     // Exclusively for council
	RootOnly           bool     // Opus etc: root-only, not delegable
	CodexOnlyDelegated bool     // Sol, Astra: delegated calls Codex only
	OracleOnly         bool     // Astra: reserved for role oracle
}

// Families contains the authoritative policy configuration for each family.
var Families = map[Family]FamilyInfo{
	FamilyMiMoPro: {
		Family:        FamilyMiMoPro,
		DisplayName:   "MiMo 2.6 Pro",
		CardID:        "mimo-v2.6-pro",
		DelegatedTier: TierHigh,
		ProviderOrder: []string{"openrouter", "opencode-go"},
		RootProviders: []string{"openrouter", "opencode-go"},
		CouncilCoord:  true,
	},
	FamilyMiMoFlash: {
		Family:        FamilyMiMoFlash,
		DisplayName:   "MiMo 2.6 Flash",
		CardID:        "mimo-v2.6-flash",
		DelegatedTier: TierLow,
		ProviderOrder: []string{"opencode-go", "opencode-zen", "openrouter"},
		RootProviders: []string{"openrouter", "opencode-go", "opencode-zen"},
		CouncilPanel:  true,
	},
	FamilyGPTSol: {
		Family:             FamilyGPTSol,
		DisplayName:        "GPT-6.1 Sol",
		CardID:             "gpt-sol",
		DelegatedTier:      TierHigh,
		ProviderOrder:      []string{"codex"},
		RootProviders:      []string{"codex", "openrouter", "opencode-zen"},
		CouncilCoord:       true,
		CodexOnlyDelegated: true,
	},
	FamilyGPTAstra: {
		Family:             FamilyGPTAstra,
		DisplayName:        "GPT-6 Astra",
		CardID:             "gpt-astra",
		DelegatedTier:      TierOracle,
		ProviderOrder:      []string{"codex"},
		RootProviders:      []string{"codex", "openrouter", "opencode-zen"},
		CouncilSynth:       true,
		CodexOnlyDelegated: true,
		OracleOnly:         true,
	},
	FamilyGPTLuna: {
		Family:        FamilyGPTLuna,
		DisplayName:   "GPT-6 Luna",
		CardID:        "gpt-luna",
		DelegatedTier: TierLow,
		ProviderOrder: []string{"codex", "openrouter", "opencode-zen", "opencode-go"},
		RootProviders: []string{"codex", "openrouter", "opencode-zen", "opencode-go"},
		CouncilPanel:  true,
		CouncilSynth:  true,
	},
	FamilyClaudeHaiku: {
		Family:        FamilyClaudeHaiku,
		DisplayName:   "Claude Haiku 5.5",
		CardID:        "claude-haiku-5.5",
		DelegatedTier: TierLow,
		ProviderOrder: []string{"openrouter", "opencode-zen", "opencode-go"},
		RootProviders: []string{"openrouter", "opencode-go", "opencode-zen"},
		CouncilPanel:  true,
		CouncilSynth:  true,
	},
	FamilyClaudeSonnet: {
		Family:        FamilyClaudeSonnet,
		DisplayName:   "Claude Sonnet 5.5",
		CardID:        "claude-sonnet-5.5",
		DelegatedTier: TierHigh,
		ProviderOrder: []string{"openrouter", "opencode-zen", "opencode-go"},
		RootProviders: []string{"openrouter", "opencode-zen"},
		CouncilCoord:  true,
	},
	FamilyClaudeOpus: {
		Family:        FamilyClaudeOpus,
		DisplayName:   "Claude Opus 5.5",
		CardID:        "claude-opus-5.5",
		RootOnly:      true,
		ProviderOrder: []string{"openrouter", "opencode-zen"},
		RootProviders: []string{"openrouter", "opencode-zen"},
	},
	FamilyClaudeFable: {
		Family:        FamilyClaudeFable,
		DisplayName:   "Claude Fable 5.1",
		CardID:        "claude-fable-5.1",
		DelegatedTier: TierOracle,
		ProviderOrder: []string{"openrouter", "opencode-zen", "opencode-go"},
		RootProviders: []string{"openrouter", "opencode-zen"},
		CouncilSynth:  true,
	},
	FamilyDeepSeekFlash: {
		Family:        FamilyDeepSeekFlash,
		DisplayName:   "DeepSeek v4.1 Flash",
		CardID:        "deepseek-v4.1-flash",
		DelegatedTier: TierLow,
		ProviderOrder: []string{"opencode-go", "opencode-zen", "openrouter"},
		CouncilPanel:  true,
		CouncilSynth:  true,
	},
	FamilyGLMFlash: {
		Family:        FamilyGLMFlash,
		DisplayName:   "GLM 5.3 Flash",
		CardID:        "glm-5.3-flash",
		CouncilOnly:   true,
		ProviderOrder: []string{"opencode-go", "opencode-zen", "openrouter"},
		CouncilPanel:  true,
	},
	FamilyQwen: {
		Family:        FamilyQwen,
		DisplayName:   "Qwen 3.8 Flash",
		CardID:        "qwen3.8-flash",
		CouncilOnly:   true,
		ProviderOrder: []string{"openrouter", "opencode-zen", "opencode-go"},
		CouncilPanel:  true,
	},
	FamilyGeminiFlash: {
		Family:        FamilyGeminiFlash,
		DisplayName:   "Gemini 3.8 Flash",
		CardID:        "gemini-3.8-flash",
		CouncilOnly:   true,
		ProviderOrder: []string{"openrouter", "opencode-zen"}, // never Go
		CouncilPanel:  true,
	},
}

// ConcreteRoutes maps (family, provider) to Route details.
var ConcreteRoutes = map[Family]map[string]Route{
	FamilyMiMoPro: {
		"openrouter":  {Family: FamilyMiMoPro, Provider: "openrouter", ModelID: "xiaomi/mimo-v2.6-pro", Transport: TransportChat},
		"opencode-go": {Family: FamilyMiMoPro, Provider: "opencode-go", ModelID: "mimo-v2.6-pro", Transport: TransportChat},
	},
	FamilyMiMoFlash: {
		"openrouter":   {Family: FamilyMiMoFlash, Provider: "openrouter", ModelID: "xiaomi/mimo-v2.6-flash", Transport: TransportChat},
		"opencode-go":  {Family: FamilyMiMoFlash, Provider: "opencode-go", ModelID: "mimo-v2.6-flash", Transport: TransportChat},
		"opencode-zen": {Family: FamilyMiMoFlash, Provider: "opencode-zen", ModelID: "xiaomi/mimo-v2.6-flash", Transport: TransportChat},
	},
	FamilyGPTSol: {
		"codex":        {Family: FamilyGPTSol, Provider: "codex", ModelID: "gpt-6.1-sol", Transport: TransportResponses},
		"openrouter":   {Family: FamilyGPTSol, Provider: "openrouter", ModelID: "openai/gpt-6.1-sol", Transport: TransportChat},
		"opencode-zen": {Family: FamilyGPTSol, Provider: "opencode-zen", ModelID: "openai/gpt-6.1-sol", Transport: TransportChat},
	},
	FamilyGPTAstra: {
		"codex":        {Family: FamilyGPTAstra, Provider: "codex", ModelID: "gpt-6-astra", Transport: TransportResponses},
		"openrouter":   {Family: FamilyGPTAstra, Provider: "openrouter", ModelID: "openai/gpt-6-astra", Transport: TransportChat},
		"opencode-zen": {Family: FamilyGPTAstra, Provider: "opencode-zen", ModelID: "openai/gpt-6-astra", Transport: TransportChat},
	},
	FamilyGPTLuna: {
		"codex":        {Family: FamilyGPTLuna, Provider: "codex", ModelID: "gpt-6-luna", Transport: TransportResponses},
		"openrouter":   {Family: FamilyGPTLuna, Provider: "openrouter", ModelID: "openai/gpt-6-luna", Transport: TransportChat},
		"opencode-zen": {Family: FamilyGPTLuna, Provider: "opencode-zen", ModelID: "openai/gpt-6-luna", Transport: TransportChat},
		"opencode-go":  {Family: FamilyGPTLuna, Provider: "opencode-go", ModelID: "gpt-6-luna", Transport: TransportResponses}, // Responses required; chat unsupported
	},
	FamilyClaudeHaiku: {
		"openrouter":   {Family: FamilyClaudeHaiku, Provider: "openrouter", ModelID: "anthropic/claude-haiku-5.5", Transport: TransportChat},
		"opencode-go":  {Family: FamilyClaudeHaiku, Provider: "opencode-go", ModelID: "claude-haiku-5.5", Transport: TransportChat},
		"opencode-zen": {Family: FamilyClaudeHaiku, Provider: "opencode-zen", ModelID: "claude-haiku-5.5", Transport: TransportChat},
	},
	FamilyClaudeSonnet: {
		"openrouter":   {Family: FamilyClaudeSonnet, Provider: "openrouter", ModelID: "anthropic/claude-sonnet-5.5", Transport: TransportChat},
		"opencode-zen": {Family: FamilyClaudeSonnet, Provider: "opencode-zen", ModelID: "claude-sonnet-5.5", Transport: TransportChat},
		"opencode-go":  {Family: FamilyClaudeSonnet, Provider: "opencode-go", ModelID: "claude-sonnet-5.5", Transport: TransportChat},
	},
	FamilyClaudeOpus: {
		"openrouter":   {Family: FamilyClaudeOpus, Provider: "openrouter", ModelID: "anthropic/claude-opus-5.5", Transport: TransportChat},
		"opencode-zen": {Family: FamilyClaudeOpus, Provider: "opencode-zen", ModelID: "claude-opus-5.5", Transport: TransportChat},
	},
	FamilyClaudeFable: {
		"openrouter":   {Family: FamilyClaudeFable, Provider: "openrouter", ModelID: "anthropic/claude-fable-5.1", Transport: TransportChat},
		"opencode-zen": {Family: FamilyClaudeFable, Provider: "opencode-zen", ModelID: "claude-fable-5.1", Transport: TransportChat},
		"opencode-go":  {Family: FamilyClaudeFable, Provider: "opencode-go", ModelID: "claude-fable-5.1", Transport: TransportChat},
	},
	FamilyDeepSeekFlash: {
		"opencode-go":  {Family: FamilyDeepSeekFlash, Provider: "opencode-go", ModelID: "deepseek-v4.1-flash", Transport: TransportChat},
		"opencode-zen": {Family: FamilyDeepSeekFlash, Provider: "opencode-zen", ModelID: "deepseek-v4.1-flash", Transport: TransportChat},
		"openrouter":   {Family: FamilyDeepSeekFlash, Provider: "openrouter", ModelID: "deepseek/deepseek-v4.1-flash", Transport: TransportChat},
	},
	FamilyGLMFlash: {
		"opencode-go":  {Family: FamilyGLMFlash, Provider: "opencode-go", ModelID: "glm-5.3-flash", Transport: TransportChat},
		"opencode-zen": {Family: FamilyGLMFlash, Provider: "opencode-zen", ModelID: "glm-5.3-flash", Transport: TransportChat},
		"openrouter":   {Family: FamilyGLMFlash, Provider: "openrouter", ModelID: "zhipu/glm-5.3-flash", Transport: TransportChat},
	},
	FamilyQwen: {
		"openrouter":   {Family: FamilyQwen, Provider: "openrouter", ModelID: "qwen/qwen3.8-flash", Transport: TransportChat},
		"opencode-zen": {Family: FamilyQwen, Provider: "opencode-zen", ModelID: "qwen3.8-flash", Transport: TransportChat},
		"opencode-go":  {Family: FamilyQwen, Provider: "opencode-go", ModelID: "qwen3.8-flash", Transport: TransportChat},
	},
	FamilyGeminiFlash: {
		"openrouter":   {Family: FamilyGeminiFlash, Provider: "openrouter", ModelID: "google/gemini-3.8-flash", Transport: TransportChat},
		"opencode-zen": {Family: FamilyGeminiFlash, Provider: "opencode-zen", ModelID: "gemini-3.8-flash", Transport: TransportChat},
	},
}

// ProviderSupportedTransport reports whether the harness currently supports
// the given transport on the specified provider.
func ProviderSupportedTransport(provider string, transport Transport) bool {
	switch provider {
	case "codex":
		return transport == TransportResponses
	case "opencode-go":
		// OpenCode Go chat client only supports Chat Completions today.
		// Responses protocol is unsupported.
		return transport == TransportChat
	case "openrouter", "opencode-zen":
		return transport == TransportChat
	default:
		// Custom OpenAI-compatible providers default to Chat Completions.
		return transport == TransportChat
	}
}

// EligibilityRequest is the input to the shared eligibility predicate.
type EligibilityRequest struct {
	Purpose      Purpose      `json:"-"` // trusted executor purpose, not model arguments
	Role         string       `json:"-"` // trusted executor role
	Tier         Tier         `json:"tier"`
	Family       Family       `json:"family"`
	Provider     string       `json:"provider"`
	ModelID      string       `json:"model_id"`
	Transport    Transport    `json:"-"` // resolved adapter capability, not model arguments
	Access       AccessStatus `json:"-"` // local evidence or explicit user override
	AccessReason string       `json:"-"`
	IsManual     bool         `json:"-"` // manual selection / user override
	// Authority is supplied by harness control flow, never decoded model JSON.
	OracleLed        bool `json:"-"`
	ApprovalRequired bool `json:"-"`
	Approved         bool `json:"-"`
}

// EligibilityResult is the outcome of the eligibility predicate.
type EligibilityResult struct {
	Eligible bool         `json:"eligible"`
	Status   AccessStatus `json:"status"`
	Reason   string       `json:"reason"`
}

// CheckEligibility is the shared policy contract. Execution callers must supply
// trusted context and require task-scoped approval when applicable; a candidate
// list or eligibility result does not itself authorize execution.
func CheckEligibility(req EligibilityRequest) EligibilityResult {
	switch req.Purpose {
	case PurposeRoot, PurposeDelegated, PurposeCouncilPanel, PurposeCouncilCoord, PurposeCouncilSynth:
	default:
		return EligibilityResult{Status: AccessDenied, Reason: "unknown execution purpose"}
	}
	if req.ApprovalRequired && !req.Approved {
		return EligibilityResult{Status: AccessDenied, Reason: "task-scoped approval is required"}
	}
	info, hasInfo := Families[req.Family]
	if !hasInfo {
		return EligibilityResult{
			Eligible: false,
			Status:   AccessDenied,
			Reason:   fmt.Sprintf("unknown model family %q", req.Family),
		}
	}

	if req.Purpose != PurposeRoot && info.CodexOnlyDelegated && req.Provider != "codex" {
		return EligibilityResult{Status: AccessDenied, Reason: "delegated Sol/Astra requests use Codex only"}
	}
	if info.OracleOnly && (req.Purpose == PurposeCouncilSynth || req.Purpose == PurposeCouncilCoord) && !req.OracleLed {
		return EligibilityResult{Status: AccessDenied, Reason: "Astra coordination/synthesis requires an authorized oracle-led council"}
	}

	// Rule 1: Transport support.
	if !ProviderSupportedTransport(req.Provider, req.Transport) {
		return EligibilityResult{
			Eligible: false,
			Status:   AccessUnsupported,
			Reason:   fmt.Sprintf("transport %s is unsupported on provider %s", req.Transport, req.Provider),
		}
	}

	// Rule 2: Hard negative constraints by provider / family.
	if req.Family == FamilyGeminiFlash && req.Provider == "opencode-go" {
		return EligibilityResult{
			Eligible: false,
			Status:   AccessDenied,
			Reason:   "Gemini must never route through OpenCode Go",
		}
	}

	// Rule 3: Purpose-specific constraints.
	switch req.Purpose {
	case PurposeRoot:
		// Root choices may include non-Codex Sol/Astra and Opus.
		// Verify provider is in the root set for this family (or manual override).
		if !req.IsManual && !contains(info.RootProviders, req.Provider) {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("provider %s is not in default root scope for %s", req.Provider, req.Family),
			}
		}

	case PurposeDelegated:
		// Root-only families cannot be delegated.
		if info.RootOnly {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s is root-only and not available for delegation", info.DisplayName),
			}
		}
		// Council-only families cannot be ordinarily delegated.
		if info.CouncilOnly {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s is council-only and not available for ordinary delegation", info.DisplayName),
			}
		}
		// Sol and Astra delegated calls use Codex only.
		if info.CodexOnlyDelegated && req.Provider != "codex" {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s delegated calls must use Codex only (got %s)", info.DisplayName, req.Provider),
			}
		}
		// Astra is oracle-only.
		if info.OracleOnly && req.Role != "oracle" {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s is reserved for role oracle", info.DisplayName),
			}
		}
		// Tier validation if specified.
		if req.Tier != "" && info.DelegatedTier != req.Tier {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s belongs to tier %s, not requested tier %s", info.DisplayName, info.DelegatedTier, req.Tier),
			}
		}

	case PurposeCouncilPanel:
		if !info.CouncilPanel {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s is not an eligible council panel family", info.DisplayName),
			}
		}

	case PurposeCouncilCoord:
		if !info.CouncilCoord && !(info.OracleOnly && req.OracleLed) {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s is not in the normal council coordinator chain", info.DisplayName),
			}
		}

	case PurposeCouncilSynth:
		if !info.CouncilSynth && !info.CouncilCoord {
			return EligibilityResult{
				Eligible: false,
				Status:   AccessDenied,
				Reason:   fmt.Sprintf("%s is not an eligible council synthesizer", info.DisplayName),
			}
		}
	}

	// Rule 4: Access evaluations.
	switch req.Access {
	case AccessDenied:
		return EligibilityResult{
			Eligible: false,
			Status:   AccessDenied,
			Reason:   fallback(req.AccessReason, "access confirmed denied"),
		}
	case AccessNoCredential:
		return EligibilityResult{
			Eligible: false,
			Status:   AccessNoCredential,
			Reason:   fallback(req.AccessReason, fmt.Sprintf("no credential for provider %s", req.Provider)),
		}
	case AccessExhausted:
		return EligibilityResult{
			Eligible: false,
			Status:   AccessExhausted,
			Reason:   fallback(req.AccessReason, fmt.Sprintf("provider %s is exhausted", req.Provider)),
		}
	case AccessReserved:
		return EligibilityResult{
			Eligible: false,
			Status:   AccessReserved,
			Reason:   fallback(req.AccessReason, fmt.Sprintf("provider %s reached usage reserve", req.Provider)),
		}
	case AccessUnsupported:
		return EligibilityResult{
			Eligible: false,
			Status:   AccessUnsupported,
			Reason:   fallback(req.AccessReason, fmt.Sprintf("route %s/%s unsupported", req.Provider, req.ModelID)),
		}
	case AccessUnknown:
		// Explicit user configuration or manual selection can authorize trying an unknown route.
		// Automatic delegation requires confirmed eligibility or explicit opt-in.
		if req.Purpose == PurposeRoot || req.IsManual {
			return EligibilityResult{
				Eligible: true,
				Status:   AccessUnknown,
				Reason:   "access unknown; permitted for manual/explicit exploration",
			}
		}
		return EligibilityResult{
			Eligible: false,
			Status:   AccessUnknown,
			Reason:   "automatic delegation requires confirmed eligibility; access is unknown",
		}
	case AccessGranted:
		return EligibilityResult{
			Eligible: true,
			Status:   AccessGranted,
			Reason:   "eligible",
		}
	default:
		return EligibilityResult{
			Eligible: false,
			Status:   AccessUnknown,
			Reason:   "unknown access state",
		}
	}
}

// AccessEvaluator evaluates the access status for a given provider and route.
type AccessEvaluator interface {
	CheckAccess(provider, modelID string) (AccessStatus, string)
}

// FuncAccessEvaluator adapts a simple function into AccessEvaluator.
type FuncAccessEvaluator func(provider, modelID string) (AccessStatus, string)

func (f FuncAccessEvaluator) CheckAccess(provider, modelID string) (AccessStatus, string) {
	return f(provider, modelID)
}

// DefaultRootScope returns the default root model refs filtered by eligibility.
func DefaultRootScope(eval AccessEvaluator) []string {
	var out []string
	seen := map[string]bool{}

	// Built-in provider order for root scope discovery.
	providers := []string{"openrouter", "opencode-go", "opencode-zen", "codex"}

	for _, p := range providers {
		for _, fam := range rootFamilyOrder() {
			info := Families[fam]
			if !contains(info.RootProviders, p) {
				continue
			}
			routes, ok := ConcreteRoutes[fam]
			if !ok {
				continue
			}
			r, ok := routes[p]
			if !ok {
				continue
			}
			st, reason := AccessUnknown, ""
			if eval != nil {
				st, reason = eval.CheckAccess(p, r.ModelID)
			}
			res := CheckEligibility(EligibilityRequest{
				Purpose:      PurposeRoot,
				Family:       fam,
				Provider:     p,
				ModelID:      r.ModelID,
				Transport:    r.Transport,
				Access:       st,
				AccessReason: reason,
				IsManual:     false,
			})
			if res.Eligible {
				ref := r.Ref()
				if !seen[ref] {
					seen[ref] = true
					out = append(out, ref)
				}
			}
		}
	}
	return out
}

// rootFamilyOrder returns the deterministic order of families in root scope.
func rootFamilyOrder() []Family {
	return []Family{
		FamilyMiMoPro,
		FamilyMiMoFlash,
		FamilyGPTSol,
		FamilyGPTAstra,
		FamilyGPTLuna,
		FamilyClaudeHaiku,
		FamilyClaudeSonnet,
		FamilyClaudeOpus,
		FamilyClaudeFable,
	}
}

// FamilyByID maps a card id or family name (e.g. "gpt-sol", "claude-haiku-5.5") to its family.
func FamilyByID(id string) (Family, bool) {
	for f, info := range Families {
		if id == string(f) || id == info.CardID {
			return f, true
		}
	}
	return "", false
}

// DelegatedFallbackChain returns the eligible routes for a delegated chain of
// families (configured per tier): each family's providers in preference order.
func DelegatedFallbackChain(fams []Family, role string, eval AccessEvaluator) []Route {
	var candidates []Route
	for _, fam := range fams {
		info := Families[fam]
		for _, p := range info.ProviderOrder {
			r, ok := ConcreteRoutes[fam][p]
			if !ok {
				continue
			}
			st, reason := AccessUnknown, ""
			if eval != nil {
				st, reason = eval.CheckAccess(p, r.ModelID)
			}
			res := CheckEligibility(EligibilityRequest{
				Purpose:      PurposeDelegated,
				Role:         role,
				Family:       fam,
				Provider:     p,
				ModelID:      r.ModelID,
				Transport:    r.Transport,
				Access:       st,
				AccessReason: reason,
			})
			if res.Eligible {
				candidates = append(candidates, r)
			}
		}
	}
	return candidates
}

// CouncilPanelFamilies returns the authoritative panel families for council.
func CouncilPanelFamilies() []Family {
	return []Family{
		FamilyGPTLuna,
		FamilyMiMoFlash,
		FamilyDeepSeekFlash,
		FamilyGLMFlash,
		FamilyQwen,
		FamilyClaudeHaiku,
		FamilyGeminiFlash,
	}
}

// CouncilCoordinatorChain returns the authoritative coordinator family order.
func CouncilCoordinatorChain() []Family {
	return []Family{
		FamilyMiMoPro,
		FamilyGPTSol,
		FamilyClaudeSonnet,
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func fallback(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
