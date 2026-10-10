package admission

import (
	"errors"
	"io"
	"time"
)

var (
	// ErrCapacityExceeded is returned when the premium slot limit is reached
	// and the request cannot be admitted (e.g. nonblocking child fallback).
	ErrCapacityExceeded = errors.New("admission: premium capacity exceeded")

	// ErrStorageUnavailable is returned when storage paths or lock files cannot
	// be accessed or created (fail-closed).
	ErrStorageUnavailable = errors.New("admission: storage unavailable")

	// ErrInvalidExemption is returned when a council coordinator exemption is
	// requested for a model or provider other than Codex Sol.
	ErrInvalidExemption = errors.New("admission: council coordinator exemption valid only for Codex Sol")

	// ErrInvalidModel is returned when a requested model is unrecognized or empty.
	ErrInvalidModel = errors.New("admission: invalid model")
)

// Priority defines request admission priority.
type Priority int

const (
	// PriorityChild is ordinary delegated / subagent work.
	// It performs nonblocking capacity fallback when quota is full or a root waiter is registered.
	PriorityChild Priority = iota

	// PriorityRoot is interactive user turns or root orchestrator turns.
	// It registers as a waiter for priority on the next available slot when quota is full.
	PriorityRoot
)

func (p Priority) String() string {
	switch p {
	case PriorityRoot:
		return "root"
	default:
		return "child"
	}
}

// Exemption represents narrowly typed exemption authority for quota buckets.
// This type is minted only in harness control flow, never parsed from model JSON strings.
type Exemption int

const (
	// ExemptionNone represents standard metered admission.
	ExemptionNone Exemption = iota

	// ExemptionCouncilCoordinatorSol covers bounded council coordinator preparation,
	// angle generation, and synthesis turns on Codex Sol.
	ExemptionCouncilCoordinatorSol
)

func (e Exemption) String() string {
	switch e {
	case ExemptionCouncilCoordinatorSol:
		return "council_coordinator_sol"
	default:
		return "none"
	}
}

// BucketKind identifies the admission bucket used for a request.
type BucketKind string

const (
	// BucketUnmetered is for models that require no premium slot (non-Codex, Codex Luna).
	BucketUnmetered BucketKind = "unmetered"

	// BucketPremiumCombined is the combined Codex Sol/Astra quota pool.
	BucketPremiumCombined BucketKind = "premium_combined"

	// BucketCouncilExempt is the dedicated, separate pool for Council Coordinator Sol turns.
	BucketCouncilExempt BucketKind = "council_exempt"
)

// Preset represents the user-requested plan preset.
type Preset string

const (
	PresetBasic Preset = "basic"
	Preset5x    Preset = "5x"
	Preset10x   Preset = "10x"
	Preset25x   Preset = "25x"
)

// AccountConfig defines account-scoped quota limits stored atomically on disk.
type AccountConfig struct {
	Preset             Preset `json:"preset"`
	CombinedLimit      int    `json:"combined_limit"`
	CouncilExemptLimit int    `json:"council_exempt_limit"`
	UpdatedAt          int64  `json:"updated_at_unix_ms"`
}

// Request describes a single call to be admitted at the common request boundary.
type Request struct {
	AccountID string    `json:"account_id,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model"`
	Priority  Priority  `json:"priority"`
	Exemption Exemption `json:"exemption"`
	RequestID string    `json:"request_id,omitempty"`
	Purpose   string    `json:"purpose,omitempty"`
}

// SlotDiagnostics records metadata for an active slot holder.
type SlotDiagnostics struct {
	SlotIndex  int       `json:"slot_index"`
	PID        int       `json:"pid"`
	RequestID  string    `json:"request_id,omitempty"`
	Purpose    string    `json:"purpose,omitempty"`
	Model      string    `json:"model,omitempty"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// WaiterDiagnostics records metadata for a registered root waiter.
type WaiterDiagnostics struct {
	WaiterID    string    `json:"waiter_id"`
	PID         int       `json:"pid"`
	RequestID   string    `json:"request_id,omitempty"`
	Purpose     string    `json:"purpose,omitempty"`
	Model       string    `json:"model,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
}

// Diagnostics contains real-time quota state for an account.
type Diagnostics struct {
	AccountID          string              `json:"account_id"`
	IsFallbackBucket   bool                `json:"is_fallback_bucket"`
	Preset             Preset              `json:"preset"`
	CombinedLimit      int                 `json:"combined_limit"`
	CouncilExemptLimit int                 `json:"council_exempt_limit"`
	ActiveCombined     []SlotDiagnostics   `json:"active_combined"`
	ActiveExempt       []SlotDiagnostics   `json:"active_exempt"`
	Waiters            []WaiterDiagnostics `json:"waiters"`
	StorageDir         string              `json:"storage_dir"`
}

// Handle represents an acquired admission lease.
// Close releases the lock only after request execution and caller cleanup are complete.
type Handle interface {
	io.Closer
	Bucket() BucketKind
	Slot() int
	AccountID() string
	IsExempt() bool
	IsMetered() bool
	Diagnostics() SlotDiagnostics
}
