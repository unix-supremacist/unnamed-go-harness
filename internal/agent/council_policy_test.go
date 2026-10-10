package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
)

// TestCouncilAllocation_SpreadAndCaps verifies that for >= 4 angles,
// capped families (Luna, Haiku, Gemini) are strictly capped at 3 seats each,
// deterministically spread across angles rather than clustering on angles 0..2,
// and other panel families achieve full angle coverage.
func TestCouncilAllocation_SpreadAndCaps(t *testing.T) {
	angles := []CouncilAngle{
		{ID: "a1", Text: "Angle 1: evidence map", Lens: "evidence-map"},
		{ID: "a2", Text: "Angle 2: adversarial checks", Lens: "adversarial"},
		{ID: "a3", Text: "Angle 3: conventions & consistency", Lens: "conventions"},
		{ID: "a4", Text: "Angle 4: hidden constraints", Lens: "hidden-constraints"},
	}

	plan, err := PlanAllocation(AllocationRequest{
		Question: "Should council coordinator default to MiMo Pro?",
		Angles:   angles,
		Limits:   CouncilLimits{},
	})
	if err != nil {
		t.Fatalf("PlanAllocation failed: %v", err)
	}

	if plan.DistinctFamiliesCount != len(DefaultPanelFamilies) {
		t.Fatalf("expected %d distinct families, got %d", len(DefaultPanelFamilies), plan.DistinctFamiliesCount)
	}

	// Count seats per family and track which angles each family was assigned to.
	seatsPerFamily := make(map[PanelFamily]int)
	anglesPerFamily := make(map[PanelFamily]map[string]bool)
	seatsPerAngle := make(map[string]int)

	for _, seat := range plan.AllocatedSeats {
		seatsPerFamily[seat.Family]++
		if anglesPerFamily[seat.Family] == nil {
			anglesPerFamily[seat.Family] = make(map[string]bool)
		}
		anglesPerFamily[seat.Family][seat.AngleID] = true
		seatsPerAngle[seat.AngleID]++

		// Verify stable ID structure: not rectangular array indices.
		expectedID := fmt.Sprintf("seat-%s-%s", seat.AngleID, seat.Family)
		if seat.ID != expectedID {
			t.Errorf("seat ID mismatch: got %q, want %q", seat.ID, expectedID)
		}
		if !strings.HasPrefix(seat.Label, "P") {
			t.Errorf("expected anonymized P-label, got %q", seat.Label)
		}
	}

	// Verify capped families: Luna, Haiku, Gemini must have exactly 3 seats each.
	for _, capped := range CappedFamilies {
		count := seatsPerFamily[capped]
		if count != DefaultFamilySeatCap {
			t.Errorf("capped family %s: want %d seats, got %d", capped, DefaultFamilySeatCap, count)
		}
		covered := anglesPerFamily[capped]
		if len(covered) != DefaultFamilySeatCap {
			t.Errorf("capped family %s: want %d distinct angles covered, got %d", capped, DefaultFamilySeatCap, len(covered))
		}
	}

	// Verify deterministic spread: capped families must NOT all take {a1, a2, a3}.
	// All capped families reach beyond the first three angles.
	if !anglesPerFamily[FamilyHaiku]["a4"] {
		t.Errorf("Haiku must cover a4 in 4-angle council; got angles: %v", anglesPerFamily[FamilyHaiku])
	}
	if !anglesPerFamily[FamilyGemini]["a4"] {
		t.Errorf("Gemini must cover a4 in 4-angle council; got angles: %v", anglesPerFamily[FamilyGemini])
	}
	if !anglesPerFamily[FamilyLuna]["a4"] {
		t.Errorf("Luna must not always take the first three angles; got angles: %v", anglesPerFamily[FamilyLuna])
	}

	// Verify uncapped families: MiMo Flash, DeepSeek Flash, GLM Flash, Qwen must have 4 seats each (1 per angle).
	uncappedFamilies := []PanelFamily{FamilyMiMoFlash, FamilyDeepSeekFlash, FamilyGLMFlash, FamilyQwen}
	for _, uncapped := range uncappedFamilies {
		count := seatsPerFamily[uncapped]
		if count != len(angles) {
			t.Errorf("uncapped family %s: want %d seats (full coverage), got %d", uncapped, len(angles), count)
		}
		if len(anglesPerFamily[uncapped]) != len(angles) {
			t.Errorf("uncapped family %s: missing angle coverage: %v", uncapped, anglesPerFamily[uncapped])
		}
	}

	// Verify all 4 angles have coverage.
	for _, a := range angles {
		if seatsPerAngle[a.ID] == 0 {
			t.Errorf("angle %s has no seats", a.ID)
		}
	}
}

