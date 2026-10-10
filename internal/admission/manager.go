package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var safeAccountRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// sanitizeAccountKey produces a safe directory component for an account ID.
func sanitizeAccountKey(accountID string) (key string, isFallback bool) {
	trimmed := strings.TrimSpace(accountID)
	if trimmed == "" || strings.EqualFold(trimmed, "unknown") || strings.EqualFold(trimmed, "shared_unknown") {
		return "shared_unknown", true
	}
	if safeAccountRe.MatchString(trimmed) {
		return "acct_" + trimmed, false
	}
	// For longer or special-character IDs, hash securely
	h := sha256.Sum256([]byte(trimmed))
	return "acct_hash_" + hex.EncodeToString(h[:16]), false
}

// Manager coordinates cross-process request admission.
type Manager struct {
	baseDir string
}

// Option configures Manager.
type Option func(*Manager)

// WithBaseDir sets a custom storage root directory.
func WithBaseDir(dir string) Option {
	return func(m *Manager) {
		m.baseDir = dir
	}
}

// DefaultBaseDir determines the default runtime admission directory.
// It ensures that differing XDG_RUNTIME_DIR or TMPDIR does not split the quota pool
// across processes for the same UID on the machine.
func DefaultBaseDir() string {
	if env := os.Getenv("UGH_ADMISSION_DIR"); env != "" && filepath.IsAbs(env) {
		return env
	}
	uid := os.Getuid()
	runUser := fmt.Sprintf("/run/user/%d", uid)
	if fi, err := os.Stat(runUser); err == nil && fi.IsDir() {
		return filepath.Join(runUser, "ugh", "admission")
	}
	return filepath.Join(fmt.Sprintf("/tmp/ugh-%d", uid), "ugh", "admission")
}

