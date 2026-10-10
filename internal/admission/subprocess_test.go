package admission

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess serves as the entrypoint for child processes spawned during tests.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	baseDir := os.Getenv("HELPER_BASE_DIR")
	accountID := os.Getenv("HELPER_ACCOUNT")
	model := os.Getenv("HELPER_MODEL")
	if model == "" {
		model = "gpt-6-sol"
	}
	cmd := os.Getenv("HELPER_CMD")

	mgr := NewManager(WithBaseDir(baseDir))

	switch cmd {
	case "hold_slot":
		priority := PriorityChild
		if os.Getenv("HELPER_PRIORITY") == "root" {
			priority = PriorityRoot
		}
		handle, err := mgr.Acquire(context.Background(), Request{
			AccountID: accountID,
			Provider:  "codex",
			Model:     model,
			Priority:  priority,
			RequestID: "helper-" + strconv.Itoa(os.Getpid()),
		})
		if err != nil {
			fmt.Printf("ACQUIRE_ERROR: %v\n", err)
			os.Exit(1)
		}
		// Signal to parent that slot is held
		fmt.Println("ACQUIRED")

		// Wait on stdin for instruction to release
		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadString('\n')

		_ = handle.Close()
		fmt.Println("RELEASED")
		os.Exit(0)

	case "try_acquire_child":
		handle, err := mgr.Acquire(context.Background(), Request{
			AccountID: accountID,
			Provider:  "codex",
			Model:     model,
			Priority:  PriorityChild,
			RequestID: "child-probe-" + strconv.Itoa(os.Getpid()),
		})
		if err != nil {
			if strings.Contains(err.Error(), "capacity exceeded") {
				fmt.Println("CAPACITY_EXCEEDED")
				os.Exit(2)
			}
			fmt.Printf("ERROR: %v\n", err)
			os.Exit(1)
		}
		_ = handle.Close()
		fmt.Println("ACQUIRED")
		os.Exit(0)

	case "acquire_root":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		handle, err := mgr.Acquire(ctx, Request{
			AccountID: accountID,
			Provider:  "codex",
			Model:     model,
			Priority:  PriorityRoot,
			RequestID: "root-waiter-" + strconv.Itoa(os.Getpid()),
		})
		if err != nil {
			fmt.Printf("ACQUIRE_ERROR: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("ACQUIRED")

		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadString('\n')
		_ = handle.Close()
		fmt.Println("RELEASED")
		os.Exit(0)

	case "set_config":
		preset := Preset(os.Getenv("HELPER_PRESET"))
		limit, _ := strconv.Atoi(os.Getenv("HELPER_LIMIT"))
		exemptLimit, _ := strconv.Atoi(os.Getenv("HELPER_EXEMPT_LIMIT"))
		if exemptLimit <= 0 {
			exemptLimit = 1
		}
		err := mgr.SetConfig(accountID, AccountConfig{
			Preset:             preset,
			CombinedLimit:      limit,
			CouncilExemptLimit: exemptLimit,
		})
		if err != nil {
			fmt.Printf("SET_CONFIG_ERROR: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("CONFIG_SET")
		os.Exit(0)

	case "spawn_exec_child":
		// Acquire a slot with O_CLOEXEC
		handle, err := mgr.Acquire(context.Background(), Request{
			AccountID: accountID,
			Provider:  "codex",
			Model:     model,
			Priority:  PriorityChild,
		})
		if err != nil {
			fmt.Printf("ACQUIRE_ERROR: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("ACQUIRED")

		// Spawn a child process that sleeps
		childCmd := exec.Command("sleep", "10")
		if err := childCmd.Start(); err != nil {
			fmt.Printf("CHILD_START_ERROR: %v\n", err)
			os.Exit(1)
		}

		// Close parent handle and exit immediately, leaving child process running
		_ = handle.Close()
		os.Exit(0)

	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)

	default:
		fmt.Printf("UNKNOWN_HELPER_CMD: %s\n", cmd)
		os.Exit(1)
	}
}

// helperCmd builds a command invocation for TestHelperProcess.
func helperCmd(baseDir, accountID, cmdName string, envExtra ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"HELPER_BASE_DIR="+baseDir,
		"HELPER_ACCOUNT="+accountID,
		"HELPER_CMD="+cmdName,
	)
	cmd.Env = append(cmd.Env, envExtra...)
	return cmd
}

// TestSubprocessCaps verifies cross-process enforcement of configured caps.
func TestSubprocessCaps(t *testing.T) {
	baseDir := t.TempDir()
	accountID := "subproc_caps_acct"

	// 1. Start process 1 to hold slot
	p1 := helperCmd(baseDir, accountID, "hold_slot")
	stdin1, err := p1.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe failed: %v", err)
	}
	stdout1, err := p1.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe failed: %v", err)
	}
	if err := p1.Start(); err != nil {
		t.Fatalf("p1 start failed: %v", err)
	}
	defer func() {
		_ = p1.Process.Kill()
		_ = p1.Wait()
	}()

	scanner1 := bufio.NewScanner(stdout1)
	if !scanner1.Scan() || scanner1.Text() != "ACQUIRED" {
		t.Fatalf("p1 failed to acquire slot: %v", scanner1.Text())
	}

	// 2. Process 2 attempts acquire: should fail with capacity exceeded
	p2 := helperCmd(baseDir, accountID, "try_acquire_child")
	out2, err2 := p2.CombinedOutput()
	if !strings.Contains(string(out2), "CAPACITY_EXCEEDED") {
		t.Fatalf("p2 expected CAPACITY_EXCEEDED, got err=%v out=%s", err2, string(out2))
	}

	// 3. Release process 1
	_, _ = io.WriteString(stdin1, "\n")
	_ = p1.Wait()

	// 4. Process 3 attempts acquire: succeeds now
	p3 := helperCmd(baseDir, accountID, "try_acquire_child")
	out3, err3 := p3.CombinedOutput()
	if err3 != nil || !strings.Contains(string(out3), "ACQUIRED") {
		t.Fatalf("p3 expected ACQUIRED, got err=%v out=%s", err3, string(out3))
	}
}

// TestSubprocessDeath verifies immediate OS advisory lock reclamation upon SIGKILL.
func TestSubprocessDeath(t *testing.T) {
	baseDir := t.TempDir()
	accountID := "subproc_death_acct"

	// Process 1 acquires slot
	p1 := helperCmd(baseDir, accountID, "hold_slot")
	stdin1, err := p1.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe failed: %v", err)
	}
	_ = stdin1 // kept open until process is killed
	stdout1, err := p1.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe failed: %v", err)
	}
	if err := p1.Start(); err != nil {
		t.Fatalf("p1 start failed: %v", err)
	}

	scanner1 := bufio.NewScanner(stdout1)
	if !scanner1.Scan() || scanner1.Text() != "ACQUIRED" {
		t.Fatalf("p1 failed to acquire: %v", scanner1.Text())
	}

	// Confirm slot is held
	p2 := helperCmd(baseDir, accountID, "try_acquire_child")
	out2, _ := p2.CombinedOutput()
	if !strings.Contains(string(out2), "CAPACITY_EXCEEDED") {
		t.Fatalf("expected slot to be full, got: %s", string(out2))
	}

	// Abruptly kill Process 1 with SIGKILL (simulating crash)
	if err := p1.Process.Kill(); err != nil {
		t.Fatalf("p1 kill failed: %v", err)
	}
	_ = p1.Wait()

	// Process 3 immediately attempts acquire: must succeed without timeout
	p3 := helperCmd(baseDir, accountID, "try_acquire_child")
	out3, err3 := p3.CombinedOutput()
	if err3 != nil || !strings.Contains(string(out3), "ACQUIRED") {
		t.Fatalf("expected immediate acquisition after SIGKILL, got err=%v out=%s", err3, string(out3))
	}
}

