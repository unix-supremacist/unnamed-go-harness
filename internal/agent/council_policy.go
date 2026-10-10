// Package agent provides council allocation, outcome, and limits contracts (PLAN §Council).
// These helper types and policy functions establish deterministic seat allocation,
// preflight and reassessment quorum rules, bounded context errors before spend,
// typed outcome states, and task+report hash bound approvals.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/unix/ugh/internal/llm"
	"github.com/unix/ugh/internal/policy"
)

// PanelFamily represents a model family eligible for council panel seats (PLAN §Council).
type PanelFamily string

const (
	FamilyLuna          PanelFamily = "gpt-luna"
	FamilyMiMoFlash     PanelFamily = "mimo-flash"
	FamilyDeepSeekFlash PanelFamily = "deepseek-flash"
	FamilyGLMFlash      PanelFamily = "glm-flash"
	FamilyQwen          PanelFamily = "qwen"
	FamilyHaiku         PanelFamily = "claude-haiku"
	FamilyGemini        PanelFamily = "gemini-flash"
)

var (
	DefaultPanelFamilies = []PanelFamily{FamilyLuna, FamilyMiMoFlash, FamilyDeepSeekFlash, FamilyGLMFlash, FamilyQwen, FamilyHaiku, FamilyGemini}
	CappedFamilies       = []PanelFamily{FamilyLuna, FamilyHaiku, FamilyGemini}
)

const (
	DefaultFamilySeatCap     = 3
	MinCouncilQuorumFamilies = 3
	DefaultWordsPerReport    = 800
	DefaultWorkUnitsPerSeat  = 12
)

// PanelFamilyForModel resolves a model card ID, route string, or family alias to its PanelFamily
// reusing internal/policy authoritative exact route/card families. High-tier coordinators (e.g. MiMo Pro)
// or unknown models are never misclassified as panel Flash families.
func PanelFamilyForModel(modelID string) (PanelFamily, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return "", false
	}
	switch id {
	case "gpt-luna":
		return FamilyLuna, true
	case "mimo-flash", "mimo-v2.6-flash":
		return FamilyMiMoFlash, true
	case "deepseek-flash", "deepseek-v4.1-flash":
		return FamilyDeepSeekFlash, true
	case "glm-flash", "glm-5.3-flash":
		return FamilyGLMFlash, true
	case "qwen", "qwen3.8-flash":
		return FamilyQwen, true
	case "claude-haiku", "claude-haiku-5.5":
		return FamilyHaiku, true
	case "gemini-flash", "gemini-3.8-flash":
		return FamilyGemini, true
	}
	famToPanel := map[policy.Family]PanelFamily{
		policy.FamilyGPTLuna:       FamilyLuna,
		policy.FamilyMiMoFlash:     FamilyMiMoFlash,
		policy.FamilyDeepSeekFlash: FamilyDeepSeekFlash,
		policy.FamilyGLMFlash:      FamilyGLMFlash,
		policy.FamilyQwen:          FamilyQwen,
		policy.FamilyClaudeHaiku:   FamilyHaiku,
		policy.FamilyGeminiFlash:   FamilyGemini,
	}
	for fam, pFam := range famToPanel {
		info := policy.Families[fam]
		if strings.EqualFold(id, string(fam)) || strings.EqualFold(id, info.CardID) {
			return pFam, true
		}
		for _, r := range policy.ConcreteRoutes[fam] {
			if strings.EqualFold(id, r.ModelID) || strings.EqualFold(id, r.Ref()) || strings.EqualFold(id, r.RouteKey()) {
				return pFam, true
			}
		}
	}
	return "", false
}

func (f PanelFamily) DefaultModelID() string {
	switch f {
	case FamilyLuna:
		return "gpt-luna"
	case FamilyMiMoFlash:
		return "mimo-v2.6-flash"
	case FamilyDeepSeekFlash:
		return "deepseek-v4.1-flash"
	case FamilyGLMFlash:
		return "glm-5.3-flash"
	case FamilyQwen:
		return "qwen3.8-flash"
	case FamilyHaiku:
		return "claude-haiku-5.5"
	case FamilyGemini:
		return "gemini-3.8-flash"
	default:
		return string(f)
	}
}

