package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPresets(t *testing.T) {
	tests := []struct {
		input     string
		wantP     Preset
		wantLimit int
	}{
		{"basic", PresetBasic, 1},
		{"basic1", PresetBasic, 1},
		{"1", PresetBasic, 1},
		{"1x", PresetBasic, 1},
		{"5x", Preset5x, 5},
		{"5", Preset5x, 5},
		{"$100", Preset5x, 5},
		{"100", Preset5x, 5},
		{"10x", Preset10x, 10},
		{"10", Preset10x, 10},
		{"$200", Preset10x, 10},
		{"200", Preset10x, 10},
		{"25x", Preset25x, 25},
		{"25", Preset25x, 25},
		{"$500", Preset25x, 25},
		{"500", Preset25x, 25},
		{"unknown", PresetBasic, 1},
		{"", PresetBasic, 1},
		{"pro-tier", PresetBasic, 1},
		{"-5", PresetBasic, 1},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			p, limit := ParsePreset(tc.input)
			if p != tc.wantP || limit != tc.wantLimit {
				t.Errorf("ParsePreset(%q) = (%v, %d), want (%v, %d)", tc.input, p, limit, tc.wantP, tc.wantLimit)
			}
		})
	}
}

func TestClassifyModel(t *testing.T) {
	tests := []struct {
		name        string
		req         Request
		wantBucket  BucketKind
		wantMetered bool
		wantErr     error
	}{
		{
			name:        "empty model",
			req:         Request{Provider: "codex", Model: ""},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     ErrInvalidModel,
		},
		{
			name:        "codex gpt-6-sol",
			req:         Request{Provider: "codex", Model: "gpt-6-sol"},
			wantBucket:  BucketPremiumCombined,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name:        "codex gpt-6.1-sol",
			req:         Request{Provider: "codex", Model: "gpt-6.1-sol"},
			wantBucket:  BucketPremiumCombined,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name:        "codex gpt-sol",
			req:         Request{Provider: "codex", Model: "gpt-sol"},
			wantBucket:  BucketPremiumCombined,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name:        "codex/gpt-6-sol compound ref",
			req:         Request{Model: "codex/gpt-6-sol"},
			wantBucket:  BucketPremiumCombined,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name:        "codex gpt-6-astra",
			req:         Request{Provider: "codex", Model: "gpt-6-astra"},
			wantBucket:  BucketPremiumCombined,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name:        "codex gpt-astra",
			req:         Request{Provider: "codex", Model: "gpt-astra"},
			wantBucket:  BucketPremiumCombined,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name:        "codex gpt-6-luna (unmetered)",
			req:         Request{Provider: "codex", Model: "gpt-6-luna"},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     nil,
		},
		{
			name:        "codex gpt-luna (unmetered)",
			req:         Request{Provider: "codex", Model: "gpt-luna"},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     nil,
		},
		{
			name:        "openrouter gpt-6-sol (unmetered)",
			req:         Request{Provider: "openrouter", Model: "gpt-6-sol"},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     nil,
		},
		{
			name:        "opencode-zen gpt-6.1-sol (unmetered)",
			req:         Request{Provider: "opencode-zen", Model: "gpt-6.1-sol"},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     nil,
		},
		{
			name:        "opencode-go mimo-flash (unmetered)",
			req:         Request{Provider: "opencode-go", Model: "mimo-flash"},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     nil,
		},
		{
			name: "codex sol council coordinator exempt",
			req: Request{
				Provider:  "codex",
				Model:     "gpt-6-sol",
				Exemption: ExemptionCouncilCoordinatorSol,
			},
			wantBucket:  BucketCouncilExempt,
			wantMetered: true,
			wantErr:     nil,
		},
		{
			name: "codex astra council coordinator exempt rejected",
			req: Request{
				Provider:  "codex",
				Model:     "gpt-6-astra",
				Exemption: ExemptionCouncilCoordinatorSol,
			},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     ErrInvalidExemption,
		},
		{
			name: "codex luna council coordinator exempt rejected",
			req: Request{
				Provider:  "codex",
				Model:     "gpt-6-luna",
				Exemption: ExemptionCouncilCoordinatorSol,
			},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     ErrInvalidExemption,
		},
		{
			name: "non-codex council coordinator exempt rejected",
			req: Request{
				Provider:  "openrouter",
				Model:     "gpt-6-sol",
				Exemption: ExemptionCouncilCoordinatorSol,
			},
			wantBucket:  BucketUnmetered,
			wantMetered: false,
			wantErr:     ErrInvalidExemption,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, metered, err := ClassifyRequest(tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ClassifyRequest() error = %v, want %v", err, tc.wantErr)
			}
			if b != tc.wantBucket {
				t.Errorf("ClassifyRequest() bucket = %v, want %v", b, tc.wantBucket)
			}
			if metered != tc.wantMetered {
				t.Errorf("ClassifyRequest() metered = %v, want %v", metered, tc.wantMetered)
			}
		})
	}
}