// TestSubprocessFDNonInheritance verifies lock descriptors are not inherited across exec.
func TestSubprocessFDNonInheritance(t *testing.T) {
	baseDir := t.TempDir()
	accountID := "subproc_cloexec_acct"

	// Process 1 runs spawn_exec_child: acquires slot, spawns background sleep 10, then exits
	p1 := helperCmd(baseDir, accountID, "spawn_exec_child")
	out1, err := p1.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn_exec_child failed: %v out=%s", err, string(out1))
	}

	// Process 2 attempts acquire: should succeed immediately because sleep 10 did not inherit the lock FD
	p2 := helperCmd(baseDir, accountID, "try_acquire_child")
	out2, err2 := p2.CombinedOutput()
	if err2 != nil || !strings.Contains(string(out2), "ACQUIRED") {
		t.Fatalf("expected ACQUIRED, got err=%v out=%s (FD was likely inherited!)", err2, string(out2))
	}
}

// TestSubprocessRootPriority verifies cross-process root waiter priority over children.
func TestSubprocessRootPriority(t *testing.T) {
	baseDir := t.TempDir()
	accountID := "subproc_root_prio_acct"

	// 1. Process 1 holds the only slot (limit 1)
	p1 := helperCmd(baseDir, accountID, "hold_slot")
	stdin1, _ := p1.StdinPipe()
	stdout1, _ := p1.StdoutPipe()
	if err := p1.Start(); err != nil {
		t.Fatalf("p1 start failed: %v", err)
	}
	defer func() {
		_ = p1.Process.Kill()
		_ = p1.Wait()
	}()

	scanner1 := bufio.NewScanner(stdout1)
	if !scanner1.Scan() || scanner1.Text() != "ACQUIRED" {
		t.Fatalf("p1 acquire failed: %v", scanner1.Text())
	}

	// 2. Process 2 (Root priority) starts waiting for next slot
	p2 := helperCmd(baseDir, accountID, "acquire_root")
	stdin2, _ := p2.StdinPipe()
	stdout2, _ := p2.StdoutPipe()
	if err := p2.Start(); err != nil {
		t.Fatalf("p2 start failed: %v", err)
	}
	defer func() {
		_ = p2.Process.Kill()
		_ = p2.Wait()
	}()

	// Wait for root waiter to register
	time.Sleep(100 * time.Millisecond)

	// 3. Process 3 (Child priority) attempts to acquire: must fail immediately with CAPACITY_EXCEEDED
	p3 := helperCmd(baseDir, accountID, "try_acquire_child")
	out3, _ := p3.CombinedOutput()
	if !strings.Contains(string(out3), "CAPACITY_EXCEEDED") {
		t.Fatalf("expected child to yield to root waiter with CAPACITY_EXCEEDED, got: %s", string(out3))
	}

	// 4. Release Process 1
	_, _ = io.WriteString(stdin1, "\n")
	_ = p1.Wait()

	// 5. Root waiter (Process 2) should now acquire the slot
	scanner2 := bufio.NewScanner(stdout2)
	if !scanner2.Scan() || scanner2.Text() != "ACQUIRED" {
		t.Fatalf("expected root waiter to acquire, got: %v", scanner2.Text())
	}

	// Release root waiter
	_, _ = io.WriteString(stdin2, "\n")
	_ = p2.Wait()
}