func (f PanelFamily) IsCapped() bool {
	for _, c := range CappedFamilies {
		if f == c {
			return true
		}
	}
	return false
}

func CappedFamilyIndex(f PanelFamily) (int, bool) {
	for i, c := range CappedFamilies {
		if f == c {
			return i, true
		}
	}
	return -1, false
}

// CouncilOutcomeState models the typed lifecycle and terminal states of a council run (PLAN §Council).
type CouncilOutcomeState string

const (
	StateUnavailableBeforeSpend CouncilOutcomeState = "unavailable-before-spend"
	StateCompleted              CouncilOutcomeState = "completed"
	StateDegradedViable         CouncilOutcomeState = "degraded-viable"
	StatePanelFailed            CouncilOutcomeState = "panel-failed"
	StateSynthesisFailed        CouncilOutcomeState = "synthesis-failed"
	StateCancelled              CouncilOutcomeState = "cancelled"
	StateDenied                 CouncilOutcomeState = "denied"
)

func (s CouncilOutcomeState) IsViable() bool { return s == StateCompleted || s == StateDegradedViable }
func (s CouncilOutcomeState) AllowsDirectOracleFallback() bool {
	return s == StateUnavailableBeforeSpend
}
func (s CouncilOutcomeState) IsTerminalStop() bool { return s == StateCancelled || s == StateDenied }

type SynthesisStrategy string

const (
	StrategyNormal     SynthesisStrategy = "normal"
	StrategyEconomical SynthesisStrategy = "economical"
	StrategyOracleLed  SynthesisStrategy = "oracle-led"
)

type CouncilAngle struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Lens string `json:"lens,omitempty"`
}

type CouncilSeat struct {
	ID      string      `json:"id"`
	AngleID string      `json:"angle_id"`
	Family  PanelFamily `json:"family"`
	ModelID string      `json:"model_id"`
	Label   string      `json:"label"`
}

type CouncilLimits struct {
	MaxSeats              int `json:"max_seats,omitempty"`
	MaxReportContextWords int `json:"max_report_context_words,omitempty"`
	MaxTotalWorkUnits     int `json:"max_total_work_units,omitempty"`
	MaxAngles             int `json:"max_angles,omitempty"`
	MaxReportWordsPerSeat int `json:"max_report_words_per_seat,omitempty"`
}

type CouncilPlan struct {
	Question                    string         `json:"question"`
	Angles                      []CouncilAngle `json:"angles"`
	AllocatedSeats              []CouncilSeat  `json:"allocated_seats"`
	OmittedFamilies             []PanelFamily  `json:"omitted_families,omitempty"`
	DistinctFamiliesCount       int            `json:"distinct_families_count"`
	EstimatedReportContextWords int            `json:"estimated_report_context_words"`
	EstimatedWorkUnits          int            `json:"estimated_work_units"`
}

var (
	ErrInsufficientQuorum   = errors.New("council: insufficient panel families for quorum (minimum 3 distinct families required)")
	ErrUncoveredAngle       = errors.New("council: one or more requested angles have no seat coverage")
	ErrEmptyQuestion        = errors.New("council: question cannot be empty")
	ErrNoAngles             = errors.New("council: at least one angle is required")
	ErrApprovalNotGranted   = errors.New("council: escalation approval not granted")
	ErrApprovalCancelled    = errors.New("council: escalation cannot be approved for cancelled or denied task")
	ErrApprovalInvalidState = errors.New("council: escalation approval requires completed or degraded-viable state")
)

type OversizedRequestError struct {
	Metric string `json:"metric"`
	Limit  int    `json:"limit"`
	Actual int    `json:"actual"`
	Detail string `json:"detail"`
}

func (e *OversizedRequestError) Error() string {
	return fmt.Sprintf("council bounds exceeded: %s limit is %d, requested %d: %s", e.Metric, e.Limit, e.Actual, e.Detail)
}

type ApprovalBindingError struct {
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Message  string `json:"message"`
}

func (e *ApprovalBindingError) Error() string {
	return fmt.Sprintf("council approval binding error (%s): %s", e.Field, e.Message)
}