// TestCouncilAllocation_ArbitraryAngles verifies deterministic spread across 5, 6, and 7 angles.
func TestCouncilAllocation_ArbitraryAngles(t *testing.T) {
	testCounts := []int{5, 6, 7}
	for _, count := range testCounts {
		t.Run(fmt.Sprintf("%d_angles", count), func(t *testing.T) {
			var angles []CouncilAngle
			for i := 0; i < count; i++ {
				angles = append(angles, CouncilAngle{
					ID:   fmt.Sprintf("ang%d", i+1),
					Text: fmt.Sprintf("Investigation angle %d", i+1),
				})
			}

			plan, err := PlanAllocation(AllocationRequest{
				Question: "Arbitrary angle coverage check",
				Angles:   angles,
			})
			if err != nil {
				t.Fatalf("PlanAllocation failed for %d angles: %v", count, err)
			}

			seatsPerFamily := make(map[PanelFamily]int)
			anglesPerFamily := make(map[PanelFamily]map[string]bool)
			for _, s := range plan.AllocatedSeats {
				seatsPerFamily[s.Family]++
				if anglesPerFamily[s.Family] == nil {
					anglesPerFamily[s.Family] = make(map[string]bool)
				}
				anglesPerFamily[s.Family][s.AngleID] = true
			}

			// Capped families must have exactly 3 seats.
			for _, capped := range CappedFamilies {
				if seatsPerFamily[capped] != DefaultFamilySeatCap {
					t.Errorf("family %s for %d angles: want %d seats, got %d",
						capped, count, DefaultFamilySeatCap, seatsPerFamily[capped])
				}
				if len(anglesPerFamily[capped]) != DefaultFamilySeatCap {
					t.Errorf("family %s for %d angles: angles are not distinct: %v",
						capped, count, anglesPerFamily[capped])
				}
			}

			// Uncapped families must have full coverage.
			for _, f := range DefaultPanelFamilies {
				if !f.IsCapped() && seatsPerFamily[f] != count {
					t.Errorf("uncapped family %s for %d angles: want %d seats, got %d",
						f, count, count, seatsPerFamily[f])
				}
			}
		})
	}
}

// TestCouncilAllocation_AbsentFamilyNotReplaced verifies that absent families
// are omitted rather than replaced by another family or inflating remaining caps.
func TestCouncilAllocation_AbsentFamilyNotReplaced(t *testing.T) {
	angles := []CouncilAngle{
		{ID: "a1", Text: "Angle 1"},
		{ID: "a2", Text: "Angle 2"},
		{ID: "a3", Text: "Angle 3"},
		{ID: "a4", Text: "Angle 4"},
	}

	// Provide eligible models omitting Gemini and Qwen.
	eligible := []string{
		"gpt-luna",
		"claude-haiku-5.5",
		"mimo-v2.6-flash",
		"deepseek-v4.1-flash",
	}

	plan, err := PlanAllocation(AllocationRequest{
		Question:       "Test absent family behavior",
		Angles:         angles,
		EligibleModels: eligible,
	})
	if err != nil {
		t.Fatalf("PlanAllocation failed: %v", err)
	}

	// Verify omitted families are recorded.
	omittedMap := make(map[PanelFamily]bool)
	for _, o := range plan.OmittedFamilies {
		omittedMap[o] = true
	}
	if !omittedMap[FamilyGemini] {
		t.Errorf("Gemini should be in omitted families: %v", plan.OmittedFamilies)
	}
	if !omittedMap[FamilyQwen] {
		t.Errorf("Qwen should be in omitted families: %v", plan.OmittedFamilies)
	}

	// Verify Luna and Haiku caps are still strictly 3 (NOT increased to replace Gemini).
	seatsPerFamily := make(map[PanelFamily]int)
	for _, s := range plan.AllocatedSeats {
		seatsPerFamily[s.Family]++
	}
	if seatsPerFamily[FamilyLuna] != 3 {
		t.Errorf("Luna seats should be 3, got %d", seatsPerFamily[FamilyLuna])
	}
	if seatsPerFamily[FamilyHaiku] != 3 {
		t.Errorf("Haiku seats should be 3, got %d", seatsPerFamily[FamilyHaiku])
	}
	if seatsPerFamily[FamilyGemini] != 0 {
		t.Errorf("Gemini was absent, but has %d seats", seatsPerFamily[FamilyGemini])
	}
}

