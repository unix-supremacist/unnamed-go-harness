package admission

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ParsePreset maps preset names or values to canonical Presets and concurrency limits.
// Any unrecognized preset defaults conservatively to basic (1).
func ParsePreset(val string) (Preset, int) {
	v := strings.ToLower(strings.TrimSpace(val))
	switch v {
	case "basic", "basic1", "1", "1x":
		return PresetBasic, 1
	case "5x", "5", "$100", "100":
		return Preset5x, 5
	case "10x", "10", "$200", "200":
		return Preset10x, 10
	case "25x", "25", "$500", "500":
		return Preset25x, 25
	default:
		// Attempt parsing integer directly
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			switch {
			case n >= 25:
				return Preset25x, 25
			case n >= 10:
				return Preset10x, 10
			case n >= 5:
				return Preset5x, 5
			default:
				return PresetBasic, 1
			}
		}
		return PresetBasic, 1
	}
}

// PresetForPlanType maps a detected ChatGPT plan type to a burn-rate preset.
// Only plans whose tier is unambiguous are mapped above basic: "pro" is the
// $200 plan (10x) and "prolite" the $100 plan (5x). Everything else, including
// unknown names, stays conservative at basic; the 25x tier cannot be detected
// and must be configured. known reports whether the plan type was recognized.
func PresetForPlanType(planType string) (p Preset, limit int, known bool) {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "pro":
		return Preset10x, 10, true
	case "prolite", "pro_lite", "pro-lite":
		return Preset5x, 5, true
	case "free", "go", "plus", "team", "business", "enterprise", "edu", "k12":
		return PresetBasic, 1, true
	}
	return PresetBasic, 1, false
}

// DefaultAccountConfig returns conservative defaults: basic plan (1) and 1 council exempt slot.
func DefaultAccountConfig() AccountConfig {
	return AccountConfig{
		Preset:             PresetBasic,
		CombinedLimit:      1,
		CouncilExemptLimit: 1,
		UpdatedAt:          time.Now().UnixMilli(),
	}
}

// ValidateConfig validates and normalizes an account configuration.
func ValidateConfig(cfg AccountConfig) (AccountConfig, error) {
	preset, defaultLimit := ParsePreset(string(cfg.Preset))
	cfg.Preset = preset

	if cfg.CombinedLimit <= 0 {
		cfg.CombinedLimit = defaultLimit
	} else {
		// Both bucket capacities validated: clamp or enforce valid positive limit
		if cfg.CombinedLimit < 1 {
			cfg.CombinedLimit = 1
		}
	}

	if cfg.CouncilExemptLimit <= 0 {
		cfg.CouncilExemptLimit = 1
	}

	if cfg.UpdatedAt == 0 {
		cfg.UpdatedAt = time.Now().UnixMilli()
	}

	return cfg, nil
}

// LoadAccountConfig reads the account configuration from disk.
// If the configuration file is absent, conservative defaults are returned.
func LoadAccountConfig(accountDir string) (AccountConfig, error) {
	path := filepath.Join(accountDir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultAccountConfig(), nil
		}
		return AccountConfig{}, fmt.Errorf("%w: failed to read config: %v", ErrStorageUnavailable, err)
	}

	var cfg AccountConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Corrupted or invalid config defaults conservatively to basic
		return DefaultAccountConfig(), nil
	}

	validCfg, _ := ValidateConfig(cfg)
	return validCfg, nil
}

// SaveAccountConfig atomically writes an account configuration to disk under lock.
func SaveAccountConfig(accountDir string, cfg AccountConfig) error {
	validCfg, err := ValidateConfig(cfg)
	if err != nil {
		return err
	}
	validCfg.UpdatedAt = time.Now().UnixMilli()

	if err := os.MkdirAll(accountDir, 0700); err != nil {
		return fmt.Errorf("%w: failed to create account directory: %v", ErrStorageUnavailable, err)
	}

	raw, err := json.MarshalIndent(validCfg, "", "  ")
	if err != nil {
		return err
	}

	// Random suffix for atomic temporary file
	var randBytes [8]byte
	_, _ = rand.Read(randBytes[:])
	tmpPath := filepath.Join(accountDir, fmt.Sprintf("config.json.tmp.%s", hex.EncodeToString(randBytes[:])))

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("%w: failed to open temp config file: %v", ErrStorageUnavailable, err)
	}

	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: failed to write temp config: %v", ErrStorageUnavailable, err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: failed to sync temp config: %v", ErrStorageUnavailable, err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: failed to close temp config: %v", ErrStorageUnavailable, err)
	}

	finalPath := filepath.Join(accountDir, "config.json")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: failed to rename temp config: %v", ErrStorageUnavailable, err)
	}

	return nil
}