// NewManager creates a new admission Manager.
func NewManager(opts ...Option) *Manager {
	m := &Manager{
		baseDir: DefaultBaseDir(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

var (
	defaultMgrOnce sync.Once
	defaultMgr     *Manager
)

// DefaultManager returns the package-level default admission manager.
func DefaultManager() *Manager {
	defaultMgrOnce.Do(func() {
		defaultMgr = NewManager()
	})
	return defaultMgr
}

func (m *Manager) accountDir(accountID string) (dir string, isFallback bool, err error) {
	key, isFallback := sanitizeAccountKey(accountID)
	dir = filepath.Join(m.baseDir, "accounts", key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", false, fmt.Errorf("%w: cannot create account directory: %v", ErrStorageUnavailable, err)
	}
	return dir, isFallback, nil
}

// scanSlotsLocked inspects slot descriptor files in slotsDir under admin.lock.
// It reports held slot indices, the lowest free slot index (< capacity), and active diagnostics.
func (m *Manager) scanSlotsLocked(slotsDir string, capacity int) (activeSlots []int, freeSlot int, diags []SlotDiagnostics, err error) {
	if err := os.MkdirAll(slotsDir, 0700); err != nil {
		return nil, -1, nil, fmt.Errorf("%w: failed to create slots directory: %v", ErrStorageUnavailable, err)
	}

	freeSlot = -1
	maxScan := capacity
	if maxScan < 1 {
		maxScan = 1
	}

	// Also check existing files beyond capacity (in case limit was reduced while slots were active)
	entries, _ := os.ReadDir(slotsDir)
	for _, e := range entries {
		var idx int
		if n, _ := fmt.Sscanf(e.Name(), "slot_%d.lock", &idx); n == 1 && idx+1 > maxScan {
			maxScan = idx + 1
		}
	}

	for i := 0; i < maxScan; i++ {
		path := filepath.Join(slotsDir, fmt.Sprintf("slot_%d.lock", i))
		fd, err := openLockFile(path)
		if err != nil {
			return nil, -1, nil, err
		}

		lockErr := tryLockEx(fd)
		if isLockedError(lockErr) {
			// Slot is actively held
			_ = closeFd(fd)
			activeSlots = append(activeSlots, i)

			var diag SlotDiagnostics
			if data, err := os.ReadFile(path); err == nil {
				_ = json.Unmarshal(data, &diag)
			}
			if diag.SlotIndex == 0 && diag.PID == 0 {
				diag.SlotIndex = i
			}
			diags = append(diags, diag)
		} else if lockErr == nil {
			// Slot is free
			_ = unlock(fd)
			_ = closeFd(fd)
			if freeSlot == -1 && i < capacity {
				freeSlot = i
			}
		} else {
			_ = closeFd(fd)
		}
	}

	return activeSlots, freeSlot, diags, nil
}

// claimSlotLocked takes exclusive ownership of slotIndex and writes diagnostic metadata.
func (m *Manager) claimSlotLocked(slotsDir string, slotIndex int, req Request, bucket BucketKind, accountID string) (Handle, error) {
	path := filepath.Join(slotsDir, fmt.Sprintf("slot_%d.lock", slotIndex))
	fd, err := openLockFile(path)
	if err != nil {
		return nil, err
	}

	if err := tryLockEx(fd); err != nil {
		_ = closeFd(fd)
		return nil, ErrCapacityExceeded
	}

	diag := SlotDiagnostics{
		SlotIndex:  slotIndex,
		PID:        os.Getpid(),
		RequestID:  req.RequestID,
		Purpose:    req.Purpose,
		Model:      req.Model,
		AcquiredAt: time.Now(),
	}

	raw, _ := json.Marshal(diag)
	_ = syscall.Ftruncate(fd, 0)
	_, _ = syscall.Seek(fd, 0, io.SeekStart)
	_, _ = syscall.Write(fd, raw)

	return &slotHandle{
		manager:     m,
		accountID:   accountID,
		bucket:      bucket,
		slotIndex:   slotIndex,
		fd:          fd,
		diagnostics: diag,
	}, nil
}

// Acquire admits a request according to the premium quota contract.
func (m *Manager) Acquire(ctx context.Context, req Request) (Handle, error) {
	bucket, isMetered, err := ClassifyRequest(req)
	if err != nil {
		return nil, err
	}
	if !isMetered {
		return &unmeteredHandle{req: req}, nil
	}

	accountDir, _, err := m.accountDir(req.AccountID)
	if err != nil {
		return nil, err
	}

	slotsSubdir := "slots"
	waitersSubdir := "waiters_combined"
	if bucket == BucketCouncilExempt {
		slotsSubdir = "slots_exempt"
		waitersSubdir = "waiters_exempt"
	}
	slotsDir := filepath.Join(accountDir, slotsSubdir)
	waitersDir := filepath.Join(accountDir, waitersSubdir)

	// Fast path under brief admin lock
	unlockAdmin, err := lockAdmin(ctx, accountDir)
	if err != nil {
		return nil, err
	}

	cfg, err := LoadAccountConfig(accountDir)
	if err != nil {
		unlockAdmin()
		return nil, err
	}

	capacity := cfg.CombinedLimit
	if bucket == BucketCouncilExempt {
		capacity = cfg.CouncilExemptLimit
	}
	if capacity < 1 {
		capacity = 1
	}

	activeSlots, freeSlot, _, err := m.scanSlotsLocked(slotsDir, capacity)
	if err != nil {
		unlockAdmin()
		return nil, err
	}

	liveWaiters, err := cleanAndListWaiters(waitersDir)
	if err != nil {
		unlockAdmin()
		return nil, err
	}

	// For ordinary child requests: nonblocking capacity fallback
	if req.Priority == PriorityChild {
		if len(activeSlots) < capacity && len(liveWaiters) == 0 && freeSlot >= 0 {
			handle, err := m.claimSlotLocked(slotsDir, freeSlot, req, bucket, req.AccountID)
			unlockAdmin()
			return handle, err
		}
		unlockAdmin()
		return nil, ErrCapacityExceeded
	}

	// For root requests: if capacity is available and no waiters exist, claim immediately
	if len(activeSlots) < capacity && len(liveWaiters) == 0 && freeSlot >= 0 {
		handle, err := m.claimSlotLocked(slotsDir, freeSlot, req, bucket, req.AccountID)
		unlockAdmin()
		return handle, err
	}

	// Root priority waiter registration
	waiter, err := registerRootWaiter(waitersDir, req)
	unlockAdmin()
	if err != nil {
		return nil, err
	}
	defer waiter.cleanup()

	// Wait loop for root request
	pollInterval := 15 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			waiter.cleanup()
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		unlockAdmin, err := lockAdmin(ctx, accountDir)
		if err != nil {
			return nil, err
		}

		// Reread config on every iteration to observe limit changes dynamically
		cfg, err := LoadAccountConfig(accountDir)
		if err != nil {
			unlockAdmin()
			return nil, err
		}

		capacity := cfg.CombinedLimit
		if bucket == BucketCouncilExempt {
			capacity = cfg.CouncilExemptLimit
		}
		if capacity < 1 {
			capacity = 1
		}

		activeSlots, freeSlot, _, err := m.scanSlotsLocked(slotsDir, capacity)
		if err != nil {
			unlockAdmin()
			return nil, err
		}

		liveWaiters, err := cleanAndListWaiters(waitersDir)
		if err != nil {
			unlockAdmin()
			return nil, err
		}

		// Check if this root waiter is the earliest in line
		isEarliest := len(liveWaiters) > 0 && liveWaiters[0].WaiterID == waiter.id
		if len(activeSlots) < capacity && isEarliest && freeSlot >= 0 {
			// Cancel waiting before actual claim: verify context is not cancelled
			if ctx.Err() != nil {
				waiter.cleanup()
				unlockAdmin()
				return nil, ctx.Err()
			}
			// Cancel/cleanup waiter registration BEFORE claiming slot under lock
			waiter.cleanup()
			handle, err := m.claimSlotLocked(slotsDir, freeSlot, req, bucket, req.AccountID)
			unlockAdmin()
			return handle, err
		}

		unlockAdmin()
	}
}

// SetConfig updates account configuration atomically under lock.
func (m *Manager) SetConfig(accountID string, cfg AccountConfig) error {
	accountDir, _, err := m.accountDir(accountID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	unlockAdmin, err := lockAdmin(ctx, accountDir)
	if err != nil {
		return err
	}
	defer unlockAdmin()

	return SaveAccountConfig(accountDir, cfg)
}

// GetConfig reads account configuration.
func (m *Manager) GetConfig(accountID string) (AccountConfig, error) {
	accountDir, _, err := m.accountDir(accountID)
	if err != nil {
		return AccountConfig{}, err
	}
	return LoadAccountConfig(accountDir)
}

// Diagnostics returns quota state and active slots for an account.
func (m *Manager) Diagnostics(accountID string) (Diagnostics, error) {
	accountDir, isFallback, err := m.accountDir(accountID)
	if err != nil {
		return Diagnostics{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	unlockAdmin, err := lockAdmin(ctx, accountDir)
	if err != nil {
		return Diagnostics{}, err
	}
	defer unlockAdmin()

	cfg, err := LoadAccountConfig(accountDir)
	if err != nil {
		return Diagnostics{}, err
	}

	slotsDir := filepath.Join(accountDir, "slots")
	_, _, activeCombined, _ := m.scanSlotsLocked(slotsDir, cfg.CombinedLimit)

	exemptDir := filepath.Join(accountDir, "slots_exempt")
	_, _, activeExempt, _ := m.scanSlotsLocked(exemptDir, cfg.CouncilExemptLimit)

	combinedWaiters, _ := cleanAndListWaiters(filepath.Join(accountDir, "waiters_combined"))
	exemptWaiters, _ := cleanAndListWaiters(filepath.Join(accountDir, "waiters_exempt"))
	legacyWaiters, _ := cleanAndListWaiters(filepath.Join(accountDir, "waiters"))
	var allWaiters []WaiterDiagnostics
	allWaiters = append(allWaiters, combinedWaiters...)
	allWaiters = append(allWaiters, exemptWaiters...)
	allWaiters = append(allWaiters, legacyWaiters...)
	sort.Slice(allWaiters, func(i, j int) bool {
		return allWaiters[i].WaiterID < allWaiters[j].WaiterID
	})

	return Diagnostics{
		AccountID:          accountID,
		IsFallbackBucket:   isFallback,
		Preset:             cfg.Preset,
		CombinedLimit:      cfg.CombinedLimit,
		CouncilExemptLimit: cfg.CouncilExemptLimit,
		ActiveCombined:     activeCombined,
		ActiveExempt:       activeExempt,
		Waiters:            allWaiters,
		StorageDir:         accountDir,
	}, nil
}

// slotHandle implements Handle for metered requests.
type slotHandle struct {
	manager     *Manager
	accountID   string
	bucket      BucketKind
	slotIndex   int
	fd          int
	diagnostics SlotDiagnostics
	closed      bool
	mu          sync.Mutex
}

func (h *slotHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	var err error
	if h.fd >= 0 {
		_ = unlock(h.fd)
		err = closeFd(h.fd)
		h.fd = -1
	}
	return err
}

func (h *slotHandle) Bucket() BucketKind           { return h.bucket }
func (h *slotHandle) Slot() int                    { return h.slotIndex }
func (h *slotHandle) AccountID() string            { return h.accountID }
func (h *slotHandle) IsExempt() bool               { return h.bucket == BucketCouncilExempt }
func (h *slotHandle) IsMetered() bool              { return true }
func (h *slotHandle) Diagnostics() SlotDiagnostics { return h.diagnostics }

// unmeteredHandle implements Handle for non-metered requests.
type unmeteredHandle struct {
	req Request
}

func (h *unmeteredHandle) Close() error       { return nil }
func (h *unmeteredHandle) Bucket() BucketKind { return BucketUnmetered }
func (h *unmeteredHandle) Slot() int          { return -1 }
func (h *unmeteredHandle) AccountID() string  { return h.req.AccountID }
func (h *unmeteredHandle) IsExempt() bool     { return false }
func (h *unmeteredHandle) IsMetered() bool    { return false }
func (h *unmeteredHandle) Diagnostics() SlotDiagnostics {
	return SlotDiagnostics{
		SlotIndex: -1,
		RequestID: h.req.RequestID,
		Purpose:   h.req.Purpose,
		Model:     h.req.Model,
	}
}