// TestCouncilAllocation_PreflightQuorumFailures verifies preflight quorum requirements
// (at least 3 distinct panel model families overall, every requested angle covered).
func TestCouncilAllocation_PreflightQuorumFailures(t *testing.T) {
	angles := []CouncilAngle{
		{ID: "a1", Text: "Angle 1"},
		{ID: "a2", Text: "Angle 2"},
	}

	// Below quorum: only 2 distinct families.
	_, err := PlanAllocation(AllocationRequest{
		Question:         "Test quorum failure",
		Angles:           angles,
		EligibleFamilies: []PanelFamily{FamilyLuna, FamilyMiMoFlash},
	})
	if err == nil {
		t.Fatal("expected error for 2 families, got nil")
	}
	if !errors.Is(err, ErrInsufficientQuorum) {
		t.Fatalf("expected ErrInsufficientQuorum, got: %v", err)
	}

	// Empty question.
	_, err = PlanAllocation(AllocationRequest{
		Question: "",
		Angles:   angles,
	})
	if !errors.Is(err, ErrEmptyQuestion) {
		t.Fatalf("expected ErrEmptyQuestion, got: %v", err)
	}

	// No angles.
	_, err = PlanAllocation(AllocationRequest{
		Question: "Valid question",
		Angles:   nil,
	})
	if !errors.Is(err, ErrNoAngles) {
		t.Fatalf("expected ErrNoAngles, got: %v", err)
	}
}

// TestCouncilAllocation_BoundsClearErrors verifies that exceeding configured bounds
// (seats, report context words, total work units, angles) returns a clear error before spend,
// and does NOT silently truncate.
func TestCouncilAllocation_BoundsClearErrors(t *testing.T) {
	angles := []CouncilAngle{
		{ID: "a1", Text: "Angle 1"},
		{ID: "a2", Text: "Angle 2"},
		{ID: "a3", Text: "Angle 3"},
		{ID: "a4", Text: "Angle 4"},
	}

	// 1. MaxSeats exceeded.
	_, err := PlanAllocation(AllocationRequest{
		Question: "Test max seats bounds",
		Angles:   angles,
		Limits:   CouncilLimits{MaxSeats: 10}, // Full panel requires 25 seats
	})
	if err == nil {
		t.Fatal("expected oversized seats error, got nil")
	}
	var oversized *OversizedRequestError
	if !errors.As(err, &oversized) || oversized.Metric != "seats" {
		t.Fatalf("expected OversizedRequestError for seats, got: %v", err)
	}
	if oversized.Limit != 10 || oversized.Actual <= 10 {
		t.Errorf("oversized bounds details mismatch: limit=%d, actual=%d", oversized.Limit, oversized.Actual)
	}

	// 2. MaxReportContextWords exceeded.
	_, err = PlanAllocation(AllocationRequest{
		Question: "Test report context bounds",
		Angles:   angles,
		Limits:   CouncilLimits{MaxReportContextWords: 5000}, // 25 seats * 800 = 20,000 words
	})
	if err == nil {
		t.Fatal("expected oversized report context error, got nil")
	}
	if !errors.As(err, &oversized) || oversized.Metric != "report_context_words" {
		t.Fatalf("expected OversizedRequestError for report_context_words, got: %v", err)
	}

	// 3. MaxTotalWorkUnits exceeded.
	_, err = PlanAllocation(AllocationRequest{
		Question: "Test work units bounds",
		Angles:   angles,
		Limits:   CouncilLimits{MaxTotalWorkUnits: 100}, // 25 seats * 12 turns = 300 units
	})
	if err == nil {
		t.Fatal("expected oversized work units error, got nil")
	}
	if !errors.As(err, &oversized) || oversized.Metric != "work_units" {
		t.Fatalf("expected OversizedRequestError for work_units, got: %v", err)
	}

	// 4. MaxAngles exceeded.
	_, err = PlanAllocation(AllocationRequest{
		Question: "Test angle bounds",
		Angles:   angles,
		Limits:   CouncilLimits{MaxAngles: 2},
	})
	if err == nil {
		t.Fatal("expected oversized angles error, got nil")
	}
	if !errors.As(err, &oversized) || oversized.Metric != "angles" {
		t.Fatalf("expected OversizedRequestError for angles, got: %v", err)
	}
}