type AllocationRequest struct {
	Question         string
	Angles           []CouncilAngle
	EligibleModels   []string
	EligibleFamilies []PanelFamily
	ModelMap         map[PanelFamily]string
	Limits           CouncilLimits
}

// PlanAllocation validates quorum and bounds, allocates stable seats with deterministic
// spread for capped families across arbitrary >=4 angles, and reports oversized errors before spend.
func PlanAllocation(req AllocationRequest) (*CouncilPlan, error) {
	q := strings.TrimSpace(req.Question)
	if q == "" {
		return nil, ErrEmptyQuestion
	}
	if len(req.Angles) == 0 {
		return nil, ErrNoAngles
	}
	if req.Limits.MaxAngles > 0 && len(req.Angles) > req.Limits.MaxAngles {
		return nil, &OversizedRequestError{Metric: "angles", Limit: req.Limits.MaxAngles, Actual: len(req.Angles), Detail: "requested angles count exceeds max"}
	}

	angles := make([]CouncilAngle, len(req.Angles))
	seenAngleIDs := make(map[string]bool, len(req.Angles))
	for i, a := range req.Angles {
		id := strings.TrimSpace(a.ID)
		if id == "" {
			id = fmt.Sprintf("a%d", i+1)
		}
		if seenAngleIDs[id] {
			return nil, fmt.Errorf("council: duplicate angle ID %q", id)
		}
		seenAngleIDs[id] = true
		angles[i] = CouncilAngle{ID: id, Text: strings.TrimSpace(a.Text), Lens: strings.TrimSpace(a.Lens)}
	}

	famModelMap := make(map[PanelFamily]string)
	for k, v := range req.ModelMap {
		famModelMap[k] = v
	}

	var eligibleFamilies []PanelFamily
	seenFamilies := make(map[PanelFamily]bool)
	addFamily := func(fam PanelFamily, modelID string) {
		if !seenFamilies[fam] {
			seenFamilies[fam] = true
			eligibleFamilies = append(eligibleFamilies, fam)
			if _, exists := famModelMap[fam]; !exists && modelID != "" {
				famModelMap[fam] = modelID
			}
		}
	}

	if len(req.EligibleFamilies) > 0 {
		for _, f := range req.EligibleFamilies {
			if !slices.Contains(DefaultPanelFamilies, f) {
				return nil, fmt.Errorf("council: unknown panel family %q", f)
			}
			addFamily(f, f.DefaultModelID())
		}
	} else if len(req.EligibleModels) > 0 {
		for _, m := range req.EligibleModels {
			if fam, ok := PanelFamilyForModel(m); ok {
				addFamily(fam, m)
			}
		}
	} else {
		for _, f := range DefaultPanelFamilies {
			addFamily(f, f.DefaultModelID())
		}
	}

	if len(eligibleFamilies) < MinCouncilQuorumFamilies {
		return nil, fmt.Errorf("%w: got %d distinct families, need at least %d", ErrInsufficientQuorum, len(eligibleFamilies), MinCouncilQuorumFamilies)
	}

	var omittedFamilies []PanelFamily
	for _, f := range DefaultPanelFamilies {
		if !seenFamilies[f] {
			omittedFamilies = append(omittedFamilies, f)
		}
	}

	numAngles := len(angles)
	var allocatedSeats []CouncilSeat
	angleCoverage := make(map[string]int, numAngles)

	for _, fam := range eligibleFamilies {
		modelID := famModelMap[fam]
		if modelID == "" {
			modelID = fam.DefaultModelID()
		}

		if !fam.IsCapped() || numAngles <= DefaultFamilySeatCap {
			for _, angle := range angles {
				allocatedSeats = append(allocatedSeats, CouncilSeat{
					ID:      fmt.Sprintf("seat-%s-%s", angle.ID, fam),
					AngleID: angle.ID,
					Family:  fam,
					ModelID: modelID,
				})
				angleCoverage[angle.ID]++
			}
		} else {
			// Interleave families across angles. Avoid repeating an angle for
			// one family when the angle count shares a factor with the stride.
			fIdx, _ := CappedFamilyIndex(fam)
			selectedAngleIndices := make([]int, DefaultFamilySeatCap)
			selected := make(map[int]bool, DefaultFamilySeatCap)
			for k := 0; k < DefaultFamilySeatCap; k++ {
				idx := (k*len(CappedFamilies) + fIdx + numAngles - 1) % numAngles
				for selected[idx] {
					idx = (idx + 1) % numAngles
				}
				selected[idx] = true
				selectedAngleIndices[k] = idx
			}
			sort.Ints(selectedAngleIndices)

			for _, idx := range selectedAngleIndices {
				angle := angles[idx]
				allocatedSeats = append(allocatedSeats, CouncilSeat{
					ID:      fmt.Sprintf("seat-%s-%s", angle.ID, fam),
					AngleID: angle.ID,
					Family:  fam,
					ModelID: modelID,
				})
				angleCoverage[angle.ID]++
			}
		}
	}

	// Verify preflight angle coverage: every requested angle must have at least one seat.
	// When >= 10 angles with only capped families available, quorum/coverage cannot be met;
	// fail with clear error rather than silently truncating angles or exceeding family caps.
	for _, angle := range angles {
		if angleCoverage[angle.ID] == 0 {
			return nil, fmt.Errorf("%w: angle %q has no eligible seat", ErrUncoveredAngle, angle.ID)
		}
	}

	for i := range allocatedSeats {
		allocatedSeats[i].Label = fmt.Sprintf("P%d", i+1)
	}

	totalSeats := len(allocatedSeats)
	if req.Limits.MaxSeats > 0 && totalSeats > req.Limits.MaxSeats {
		return nil, &OversizedRequestError{Metric: "seats", Limit: req.Limits.MaxSeats, Actual: totalSeats, Detail: fmt.Sprintf("requires %d seats, max %d", totalSeats, req.Limits.MaxSeats)}
	}

	wordsPerSeat := req.Limits.MaxReportWordsPerSeat
	if wordsPerSeat <= 0 {
		wordsPerSeat = DefaultWordsPerReport
	}
	estReportWords := totalSeats * wordsPerSeat
	if req.Limits.MaxReportContextWords > 0 && estReportWords > req.Limits.MaxReportContextWords {
		return nil, &OversizedRequestError{Metric: "report_context_words", Limit: req.Limits.MaxReportContextWords, Actual: estReportWords, Detail: fmt.Sprintf("estimated %d words, max %d", estReportWords, req.Limits.MaxReportContextWords)}
	}

	estWorkUnits := totalSeats * DefaultWorkUnitsPerSeat
	if req.Limits.MaxTotalWorkUnits > 0 && estWorkUnits > req.Limits.MaxTotalWorkUnits {
		return nil, &OversizedRequestError{Metric: "work_units", Limit: req.Limits.MaxTotalWorkUnits, Actual: estWorkUnits, Detail: fmt.Sprintf("estimated %d work units, max %d", estWorkUnits, req.Limits.MaxTotalWorkUnits)}
	}

	return &CouncilPlan{
		Question:                    q,
		Angles:                      angles,
		AllocatedSeats:              allocatedSeats,
		OmittedFamilies:             omittedFamilies,
		DistinctFamiliesCount:       len(eligibleFamilies),
		EstimatedReportContextWords: estReportWords,
		EstimatedWorkUnits:          estWorkUnits,
	}, nil
}

