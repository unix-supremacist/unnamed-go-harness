package static

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/unix/ugh/internal/perm"
)

// TestReplayDecisionLog replays triage over a decision log (UGH_REPLAY=path):
// tier shares, flag rates per tier and the top reasons, for tuning tiers.
func TestReplayDecisionLog(t *testing.T) {
	path := os.Getenv("UGH_REPLAY")
	if path == "" {
		t.Skip()
	}
	SetWorkspace("/home/unix/src/Craftmore4", "/tmp", os.Getenv("HOME")+"/.local/state/ugh/scratch")
	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	type stat struct{ n, jev, any int }
	stats := map[string]*stat{}
	reasons := map[string]map[string]int{}
	total := 0
	for sc.Scan() {
		var e struct {
			Tool, Detail, Cwd string
			Verdicts          []struct {
				Name    string
				Flagged bool
			}
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Detail == "output check" || len(e.Verdicts) == 0 {
			continue
		}
		v := map[string]bool{}
		hasJev := false
		for _, x := range e.Verdicts {
			v[x.Name] = x.Flagged
			hasJev = hasJev || x.Name == "jev_or"
		}
		if !hasJev {
			continue
		}
		var args json.RawMessage
		switch e.Tool {
		case "bash":
			args, _ = json.Marshal(map[string]string{"command": e.Detail})
		default:
			p := e.Detail
			if i := len(e.Tool) + 1; len(p) > i && p[:i] == e.Tool+" " {
				p = p[i:]
			}
			args, _ = json.Marshal(map[string]string{"path": p})
		}
		tier, why := Triage(perm.NewCall(e.Cwd, e.Tool, args))
		if v["static_extreme"] {
			tier = High
		}
		s := stats[tier]
		if s == nil {
			s = &stat{}
			stats[tier] = s
		}
		s.n++
		total++
		if v["jev_or"] {
			s.jev++
		}
		if v["jev_or"] || v["clef_or"] {
			s.any++
		}
		if reasons[tier] == nil {
			reasons[tier] = map[string]int{}
		}
		if len(why) > 40 {
			why = why[:40]
		}
		reasons[tier][why]++
	}
	for _, tier := range []string{Trivial, Low, Middle, High} {
		s := stats[tier]
		if s == nil {
			continue
		}
		t.Logf("%-8s %6d %5.1f%%  jev %5.1f%%  jev|clef %5.1f%%", tier, s.n, 100*float64(s.n)/float64(total), 100*float64(s.jev)/float64(s.n), 100*float64(s.any)/float64(s.n))
		type kv struct {
			k string
			v int
		}
		var top []kv
		for k, v := range reasons[tier] {
			top = append(top, kv{k, v})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].v > top[j].v })
		for i := 0; i < len(top) && i < 6; i++ {
			t.Logf("           %5d  %s", top[i].v, top[i].k)
		}
	}
}