// TestReassessViability_CompletedAndDegraded verifies outcome state reassessment
// when all seats succeed (Completed) versus when a subset fails while preserving quorum (DegradedViable).
func TestReassessViability_CompletedAndDegraded(t *testing.T) {
	angles := []CouncilAngle{
		{ID: "a1", Text: "Angle 1"},
		{ID: "a2", Text: "Angle 2"},
		{ID: "a3", Text: "Angle 3"},
	}

	plan, err := PlanAllocation(AllocationRequest{
		Question:         "Test viability reassessment",
		Angles:           angles,
		EligibleFamilies: []PanelFamily{FamilyLuna, FamilyMiMoFlash, FamilyDeepSeekFlash},
	})
	if err != nil {
		t.Fatalf("PlanAllocation failed: %v", err)
	}

	// Case 1: All seats succeed.
	var resultsSuccess []SeatExecutionResult
	for _, s := range plan.AllocatedSeats {
		resultsSuccess = append(resultsSuccess, SeatExecutionResult{
			SeatID:    s.ID,
			AngleID:   s.AngleID,
			Family:    s.Family,
			ModelID:   s.ModelID,
			Report:    fmt.Sprintf("Report for %s: verified findings", s.ID),
			WordCount: 150,
		})
	}
	assessment1 := ReassessPanelViability(plan, resultsSuccess)
	if assessment1.State != StateCompleted || !assessment1.Viable {
		t.Fatalf("expected StateCompleted and Viable=true, got state=%s viable=%v", assessment1.State, assessment1.Viable)
	}
	if len(assessment1.RetainedReports) != len(plan.AllocatedSeats) {
		t.Errorf("expected %d retained reports, got %d", len(plan.AllocatedSeats), len(assessment1.RetainedReports))
	}

	// Case 2: One seat fails, but remaining seats retain >=3 families and cover all 3 angles.
	// E.g. seat for a1 from Luna fails, but MiMo Flash and DeepSeek Flash also cover a1.
	var resultsDegraded []SeatExecutionResult
	for _, s := range plan.AllocatedSeats {
		if s.ID == "seat-a1-gpt-luna" {
			resultsDegraded = append(resultsDegraded, SeatExecutionResult{
				SeatID:    s.ID,
				AngleID:   s.AngleID,
				Family:    s.Family,
				ModelID:   s.ModelID,
				Err:       errors.New("timeout"),
				ErrorText: "context deadline exceeded",
			})
		} else {
			resultsDegraded = append(resultsDegraded, SeatExecutionResult{
				SeatID:    s.ID,
				AngleID:   s.AngleID,
				Family:    s.Family,
				ModelID:   s.ModelID,
				Report:    fmt.Sprintf("Report for %s: verified findings", s.ID),
				WordCount: 150,
			})
		}
	}

	assessment2 := ReassessPanelViability(plan, resultsDegraded)
	if assessment2.State != StateDegradedViable || !assessment2.Viable {
		t.Fatalf("expected StateDegradedViable and Viable=true, got state=%s viable=%v", assessment2.State, assessment2.Viable)
	}
	if assessment2.FailedSeats != 1 || assessment2.SucceededSeats != len(plan.AllocatedSeats)-1 {
		t.Errorf("unexpected counts: failed=%d succeeded=%d", assessment2.FailedSeats, assessment2.SucceededSeats)
	}
}

// TestReassessViability_FailuresLossOfQuorumAndAngle verifies that failures causing
// family count to drop below 3 OR an angle to lose all coverage result in StatePanelFailed,
// while preserving partial evidence without labeling it viable.
func TestReassessViability_FailuresLossOfQuorumAndAngle(t *testing.T) {
	angles := []CouncilAngle{
		{ID: "a1", Text: "Angle 1"},
		{ID: "a2", Text: "Angle 2"},
		{ID: "a3", Text: "Angle 3"},
	}

	plan, err := PlanAllocation(AllocationRequest{
		Question:         "Test quorum loss",
		Angles:           angles,
		EligibleFamilies: []PanelFamily{FamilyLuna, FamilyMiMoFlash, FamilyDeepSeekFlash},
	})
	if err != nil {
		t.Fatalf("PlanAllocation failed: %v", err)
	}

	// 1. Loss of family quorum: All Luna seats fail, leaving only MiMo Flash and DeepSeek Flash (2 families < 3).
	var resultsQuorumLoss []SeatExecutionResult
	for _, s := range plan.AllocatedSeats {
		if s.Family == FamilyLuna {
			resultsQuorumLoss = append(resultsQuorumLoss, SeatExecutionResult{
				SeatID:    s.ID,
				AngleID:   s.AngleID,
				Family:    s.Family,
				ModelID:   s.ModelID,
				Err:       errors.New("provider outage"),
				ErrorText: "provider outage",
			})
		} else {
			resultsQuorumLoss = append(resultsQuorumLoss, SeatExecutionResult{
				SeatID:    s.ID,
				AngleID:   s.AngleID,
				Family:    s.Family,
				ModelID:   s.ModelID,
				Report:    "Report findings",
				WordCount: 100,
			})
		}
	}

	assessmentQuorum := ReassessPanelViability(plan, resultsQuorumLoss)
	if assessmentQuorum.State != StatePanelFailed || assessmentQuorum.Viable {
		t.Fatalf("expected StatePanelFailed and Viable=false, got state=%s viable=%v", assessmentQuorum.State, assessmentQuorum.Viable)
	}
	// Verify partial evidence is preserved even below quorum.
	if len(assessmentQuorum.RetainedReports) == 0 {
		t.Errorf("partial evidence must be retained even below quorum")
	}

	// 2. Loss of angle coverage: All seats for angle a3 fail.
	var resultsAngleLoss []SeatExecutionResult
	for _, s := range plan.AllocatedSeats {
		if s.AngleID == "a3" {
			resultsAngleLoss = append(resultsAngleLoss, SeatExecutionResult{
				SeatID:    s.ID,
				AngleID:   s.AngleID,
				Family:    s.Family,
				ModelID:   s.ModelID,
				Err:       errors.New("seat error"),
				ErrorText: "failed",
			})
		} else {
			resultsAngleLoss = append(resultsAngleLoss, SeatExecutionResult{
				SeatID:    s.ID,
				AngleID:   s.AngleID,
				Family:    s.Family,
				ModelID:   s.ModelID,
				Report:    "Report findings",
				WordCount: 100,
			})
		}
	}

	assessmentAngle := ReassessPanelViability(plan, resultsAngleLoss)
	if assessmentAngle.State != StatePanelFailed || assessmentAngle.Viable {
		t.Fatalf("expected StatePanelFailed and Viable=false on lost angle, got state=%s viable=%v", assessmentAngle.State, assessmentAngle.Viable)
	}
	if len(assessmentAngle.MissingAngles) != 1 || assessmentAngle.MissingAngles[0] != "a3" {
		t.Errorf("expected missing angle a3, got: %v", assessmentAngle.MissingAngles)
	}
}