func TestConfigAtomicReadWrite(t *testing.T) {
	tmpDir := t.TempDir()
	accountDir := filepath.Join(tmpDir, "test_acct")

	// Missing config defaults conservatively
	cfg, err := LoadAccountConfig(accountDir)
	if err != nil {
		t.Fatalf("LoadAccountConfig on missing dir failed: %v", err)
	}
	if cfg.Preset != PresetBasic || cfg.CombinedLimit != 1 || cfg.CouncilExemptLimit != 1 {
		t.Fatalf("unexpected default config: %+v", cfg)
	}

	// Save new config
	newCfg := AccountConfig{
		Preset:             Preset5x,
		CombinedLimit:      5,
		CouncilExemptLimit: 2,
	}
	if err := SaveAccountConfig(accountDir, newCfg); err != nil {
		t.Fatalf("SaveAccountConfig failed: %v", err)
	}

	// Verify atomic file exists and content is readable
	loaded, err := LoadAccountConfig(accountDir)
	if err != nil {
		t.Fatalf("LoadAccountConfig after save failed: %v", err)
	}
	if loaded.Preset != Preset5x || loaded.CombinedLimit != 5 || loaded.CouncilExemptLimit != 2 {
		t.Fatalf("unexpected loaded config: %+v", loaded)
	}
}

func TestCombinedCapSingleProcess(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()

	// Default basic preset: limit is 1
	req := Request{
		AccountID: "user_123",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "req-1",
	}

	h1, err := mgr.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer h1.Close()

	if h1.Slot() != 0 || h1.Bucket() != BucketPremiumCombined || !h1.IsMetered() {
		t.Fatalf("unexpected handle 1 state: slot=%d bucket=%s metered=%v", h1.Slot(), h1.Bucket(), h1.IsMetered())
	}

	// Second child request should fail nonblocking with ErrCapacityExceeded
	req2 := Request{
		AccountID: "user_123",
		Provider:  "codex",
		Model:     "gpt-6-astra",
		Priority:  PriorityChild,
		RequestID: "req-2",
	}

	_, err2 := mgr.Acquire(ctx, req2)
	if !errors.Is(err2, ErrCapacityExceeded) {
		t.Fatalf("expected ErrCapacityExceeded, got: %v", err2)
	}

	// Release first slot
	if err := h1.Close(); err != nil {
		t.Fatalf("close h1 failed: %v", err)
	}

	// Now second request succeeds
	h2, err := mgr.Acquire(ctx, req2)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	defer h2.Close()
	if h2.Slot() != 0 {
		t.Fatalf("expected slot 0, got %d", h2.Slot())
	}
}

func TestCouncilExemptBucket(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()

	// 1. Fill the regular combined bucket
	regReq := Request{
		AccountID: "acct_council_test",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "req-reg",
	}
	hReg, err := mgr.Acquire(ctx, regReq)
	if err != nil {
		t.Fatalf("acquire regular slot failed: %v", err)
	}
	defer hReg.Close()

	// Combined bucket is now full (limit 1)
	_, err = mgr.Acquire(ctx, regReq)
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("expected ErrCapacityExceeded for regular request, got: %v", err)
	}

	// 2. Council Coordinator Sol request can acquire from separate exempt bucket
	exemptReq := Request{
		AccountID: "acct_council_test",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		Exemption: ExemptionCouncilCoordinatorSol,
		RequestID: "req-council-exempt-1",
	}
	hExempt, err := mgr.Acquire(ctx, exemptReq)
	if err != nil {
		t.Fatalf("acquire exempt slot failed: %v", err)
	}
	defer hExempt.Close()

	if hExempt.Bucket() != BucketCouncilExempt || !hExempt.IsExempt() {
		t.Fatalf("expected BucketCouncilExempt, got: %s", hExempt.Bucket())
	}

	// 3. Second exempt request fails because default exempt limit is 1
	exemptReq2 := Request{
		AccountID: "acct_council_test",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		Exemption: ExemptionCouncilCoordinatorSol,
		RequestID: "req-council-exempt-2",
	}
	_, err = mgr.Acquire(ctx, exemptReq2)
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("expected ErrCapacityExceeded for second exempt request, got: %v", err)
	}
}

