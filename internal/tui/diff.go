// Colored edit diffs (M8): an expanded edit tool call renders its real
// old → new change as -/+ lines instead of the one-line summary. The diff is
// recomputed from the call's arguments, so it matches what the tool applied.
package tui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// editArgs mirrors the edit tool's argument schema.
type editArgs struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

// diffMaxLines bounds one rendered diff; longer changes end with an ellipsis.
const diffMaxLines = 60

// editDiffBlock renders the change as a short unified-style diff: trimmed
// common prefix/suffix around the removed (red -) and added (green +) lines.
func (s *State) editDiffBlock(t *toolEntry) []string {
	var a editArgs
	if err := json.Unmarshal([]byte(t.call.Args), &a); err != nil {
		return nil
	}
	old := strings.Split(strings.TrimSuffix(a.OldText, "\n"), "\n")
	new := strings.Split(strings.TrimSuffix(a.NewText, "\n"), "\n")

	pre := 0
	for pre < len(old) && pre < len(new) && old[pre] == new[pre] {
		pre++
	}
	suf := 0
	for suf < len(old)-pre && suf < len(new)-pre &&
		old[len(old)-1-suf] == new[len(new)-1-suf] {
		suf++
	}
	removed := old[pre : len(old)-suf]
	added := new[pre : len(new)-suf]

	var lines []string
	line := func(mark, color string, text string) {
		if color == "red" {
			lines = append(lines, s.pal.red.Render(mark+" "+text))
		} else {
			lines = append(lines, s.pal.green.Render(mark+" "+text))
		}
	}
	for i := 0; i < pre && len(lines) < 3; i++ { // a little context before
		line(" ", "plain", old[i])
	}
	if pre > 3 {
		lines = append(lines, s.pal.dim.Render("  …"))
	}
	for _, l := range removed {
		line("-", "red", l)
	}
	for _, l := range added {
		line("+", "green", l)
	}
	if rem := pre + len(removed) + suf; len(old) > rem && len(lines) < diffMaxLines { // context after
		tail := old[len(old)-suf:]
		for i, l := range tail {
			if len(lines) >= diffMaxLines {
				break
			}
			if i >= 3 {
				lines = append(lines, s.pal.dim.Render(fmt.Sprintf("  … (+%d context lines)", len(tail)-i)))
				break
			}
			line(" ", "plain", l)
		}
	}
	if len(lines) > diffMaxLines {
		lines = append(lines[:diffMaxLines], s.pal.dim.Render(fmt.Sprintf("  … [%d more diff lines]", len(lines)-diffMaxLines)))
	}
	return lines
}