// TestEscalationApprovalBinding verifies that oracle escalation approvals
// are strictly bound to the target task ID and report revision hash,
// eliminating the legacy boolean unlock bug.
func TestEscalationApprovalBinding(t *testing.T) {
	taskID := "task-council-100"
	question := "What is the optimal caching architecture?"
	evidence := "src/cache/cache.go: lines 10-50"
	reports := []RetainedReport{
		{SeatID: "seat-a1-gpt-luna", Label: "P1", AngleID: "a1", Family: FamilyLuna, Text: "Finding 1"},
		{SeatID: "seat-a1-mimo-flash", Label: "P2", AngleID: "a1", Family: FamilyMiMoFlash, Text: "Finding 2"},
		{SeatID: "seat-a2-deepseek-flash", Label: "P3", AngleID: "a2", Family: FamilyDeepSeekFlash, Text: "Finding 3"},
	}

	correctHash := ComputeReportRevisionHash(taskID, question, evidence, reports)

	validApproval := &CouncilApproval{
		CouncilID:          "council-xyz",
		TaskID:             taskID,
		ReportRevisionHash: correctHash,
		Approved:           true,
		GrantedAt:          time.Now(),
		ApprovedBy:         "user-explicit",
		Reason:             "Council completed with ambiguity requiring direct oracle",
	}

	validToken := MintEscalationAuthority("council-xyz", taskID, correctHash)

	// 0. Missing runtime authority token (unmarshaled session metadata alone) fails!
	if err := ValidateEscalationApproval(validApproval, EscalationAuthorityToken{}, taskID, correctHash, StateCompleted); !errors.Is(err, ErrApprovalNotGranted) {
		t.Fatalf("unmarshaled metadata alone must not grant authority: expected ErrApprovalNotGranted, got: %v", err)
	}

	// 1. Valid approval passes for StateCompleted and StateDegradedViable.
	if err := ValidateEscalationApproval(validApproval, validToken, taskID, correctHash, StateCompleted); err != nil {
		t.Fatalf("valid approval should pass for StateCompleted: %v", err)
	}
	if err := ValidateEscalationApproval(validApproval, validToken, taskID, correctHash, StateDegradedViable); err != nil {
		t.Fatalf("valid approval should pass for StateDegradedViable: %v", err)
	}

	// 2. Mismatched task ID: Question A cannot authorize spending on Question B.
	err := ValidateEscalationApproval(validApproval, validToken, "task-other-200", correctHash, StateCompleted)
	if err == nil {
		t.Fatal("expected error on mismatched task ID, got nil")
	}
	var bindErr *ApprovalBindingError
	if !errors.As(err, &bindErr) || bindErr.Field != "task_id" {
		t.Fatalf("expected ApprovalBindingError on task_id, got: %v", err)
	}

	// 3. Mismatched report revision hash: modification to report text invalidates approval.
	tamperedReports := []RetainedReport{
		{SeatID: "seat-a1-gpt-luna", Label: "P1", AngleID: "a1", Family: FamilyLuna, Text: "Altered Finding 1"},
		reports[1],
		reports[2],
	}
	tamperedHash := ComputeReportRevisionHash(taskID, question, evidence, tamperedReports)
	err = ValidateEscalationApproval(validApproval, validToken, taskID, tamperedHash, StateCompleted)
	if err == nil {
		t.Fatal("expected error on mismatched report hash, got nil")
	}
	if !errors.As(err, &bindErr) || bindErr.Field != "report_revision_hash" {
		t.Fatalf("expected ApprovalBindingError on report_revision_hash, got: %v", err)
	}

	// 4. Unapproved token.
	unapproved := *validApproval
	unapproved.Approved = false
	if err := ValidateEscalationApproval(&unapproved, validToken, taskID, correctHash, StateCompleted); !errors.Is(err, ErrApprovalNotGranted) {
		t.Fatalf("expected ErrApprovalNotGranted, got: %v", err)
	}

	// 5. Ineligible state: PanelFailed cannot be escalated via normal post-spend escalation.
	if err := ValidateEscalationApproval(validApproval, validToken, taskID, correctHash, StatePanelFailed); !errors.Is(err, ErrApprovalInvalidState) {
		t.Fatalf("expected ErrApprovalInvalidState for StatePanelFailed, got: %v", err)
	}
}