type RetainedReport struct {
	SeatID    string      `json:"seat_id"`
	Label     string      `json:"label"`
	AngleID   string      `json:"angle_id"`
	Family    PanelFamily `json:"family"`
	ModelID   string      `json:"model_id"`
	Text      string      `json:"text"`
	WordCount int         `json:"word_count,omitempty"`
}

type SeatExecutionResult struct {
	SeatID    string      `json:"seat_id"`
	AngleID   string      `json:"angle_id"`
	Family    PanelFamily `json:"family"`
	ModelID   string      `json:"model_id"`
	Report    string      `json:"report,omitempty"`
	WordCount int         `json:"word_count,omitempty"`
	Err       error       `json:"-"`
	ErrorText string      `json:"error,omitempty"`
}

type ViabilityAssessment struct {
	State              CouncilOutcomeState `json:"state"`
	Viable             bool                `json:"viable"`
	TotalSeats         int                 `json:"total_seats"`
	SucceededSeats     int                 `json:"succeeded_seats"`
	FailedSeats        int                 `json:"failed_seats"`
	DistinctFamilies   int                 `json:"distinct_families"`
	CoveredAngles      int                 `json:"covered_angles"`
	TotalAngles        int                 `json:"total_angles"`
	MissingAngles      []string            `json:"missing_angles,omitempty"`
	SuccessfulFamilies []PanelFamily       `json:"successful_families"`
	RetainedReports    []RetainedReport    `json:"retained_reports,omitempty"`
	FailedSeatIDs      []string            `json:"failed_seat_ids,omitempty"`
	Reason             string              `json:"reason,omitempty"`
}