// TestSubprocessSharedLimitReload verifies dynamic limit reload and non-cancellation on limit decrease.
func TestSubprocessSharedLimitReload(t *testing.T) {
	baseDir := t.TempDir()
	accountID := "subproc_reload_acct"

	// 1. Process configures limit = 5
	pCfg := helperCmd(baseDir, accountID, "set_config", "HELPER_PRESET=5x", "HELPER_LIMIT=5")
	if out, err := pCfg.CombinedOutput(); err != nil {
		t.Fatalf("set_config 5x failed: %v out=%s", err, string(out))
	}

	// 2. Hold slot A
	pA := helperCmd(baseDir, accountID, "hold_slot")
	stdinA, _ := pA.StdinPipe()
	stdoutA, _ := pA.StdoutPipe()
	if err := pA.Start(); err != nil {
		t.Fatalf("pA start failed: %v", err)
	}
	defer func() { _ = pA.Process.Kill(); _ = pA.Wait() }()
	sA := bufio.NewScanner(stdoutA)
	if !sA.Scan() || sA.Text() != "ACQUIRED" {
		t.Fatalf("pA acquire failed")
	}

	// 3. Hold slot B
	pB := helperCmd(baseDir, accountID, "hold_slot")
	stdinB, _ := pB.StdinPipe()
	stdoutB, _ := pB.StdoutPipe()
	if err := pB.Start(); err != nil {
		t.Fatalf("pB start failed: %v", err)
	}
	defer func() { _ = pB.Process.Kill(); _ = pB.Wait() }()
	sB := bufio.NewScanner(stdoutB)
	if !sB.Scan() || sB.Text() != "ACQUIRED" {
		t.Fatalf("pB acquire failed")
	}

	// 4. Update limit to 1 (decrease limit).
	// Crucial contract: existing streams are NEVER cancelled when limit decreases.
	pCfg2 := helperCmd(baseDir, accountID, "set_config", "HELPER_PRESET=basic", "HELPER_LIMIT=1")
	if out, err := pCfg2.CombinedOutput(); err != nil {
		t.Fatalf("set_config basic failed: %v out=%s", err, string(out))
	}

	// 5. Process C attempts acquire: fails because 2 active >= 1 new limit
	pC := helperCmd(baseDir, accountID, "try_acquire_child")
	outC, _ := pC.CombinedOutput()
	if !strings.Contains(string(outC), "CAPACITY_EXCEEDED") {
		t.Fatalf("expected CAPACITY_EXCEEDED after limit decrease, got: %s", string(outC))
	}

	// 6. Release slot A. Active count is now 1.
	// Since active (1) >= limit (1), next attempt should STILL fail.
	_, _ = io.WriteString(stdinA, "\n")
	_ = pA.Wait()

	pD := helperCmd(baseDir, accountID, "try_acquire_child")
	outD, _ := pD.CombinedOutput()
	if !strings.Contains(string(outD), "CAPACITY_EXCEEDED") {
		t.Fatalf("expected CAPACITY_EXCEEDED while 1 slot still active, got: %s", string(outD))
	}

	// 7. Release slot B. Active count is now 0.
	// Now next attempt succeeds!
	_, _ = io.WriteString(stdinB, "\n")
	_ = pB.Wait()

	pE := helperCmd(baseDir, accountID, "try_acquire_child")
	outE, errE := pE.CombinedOutput()
	if errE != nil || !strings.Contains(string(outE), "ACQUIRED") {
		t.Fatalf("expected ACQUIRED after all slots released, got err=%v out=%s", errE, string(outE))
	}
}