// TestCancellationNoFallback verifies that cancelled or denied tasks stop execution
// and cannot fall back or approve escalation.
func TestCancellationNoFallback(t *testing.T) {
	taskID := "task-cancel-300"
	hash := "somehash"
	approval := &CouncilApproval{
		CouncilID:          "c-1",
		TaskID:             taskID,
		ReportRevisionHash: hash,
		Approved:           true,
	}
	token := MintEscalationAuthority("c-1", taskID, hash)

	// Cancellation stops; escalation validation returns ErrApprovalCancelled.
	if err := ValidateEscalationApproval(approval, token, taskID, hash, StateCancelled); !errors.Is(err, ErrApprovalCancelled) {
		t.Fatalf("expected ErrApprovalCancelled for StateCancelled, got: %v", err)
	}
	if err := ValidateEscalationApproval(approval, token, taskID, hash, StateDenied); !errors.Is(err, ErrApprovalCancelled) {
		t.Fatalf("expected ErrApprovalCancelled for StateDenied, got: %v", err)
	}

	// Verify state properties.
	if !StateCancelled.IsTerminalStop() || !StateDenied.IsTerminalStop() {
		t.Error("cancelled and denied must be terminal stop states")
	}
	if StateCancelled.AllowsDirectOracleFallback() || StateDenied.AllowsDirectOracleFallback() {
		t.Error("cancelled and denied must NOT allow direct oracle fallback")
	}

	// Direct oracle fallback is ONLY allowed for StateUnavailableBeforeSpend.
	if !StateUnavailableBeforeSpend.AllowsDirectOracleFallback() {
		t.Error("StateUnavailableBeforeSpend must allow direct oracle fallback")
	}
	if StatePanelFailed.AllowsDirectOracleFallback() {
		t.Error("StatePanelFailed must NOT allow direct oracle fallback")
	}
}

// TestSynthesisRetryContract verifies the contract for retrying synthesis
// from retained reports when synthesis coordinator fails, avoiding re-running panel seats.
func TestSynthesisRetryContract(t *testing.T) {
	taskID := "task-retry-400"
	question := "Retry test question"
	evidence := "evidence text"
	reports := []RetainedReport{
		{SeatID: "seat-a1-gpt-luna", Label: "P1", AngleID: "a1", Family: FamilyLuna, Text: "Report 1"},
		{SeatID: "seat-a1-mimo-flash", Label: "P2", AngleID: "a1", Family: FamilyMiMoFlash, Text: "Report 2"},
		{SeatID: "seat-a2-deepseek-flash", Label: "P3", AngleID: "a2", Family: FamilyDeepSeekFlash, Text: "Report 3"},
	}

	spec, err := NewSynthesisRetrySpec("council-retry-1", taskID, question, evidence, reports, StrategyNormal, nil)
	if err != nil {
		t.Fatalf("NewSynthesisRetrySpec failed: %v", err)
	}

	// Valid retry with StateSynthesisFailed.
	if err := ValidateSynthesisRetry(spec, StateSynthesisFailed); err != nil {
		t.Fatalf("ValidateSynthesisRetry failed: %v", err)
	}

	// Reject retry if prior state is not StateSynthesisFailed.
	if err := ValidateSynthesisRetry(spec, StateCompleted); err == nil {
		t.Fatal("expected error when prior state is StateCompleted, got nil")
	}
	if err := ValidateSynthesisRetry(spec, StateCancelled); err == nil {
		t.Fatal("expected error when prior state is StateCancelled, got nil")
	}

	// Tampered report in spec causes hash mismatch.
	tamperedSpec := spec
	tamperedSpec.ReportRevisionHash = "tampered-hash"
	if err := ValidateSynthesisRetry(tamperedSpec, StateSynthesisFailed); err == nil {
		t.Fatal("expected hash mismatch error, got nil")
	}

	// Insufficient quorum in retained reports fails construction.
	fewReports := []RetainedReport{
		{SeatID: "seat-1", Family: FamilyLuna, Text: "R1"},
		{SeatID: "seat-2", Family: FamilyLuna, Text: "R2"},
	}
	if _, err := NewSynthesisRetrySpec("c2", taskID, question, evidence, fewReports, StrategyNormal, nil); !errors.Is(err, ErrInsufficientQuorum) {
		t.Fatalf("expected ErrInsufficientQuorum on few reports, got: %v", err)
	}
}