func TestRootPriorityWaitSingleProcess(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Slot occupied by child
	hChild, err := mgr.Acquire(ctx, Request{
		AccountID: "root_test",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "child-holding",
	})
	if err != nil {
		t.Fatalf("child acquire failed: %v", err)
	}

	// Root request arrives and begins waiting in background
	rootAcquired := make(chan Handle, 1)
	rootErr := make(chan error, 1)
	go func() {
		hRoot, err := mgr.Acquire(ctx, Request{
			AccountID: "root_test",
			Provider:  "codex",
			Model:     "gpt-6-sol",
			Priority:  PriorityRoot,
			RequestID: "root-waiting",
		})
		if err != nil {
			rootErr <- err
		} else {
			rootAcquired <- hRoot
		}
	}()

	// Give root waiter time to register
	time.Sleep(50 * time.Millisecond)

	// An ordinary child request attempts acquire: must fail immediately (nonblocking capacity fallback)
	// even before child releases, and because root waiter has priority
	_, errChild2 := mgr.Acquire(ctx, Request{
		AccountID: "root_test",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "child-jumping",
	})
	if !errors.Is(errChild2, ErrCapacityExceeded) {
		t.Fatalf("expected child jumping to fail with ErrCapacityExceeded, got: %v", errChild2)
	}

	// Now release the held slot
	_ = hChild.Close()

	// Root waiter should now receive the slot
	select {
	case hRoot := <-rootAcquired:
		defer hRoot.Close()
		if hRoot.Slot() != 0 {
			t.Fatalf("expected slot 0, got %d", hRoot.Slot())
		}
	case err := <-rootErr:
		t.Fatalf("root waiter returned error: %v", err)
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for root waiter to acquire slot")
	}
}

func TestHandleCloseIdempotent(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	h, err := mgr.Acquire(context.Background(), Request{
		AccountID: "idem_test",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
	})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("first close failed: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second close failed: %v", err)
	}
}

func TestStorageUnavailable(t *testing.T) {
	// Point to a file rather than directory to trigger storage failure
	f, err := os.CreateTemp("", "bad_dir_*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(f.Name())
	_ = f.Close()

	mgr := NewManager(WithBaseDir(f.Name()))
	_, err = mgr.Acquire(context.Background(), Request{
		AccountID: "bad_storage",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
	})
	if !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("expected ErrStorageUnavailable, got: %v", err)
	}
}

func TestUnknownAccountFallback(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()

	// Empty account ID
	h1, err := mgr.Acquire(ctx, Request{
		AccountID: "",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
	})
	if err != nil {
		t.Fatalf("acquire with empty account ID failed: %v", err)
	}
	defer h1.Close()

	diag, err := mgr.Diagnostics("")
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	if !diag.IsFallbackBucket {
		t.Fatalf("expected fallback bucket true, got: %v", diag.IsFallbackBucket)
	}
	if len(diag.ActiveCombined) != 1 {
		t.Fatalf("expected 1 active combined slot, got: %d", len(diag.ActiveCombined))
	}

	// Second request with empty account ID hits capacity 1 of fallback bucket
	_, err = mgr.Acquire(ctx, Request{
		AccountID: "",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
	})
	if !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("expected ErrCapacityExceeded on shared fallback bucket, got: %v", err)
	}
}

func TestDiagnostics(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()

	h, err := mgr.Acquire(ctx, Request{
		AccountID: "diag_acct",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "req-diag-1",
		Purpose:   "test-run",
	})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer h.Close()

	d, err := mgr.Diagnostics("diag_acct")
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	if len(d.ActiveCombined) != 1 {
		t.Fatalf("expected 1 active slot, got: %d", len(d.ActiveCombined))
	}
	if d.ActiveCombined[0].RequestID != "req-diag-1" {
		t.Fatalf("expected req-diag-1, got: %s", d.ActiveCombined[0].RequestID)
	}
	if d.ActiveCombined[0].Purpose != "test-run" {
		t.Fatalf("expected test-run, got: %s", d.ActiveCombined[0].Purpose)
	}
}

func TestExecuteAtBoundary(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()

	executed := false
	err := ExecuteAtBoundary(ctx, mgr, Request{
		AccountID: "boundary_acct",
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
	}, func(ctx context.Context) error {
		executed = true
		// Verify slot is actively held during stream execution
		d, err := mgr.Diagnostics("boundary_acct")
		if err != nil {
			return err
		}
		if len(d.ActiveCombined) != 1 {
			t.Errorf("expected 1 active slot during stream, got %d", len(d.ActiveCombined))
		}
		return nil
	})

	if err != nil {
		t.Fatalf("ExecuteAtBoundary failed: %v", err)
	}
	if !executed {
		t.Fatal("streamFn was not executed")
	}

	// After boundary execution, slot is released immediately
	d, err := mgr.Diagnostics("boundary_acct")
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	if len(d.ActiveCombined) != 0 {
		t.Fatalf("expected 0 active slots after boundary release, got %d", len(d.ActiveCombined))
	}
}