func ReassessPanelViability(plan *CouncilPlan, results []SeatExecutionResult) ViabilityAssessment {
	if plan == nil {
		return ViabilityAssessment{State: StatePanelFailed, Viable: false, Reason: "nil plan"}
	}
	labelBySeat := make(map[string]string, len(plan.AllocatedSeats))
	for _, s := range plan.AllocatedSeats {
		labelBySeat[s.ID] = s.Label
	}

	var succeeded []SeatExecutionResult
	var failedSeatIDs []string
	var retainedReports []RetainedReport
	successfulFamiliesMap := make(map[PanelFamily]bool)
	angleSuccessCoverage := make(map[string]int)

	for _, r := range results {
		if r.Err == nil && strings.TrimSpace(r.ErrorText) == "" && strings.TrimSpace(r.Report) != "" {
			succeeded = append(succeeded, r)
			successfulFamiliesMap[r.Family] = true
			angleSuccessCoverage[r.AngleID]++
			lbl := labelBySeat[r.SeatID]
			if lbl == "" {
				lbl = fmt.Sprintf("P%d", len(retainedReports)+1)
			}
			retainedReports = append(retainedReports, RetainedReport{
				SeatID: r.SeatID, Label: lbl, AngleID: r.AngleID, Family: r.Family, ModelID: r.ModelID, Text: r.Report, WordCount: r.WordCount,
			})
		} else {
			failedSeatIDs = append(failedSeatIDs, r.SeatID)
		}
	}

	var missingAngles []string
	for _, a := range plan.Angles {
		if angleSuccessCoverage[a.ID] == 0 {
			missingAngles = append(missingAngles, a.ID)
		}
	}
	var successfulFamilies []PanelFamily
	for fam := range successfulFamiliesMap {
		successfulFamilies = append(successfulFamilies, fam)
	}
	sort.Slice(successfulFamilies, func(i, j int) bool { return successfulFamilies[i] < successfulFamilies[j] })

	distinctFamilies := len(successfulFamilies)
	assessment := ViabilityAssessment{
		TotalSeats:         len(plan.AllocatedSeats),
		SucceededSeats:     len(succeeded),
		FailedSeats:        len(failedSeatIDs),
		DistinctFamilies:   distinctFamilies,
		CoveredAngles:      len(plan.Angles) - len(missingAngles),
		TotalAngles:        len(plan.Angles),
		MissingAngles:      missingAngles,
		SuccessfulFamilies: successfulFamilies,
		RetainedReports:    retainedReports,
		FailedSeatIDs:      failedSeatIDs,
	}

	if distinctFamilies >= MinCouncilQuorumFamilies && len(missingAngles) == 0 {
		assessment.Viable = true
		if len(failedSeatIDs) == 0 {
			assessment.State = StateCompleted
			assessment.Reason = fmt.Sprintf("all %d seats succeeded across %d families covering all %d angles", len(succeeded), distinctFamilies, len(plan.Angles))
		} else {
			assessment.State = StateDegradedViable
			assessment.Reason = fmt.Sprintf("degraded but viable: %d succeeded, %d failed; quorum preserved (%d families, %d angles)", len(succeeded), len(failedSeatIDs), distinctFamilies, len(plan.Angles))
		}
	} else {
		assessment.Viable = false
		assessment.State = StatePanelFailed
		assessment.Reason = "panel failed to preserve quorum or angle coverage"
	}
	return assessment
}