// TestPersistedCouncilRecordMeta verifies that CouncilRecordMeta serializes
// and deserializes cleanly to/from JSON for storage in session records.
func TestPersistedCouncilRecordMeta(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	meta := CouncilRecordMeta{
		CouncilID:          "council-sess-99",
		TaskID:             "task-99",
		Question:           "Persisted record test",
		EvidenceRef:        "sessions/evidence-99.txt",
		Outcome:            StateDegradedViable,
		SynthesisStrategy:  StrategyOracleLed,
		ReportRevisionHash: "sha256-abc123def456",
		SeatsTotal:         15,
		SeatsSucceeded:     14,
		SeatsFailed:        1,
		FailedSeatIDs:      []string{"seat-a2-qwen"},
		RetainedReports: []RetainedReport{
			{SeatID: "seat-a1-gpt-luna", Label: "P1", AngleID: "a1", Family: FamilyLuna, Text: "Summary finding"},
		},
		Approval: &CouncilApproval{
			CouncilID:          "council-sess-99",
			TaskID:             "task-99",
			ReportRevisionHash: "sha256-abc123def456",
			Approved:           true,
			GrantedAt:          now,
			ApprovedBy:         "user",
		},
		Usage:       &llm.Usage{In: 4500, Out: 1200},
		CreatedAt:   now,
		CompletedAt: now.Add(45 * time.Second),
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("failed to marshal CouncilRecordMeta: %v", err)
	}

	var decoded CouncilRecordMeta
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal CouncilRecordMeta: %v", err)
	}

	if decoded.CouncilID != meta.CouncilID || decoded.Outcome != meta.Outcome {
		t.Errorf("decoded metadata mismatch: got %+v", decoded)
	}
	if decoded.Approval == nil || !decoded.Approval.Approved {
		t.Errorf("decoded approval missing or not approved: %+v", decoded.Approval)
	}
	if len(decoded.RetainedReports) != 1 || decoded.RetainedReports[0].SeatID != "seat-a1-gpt-luna" {
		t.Errorf("decoded retained reports mismatch: %+v", decoded.RetainedReports)
	}
	if decoded.Usage == nil || decoded.Usage.In != 4500 {
		t.Errorf("decoded usage mismatch: %+v", decoded.Usage)
	}
}

func TestPanelFamilyForModel(t *testing.T) {
	// Authorized panel models
	panelTests := []struct {
		input  string
		family PanelFamily
	}{
		{"mimo-v2.6-flash", FamilyMiMoFlash},
		{"xiaomi/mimo-v2.6-flash", FamilyMiMoFlash},
		{"openrouter/xiaomi/mimo-v2.6-flash", FamilyMiMoFlash},
		{"claude-haiku-5.5", FamilyHaiku},
		{"anthropic/claude-haiku-5.5", FamilyHaiku},
		{"gemini-3.8-flash", FamilyGemini},
		{"google/gemini-3.8-flash", FamilyGemini},
		{"gpt-luna", FamilyLuna},
		{"deepseek-v4.1-flash", FamilyDeepSeekFlash},
		{"glm-5.3-flash", FamilyGLMFlash},
		{"qwen3.8-flash", FamilyQwen},
	}
	for _, tc := range panelTests {
		fam, ok := PanelFamilyForModel(tc.input)
		if !ok || fam != tc.family {
			t.Errorf("PanelFamilyForModel(%q): got (%s, %v), want (%s, true)", tc.input, fam, ok, tc.family)
		}
	}

	// Non-panel models must NEVER be mapped to panel families
	nonPanel := []string{
		"mimo-v2.6-pro",
		"xiaomi/mimo-v2.6-pro",
		"openrouter/xiaomi/mimo-v2.6-pro",
		"claude-sonnet-5.5",
		"anthropic/claude-sonnet-5.5",
		"claude-opus-5.5",
		"claude-fable-5.1",
		"gpt-6.1-sol",
		"gpt-sol",
		"gpt-6-astra",
		"gpt-astra",
		"unknown-model",
	}
	for _, modelID := range nonPanel {
		fam, ok := PanelFamilyForModel(modelID)
		if ok {
			t.Errorf("non-panel model %q must not map to panel family, got: %s", modelID, fam)
		}
	}
}

