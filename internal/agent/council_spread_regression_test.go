package agent

import (
	"fmt"
	"testing"
)

func TestCappedSeatsInterleaveWithoutDuplicateAngles(t *testing.T) {
	for n := 4; n <= 9; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			angles := make([]CouncilAngle, n)
			for i := range angles {
				angles[i] = CouncilAngle{ID: fmt.Sprintf("a%d", i+1), Text: "independent angle"}
			}
			plan, err := PlanAllocation(AllocationRequest{Question: "test", Angles: angles, EligibleFamilies: []PanelFamily{FamilyLuna, FamilyHaiku, FamilyGemini}})
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			counts := map[PanelFamily]int{}
			coverage := map[string]bool{}
			lunaHasLaterAngle := false
			for _, seat := range plan.AllocatedSeats {
				if seen[seat.ID] {
					t.Fatalf("duplicate seat %s", seat.ID)
				}
				seen[seat.ID] = true
				counts[seat.Family]++
				coverage[seat.AngleID] = true
				if seat.Family == FamilyLuna && seat.AngleID != "a1" && seat.AngleID != "a2" && seat.AngleID != "a3" {
					lunaHasLaterAngle = true
				}
			}
			if !lunaHasLaterAngle || len(coverage) != n {
				t.Fatalf("not spread over all angles: %v", plan.AllocatedSeats)
			}
			for family, count := range counts {
				if count != 3 {
					t.Fatalf("family %s has %d seats", family, count)
				}
			}
		})
	}
}

func TestCouncilRejectsUnknownPanelFamily(t *testing.T) {
	_, err := PlanAllocation(AllocationRequest{Question: "test", Angles: []CouncilAngle{{ID: "a1"}}, EligibleFamilies: []PanelFamily{"unknown-a", "unknown-b", "unknown-c"}})
	if err == nil {
		t.Fatal("unknown families must not establish quorum")
	}
}
