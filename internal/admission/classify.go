package admission

import (
	"strings"
)

// ModelClassification contains the parsed attributes of a provider/model pair.
type ModelClassification struct {
	Provider string
	Model    string
	IsCodex  bool
	IsSol    bool
	IsAstra  bool
	IsLuna   bool
}

// ClassifyModel normalizes provider and model strings and determines their family attributes.
func ClassifyModel(provider, model string) ModelClassification {
	prov := strings.ToLower(strings.TrimSpace(provider))
	mod := strings.ToLower(strings.TrimSpace(model))

	// Allow models specified as "provider/model" ref
	if prov == "" && strings.Contains(mod, "/") {
		parts := strings.SplitN(mod, "/", 2)
		prov = parts[0]
		mod = parts[1]
	} else if strings.Contains(mod, "/") {
		parts := strings.SplitN(mod, "/", 2)
		if prov == "" {
			prov = parts[0]
		}
		mod = parts[1]
	}

	isCodex := prov == "codex"
	isSol := false
	isAstra := false
	isLuna := false

	// Model identification according to PLAN_ROUTING_ORCHESTRATION.md
	// Sol: gpt-6-sol, gpt-6.1-sol, gpt-sol
	// Astra: gpt-6-astra, gpt-astra
	// Luna: gpt-6-luna, gpt-luna
	switch {
	case strings.Contains(mod, "sol"):
		isSol = true
	case strings.Contains(mod, "astra"):
		isAstra = true
	case strings.Contains(mod, "luna"):
		isLuna = true
	}

	return ModelClassification{
		Provider: prov,
		Model:    mod,
		IsCodex:  isCodex,
		IsSol:    isSol,
		IsAstra:  isAstra,
		IsLuna:   isLuna,
	}
}

// ClassifyRequest inspects a request to determine its admission bucket and metering requirement.
func ClassifyRequest(req Request) (bucket BucketKind, isMetered bool, err error) {
	if strings.TrimSpace(req.Model) == "" {
		return BucketUnmetered, false, ErrInvalidModel
	}

	c := ClassifyModel(req.Provider, req.Model)

	// Non-Codex routes do not consume Codex premium quota.
	if !c.IsCodex {
		if req.Exemption == ExemptionCouncilCoordinatorSol {
			return BucketUnmetered, false, ErrInvalidExemption
		}
		return BucketUnmetered, false, nil
	}

	// Codex Luna consumes no premium slot.
	if c.IsLuna {
		if req.Exemption == ExemptionCouncilCoordinatorSol {
			return BucketUnmetered, false, ErrInvalidExemption
		}
		return BucketUnmetered, false, nil
	}

	// Council Sol exemption covers Codex Sol only.
	if req.Exemption == ExemptionCouncilCoordinatorSol {
		if !c.IsSol {
			return BucketUnmetered, false, ErrInvalidExemption
		}
		return BucketCouncilExempt, true, nil
	}

	// Codex Sol and Astra consume the combined premium quota pool.
	if c.IsSol || c.IsAstra {
		return BucketPremiumCombined, true, nil
	}

	// Any other Codex model (if any) is unmetered.
	return BucketUnmetered, false, nil
}