func TestCouncilAllocation_CappedFamiliesSpread(t *testing.T) {
	// Only 3 capped families: Luna, Haiku, Gemini (3 seats each = 9 seats max)
	cappedOnly := []PanelFamily{FamilyLuna, FamilyHaiku, FamilyGemini}

	// 8 angles: 9 seats must cover all 8 angles without holes
	t.Run("8_angles_full_coverage", func(t *testing.T) {
		var angles []CouncilAngle
		for i := 1; i <= 8; i++ {
			angles = append(angles, CouncilAngle{ID: fmt.Sprintf("a%d", i), Text: fmt.Sprintf("Angle %d", i)})
		}
		plan, err := PlanAllocation(AllocationRequest{
			Question:         "8 angle test",
			Angles:           angles,
			EligibleFamilies: cappedOnly,
		})
		if err != nil {
			t.Fatalf("PlanAllocation failed for 8 angles with capped-only families: %v", err)
		}
		if len(plan.AllocatedSeats) != 9 {
			t.Fatalf("expected 9 seats (3 per family), got %d", len(plan.AllocatedSeats))
		}
		covered := make(map[string]int)
		for _, s := range plan.AllocatedSeats {
			covered[s.AngleID]++
		}
		for _, a := range angles {
			if covered[a.ID] == 0 {
				t.Errorf("angle %s was left uncovered in 8-angle capped council", a.ID)
			}
		}
	})

	// 9 angles: 9 seats must cover all 9 angles exactly
	t.Run("9_angles_full_coverage", func(t *testing.T) {
		var angles []CouncilAngle
		for i := 1; i <= 9; i++ {
			angles = append(angles, CouncilAngle{ID: fmt.Sprintf("a%d", i), Text: fmt.Sprintf("Angle %d", i)})
		}
		plan, err := PlanAllocation(AllocationRequest{
			Question:         "9 angle test",
			Angles:           angles,
			EligibleFamilies: cappedOnly,
		})
		if err != nil {
			t.Fatalf("PlanAllocation failed for 9 angles with capped-only families: %v", err)
		}
		covered := make(map[string]int)
		for _, s := range plan.AllocatedSeats {
			covered[s.AngleID]++
		}
		for _, a := range angles {
			if covered[a.ID] != 1 {
				t.Errorf("angle %s should have exactly 1 seat, got %d", a.ID, covered[a.ID])
			}
		}
	})

	// 10 angles with only capped families: cannot cover 10 angles with 9 seats.
	// Must fail with ErrUncoveredAngle and NOT silently truncate angles or exceed caps!
	t.Run("10_angles_unavailable_not_truncated", func(t *testing.T) {
		var angles []CouncilAngle
		for i := 1; i <= 10; i++ {
			angles = append(angles, CouncilAngle{ID: fmt.Sprintf("a%d", i), Text: fmt.Sprintf("Angle %d", i)})
		}
		_, err := PlanAllocation(AllocationRequest{
			Question:         "10 angle test",
			Angles:           angles,
			EligibleFamilies: cappedOnly,
		})
		if err == nil || !errors.Is(err, ErrUncoveredAngle) {
			t.Fatalf("expected ErrUncoveredAngle for 10 angles with only capped families, got: %v", err)
		}
	})
}

func TestValidateSynthesisRetryMeta(t *testing.T) {
	taskID := "retry-task-1"
	question := "Retry question"
	evidence := "evidence-ref-1"
	reports := []RetainedReport{
		{SeatID: "s1", Family: FamilyLuna, Text: "rep 1"},
		{SeatID: "s2", Family: FamilyMiMoFlash, Text: "rep 2"},
		{SeatID: "s3", Family: FamilyDeepSeekFlash, Text: "rep 3"},
	}
	hash := ComputeReportRevisionHash(taskID, question, evidence, reports)

	meta := &CouncilRecordMeta{
		TaskID:             taskID,
		Question:           question,
		EvidenceRef:        evidence,
		Outcome:            StateSynthesisFailed,
		ReportRevisionHash: hash,
		RetainedReports:    reports,
	}

	if err := ValidateSynthesisRetryMeta(meta, hash); err != nil {
		t.Fatalf("expected ValidateSynthesisRetryMeta to succeed, got: %v", err)
	}

	// Mismatched prior hash
	if err := ValidateSynthesisRetryMeta(meta, "different-hash"); err == nil {
		t.Fatal("expected hash mismatch error, got nil")
	}

	// Ineligible state
	completedMeta := *meta
	completedMeta.Outcome = StateCompleted
	if err := ValidateSynthesisRetryMeta(&completedMeta, hash); err == nil {
		t.Fatal("expected state error for StateCompleted, got nil")
	}
}