func TestSeparatedWaiterQueues(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()
	acct := "sep_waiter_acct"

	// Set 1 combined slot and 1 exempt slot
	if err := mgr.SetConfig(acct, AccountConfig{
		CombinedLimit:      1,
		CouncilExemptLimit: 1,
	}); err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}

	// 1. Fill the ordinary combined pool
	hCombined, err := mgr.Acquire(ctx, Request{
		AccountID: acct,
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "child-comb-1",
	})
	if err != nil {
		t.Fatalf("acquire combined slot failed: %v", err)
	}
	defer hCombined.Close()

	// 2. Queue an ordinary root waiter on the full combined pool
	ctxCancel, cancel := context.WithCancel(ctx)
	defer cancel()

	waiterStarted := make(chan struct{})
	go func() {
		close(waiterStarted)
		_, _ = mgr.Acquire(ctxCancel, Request{
			AccountID: acct,
			Provider:  "codex",
			Model:     "gpt-6-sol",
			Priority:  PriorityRoot,
			RequestID: "root-comb-waiting",
		})
	}()

	<-waiterStarted
	time.Sleep(50 * time.Millisecond) // Let waiter register in waiters_combined

	// 3. Council exempt request MUST succeed immediately despite waiting combined root request!
	hExempt, err := mgr.Acquire(ctx, Request{
		AccountID: acct,
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		Exemption: ExemptionCouncilCoordinatorSol,
		RequestID: "exempt-req-1",
	})
	if err != nil {
		t.Fatalf("exempt request should acquire immediately and NOT be blocked by combined waiter: %v", err)
	}
	defer hExempt.Close()

	// Diagnostics should show both slots active
	diag, err := mgr.Diagnostics(acct)
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	if len(diag.ActiveCombined) != 1 || len(diag.ActiveExempt) != 1 {
		t.Fatalf("expected 1 active combined and 1 active exempt, got %d and %d", len(diag.ActiveCombined), len(diag.ActiveExempt))
	}
}

func TestCancelWaitingBeforeActualClaim(t *testing.T) {
	mgr := NewManager(WithBaseDir(t.TempDir()))
	ctx := context.Background()
	acct := "cancel_waiting_acct"

	// Fill combined pool (capacity 1)
	h1, err := mgr.Acquire(ctx, Request{
		AccountID: acct,
		Provider:  "codex",
		Model:     "gpt-6-sol",
		Priority:  PriorityChild,
		RequestID: "hold-slot-1",
	})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer h1.Close()

	// Start root waiter with cancellable context
	ctxWait, cancelWait := context.WithCancel(ctx)
	errCh := make(chan error, 1)

	go func() {
		_, err := mgr.Acquire(ctxWait, Request{
			AccountID: acct,
			Provider:  "codex",
			Model:     "gpt-6-sol",
			Priority:  PriorityRoot,
			RequestID: "root-cancel-waiter",
		})
		errCh <- err
	}()

	time.Sleep(40 * time.Millisecond) // Let waiter register
	cancelWait()                      // Cancel waiting before slot is free

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled waiter to exit")
	}

	// Verify waiter registration was cleaned up
	time.Sleep(30 * time.Millisecond)
	diag, err := mgr.Diagnostics(acct)
	if err != nil {
		t.Fatalf("diagnostics failed: %v", err)
	}
	if len(diag.Waiters) != 0 {
		t.Fatalf("waiters should be 0 after cancellation, got: %d", len(diag.Waiters))
	}
}

func TestStableUIDDirectory(t *testing.T) {
	origAdmission := os.Getenv("UGH_ADMISSION_DIR")
	defer os.Setenv("UGH_ADMISSION_DIR", origAdmission)
	os.Unsetenv("UGH_ADMISSION_DIR")

	base1 := DefaultBaseDir()
	if base1 == "" {
		t.Fatal("DefaultBaseDir returned empty string")
	}

	// Even if XDG_RUNTIME_DIR is modified or unset, base directory must remain stable
	origXDG := os.Getenv("XDG_RUNTIME_DIR")
	defer os.Setenv("XDG_RUNTIME_DIR", origXDG)

	os.Setenv("XDG_RUNTIME_DIR", "/some/unrelated/path")
	base2 := DefaultBaseDir()

	os.Unsetenv("XDG_RUNTIME_DIR")
	base3 := DefaultBaseDir()

	if base1 != base2 || base1 != base3 {
		t.Fatalf("DefaultBaseDir split pool under differing XDG_RUNTIME_DIR: base1=%s, base2=%s, base3=%s", base1, base2, base3)
	}
}