func ComputeReportRevisionHash(taskID, question, evidence string, reports []RetainedReport) string {
	h := sha256.New()
	h.Write([]byte("task:" + strings.TrimSpace(taskID) + "\nquestion:" + strings.TrimSpace(question) + "\nevidence:" + strings.TrimSpace(evidence) + "\n"))
	sorted := make([]RetainedReport, len(reports))
	copy(sorted, reports)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SeatID < sorted[j].SeatID })
	for _, r := range sorted {
		h.Write([]byte(fmt.Sprintf("seat:%s|label:%s|angle:%s|family:%s|model:%s|text:%s\n", r.SeatID, r.Label, r.AngleID, r.Family, r.ModelID, strings.TrimSpace(r.Text))))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CouncilApproval holds persisted metadata for audit logging only.
// It does NOT confer runtime authority; authority requires an EscalationAuthorityToken.
type CouncilApproval struct {
	CouncilID          string    `json:"council_id"`
	TaskID             string    `json:"task_id"`
	ReportRevisionHash string    `json:"report_revision_hash"`
	Approved           bool      `json:"approved"`
	GrantedAt          time.Time `json:"granted_at,omitempty"`
	ApprovedBy         string    `json:"approved_by,omitempty"`
	Reason             string    `json:"reason,omitempty"`
}

// EscalationAuthorityToken is an unforgeable runtime token minted exclusively
// via harness interactive user-approval control flow.
type EscalationAuthorityToken struct {
	tokenID            string
	councilID          string
	taskID             string
	reportRevisionHash string
	mintedAt           time.Time
}

func MintEscalationAuthority(councilID, taskID, reportRevisionHash string) EscalationAuthorityToken {
	return EscalationAuthorityToken{
		tokenID:            fmt.Sprintf("auth-%d", time.Now().UnixNano()),
		councilID:          strings.TrimSpace(councilID),
		taskID:             strings.TrimSpace(taskID),
		reportRevisionHash: strings.TrimSpace(reportRevisionHash),
		mintedAt:           time.Now(),
	}
}

func (t EscalationAuthorityToken) IsValid() bool {
	return t.tokenID != "" && t.taskID != "" && t.reportRevisionHash != ""
}

func ValidateEscalationApproval(approval *CouncilApproval, token EscalationAuthorityToken, targetTaskID string, currentHash string, state CouncilOutcomeState) error {
	if approval == nil || !approval.Approved || !token.IsValid() {
		return ErrApprovalNotGranted
	}
	if state == StateCancelled || state == StateDenied {
		return ErrApprovalCancelled
	}
	if state != StateCompleted && state != StateDegradedViable {
		return ErrApprovalInvalidState
	}
	if strings.TrimSpace(token.taskID) != strings.TrimSpace(targetTaskID) || strings.TrimSpace(approval.TaskID) != strings.TrimSpace(targetTaskID) {
		return &ApprovalBindingError{Field: "task_id", Expected: targetTaskID, Actual: token.taskID, Message: fmt.Sprintf("task mismatch: %q != %q", token.taskID, targetTaskID)}
	}
	if strings.TrimSpace(token.reportRevisionHash) != strings.TrimSpace(currentHash) || strings.TrimSpace(approval.ReportRevisionHash) != strings.TrimSpace(currentHash) {
		return &ApprovalBindingError{Field: "report_revision_hash", Expected: currentHash, Actual: token.reportRevisionHash, Message: "report revision mismatch"}
	}
	return nil
}

// CouncilRecordMeta is the lightweight metadata structure persisted in existing session records (PLAN §Council).
// SynthesisRetrySpec is an alias for CouncilRecordMeta to eliminate redundant types.
type CouncilRecordMeta struct {
	CouncilID          string              `json:"council_id"`
	TaskID             string              `json:"task_id,omitempty"`
	Question           string              `json:"question"`
	Evidence           string              `json:"evidence,omitempty"`
	EvidenceRef        string              `json:"evidence_ref,omitempty"`
	Outcome            CouncilOutcomeState `json:"outcome"`
	Strategy           SynthesisStrategy   `json:"strategy,omitempty"`
	SynthesisStrategy  SynthesisStrategy   `json:"synthesis_strategy,omitempty"`
	ReportRevisionHash string              `json:"report_revision_hash,omitempty"`
	SeatsTotal         int                 `json:"seats_total,omitempty"`
	SeatsSucceeded     int                 `json:"seats_succeeded,omitempty"`
	SeatsFailed        int                 `json:"seats_failed,omitempty"`
	RetainedReports    []RetainedReport    `json:"retained_reports,omitempty"`
	FailedSeatIDs      []string            `json:"failed_seat_ids,omitempty"`
	Approval           *CouncilApproval    `json:"approval,omitempty"`
	Usage              *llm.Usage          `json:"usage,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	CompletedAt        time.Time           `json:"completed_at,omitempty"`
}

func NewSynthesisRetrySpec(councilID, taskID, question, evidence string, reports []RetainedReport, strategy SynthesisStrategy, failedSeats []string) (CouncilRecordMeta, error) {
	if len(reports) == 0 {
		return CouncilRecordMeta{}, errors.New("council: no retained reports for synthesis retry")
	}
	famSet := make(map[PanelFamily]bool)
	for _, r := range reports {
		famSet[r.Family] = true
	}
	if len(famSet) < MinCouncilQuorumFamilies {
		return CouncilRecordMeta{}, fmt.Errorf("%w: retained reports have %d families", ErrInsufficientQuorum, len(famSet))
	}
	hash := ComputeReportRevisionHash(taskID, question, evidence, reports)
	return CouncilRecordMeta{
		CouncilID:          councilID,
		TaskID:             taskID,
		Question:           question,
		Evidence:           evidence,
		EvidenceRef:        evidence,
		RetainedReports:    reports,
		ReportRevisionHash: hash,
		Strategy:           strategy,
		SynthesisStrategy:  strategy,
		FailedSeatIDs:      failedSeats,
	}, nil
}

func ValidateSynthesisRetry(meta CouncilRecordMeta, priorState CouncilOutcomeState) error {
	if priorState != StateSynthesisFailed {
		return fmt.Errorf("council: synthesis retry requires prior state %q, got %q", StateSynthesisFailed, priorState)
	}
	if len(meta.RetainedReports) == 0 {
		return errors.New("council: no retained reports in retry specification")
	}
	ev := meta.EvidenceRef
	if ev == "" {
		ev = meta.Evidence
	}
	expectedHash := ComputeReportRevisionHash(meta.TaskID, meta.Question, ev, meta.RetainedReports)
	if meta.ReportRevisionHash != expectedHash {
		return fmt.Errorf("council: report revision hash mismatch (spec %q != computed %q)", meta.ReportRevisionHash, expectedHash)
	}
	return nil
}

func ValidateSynthesisRetryMeta(meta *CouncilRecordMeta, priorHash string) error {
	if meta == nil {
		return errors.New("council: nil record metadata")
	}
	if meta.Outcome != StateSynthesisFailed {
		return fmt.Errorf("council: synthesis retry requires prior state %q, got %q", StateSynthesisFailed, meta.Outcome)
	}
	if len(meta.RetainedReports) == 0 {
		return errors.New("council: no retained reports for synthesis retry")
	}
	famSet := make(map[PanelFamily]bool)
	for _, r := range meta.RetainedReports {
		famSet[r.Family] = true
	}
	if len(famSet) < MinCouncilQuorumFamilies {
		return fmt.Errorf("%w: retained reports have %d families", ErrInsufficientQuorum, len(famSet))
	}
	ev := meta.EvidenceRef
	if ev == "" {
		ev = meta.Evidence
	}
	computed := ComputeReportRevisionHash(meta.TaskID, meta.Question, ev, meta.RetainedReports)
	if priorHash != "" && computed != priorHash {
		return fmt.Errorf("council: report revision hash mismatch (computed %q != prior %q)", computed, priorHash)
	}
	return nil
}