// TestSubprocessUnknownAccountCoordination verifies multiple processes with empty account ID
// coordinate within the same conservative fallback bucket.
func TestSubprocessUnknownAccountCoordination(t *testing.T) {
	baseDir := t.TempDir()
	emptyAccountID := ""

	// Process 1 with empty account acquires slot
	p1 := helperCmd(baseDir, emptyAccountID, "hold_slot")
	stdin1, _ := p1.StdinPipe()
	stdout1, _ := p1.StdoutPipe()
	if err := p1.Start(); err != nil {
		t.Fatalf("p1 start failed: %v", err)
	}
	defer func() { _ = p1.Process.Kill(); _ = p1.Wait() }()

	scanner1 := bufio.NewScanner(stdout1)
	if !scanner1.Scan() || scanner1.Text() != "ACQUIRED" {
		t.Fatalf("p1 acquire failed")
	}

	// Process 2 with empty account fails with CAPACITY_EXCEEDED (shared bucket limit 1)
	p2 := helperCmd(baseDir, emptyAccountID, "try_acquire_child")
	out2, _ := p2.CombinedOutput()
	if !strings.Contains(string(out2), "CAPACITY_EXCEEDED") {
		t.Fatalf("expected CAPACITY_EXCEEDED in shared fallback bucket, got: %s", string(out2))
	}

	// Release Process 1
	_, _ = io.WriteString(stdin1, "\n")
	_ = p1.Wait()

	// Process 3 with empty account can now acquire
	p3 := helperCmd(baseDir, emptyAccountID, "try_acquire_child")
	out3, err3 := p3.CombinedOutput()
	if err3 != nil || !strings.Contains(string(out3), "ACQUIRED") {
		t.Fatalf("expected ACQUIRED in shared fallback bucket after release, got err=%v out=%s", err3, string(out3))
	}
}
