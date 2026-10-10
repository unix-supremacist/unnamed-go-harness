package perm

import (
	"errors"
	"testing"
)

// pol builds a policy around four classifiers covering the interesting
// shapes: a solo stopper, a prompter, a log-only watcher and an error-prone
// model classifier with a non-ignore on_error.
func pol(mode, aggregate, aggAction string) Policy {
	return Policy{
		Mode: mode, Aggregate: aggregate, AggregateAction: aggAction,
		HeadlessPrompt: "deny",
		Classifiers: map[string]ClassifierConfig{
			"static_normal":  {State: "on", OnFlag: "stop", Solo: true, Tools: []string{"*"}},
			"static_extreme": {State: "on", OnFlag: "prompt", Tools: []string{"*"}},
			"jev_or":         {State: "on", OnFlag: "prompt", Tools: []string{"*"}, TimeoutMS: 3000},
			"clef_or":        {State: "on", OnFlag: "prompt", Tools: []string{"*"}, TimeoutMS: 3000},
			"watcher":        {State: "log", OnFlag: "stop", Tools: []string{"*"}},
			"gone":           {State: "off", OnFlag: "stop", Tools: []string{"*"}},
		},
	}
}

func flag(c string) Verdict { return Verdict{Classifier: c, Flagged: true, Reason: "bad"} }
func ok(c string) Verdict   { return Verdict{Classifier: c} }
func boom(c string) Verdict { return Verdict{Classifier: c, Err: errors.New("timeout")} }

func TestDecideTable(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		agg    string
		aggAct string
		vs     []Verdict
		want   string
		flag   bool
	}{
		// classify mode: nothing flags → run.
		{"clean classify", "classify", "any", "max", nil, Allow, false},
		{"only ok verdicts", "classify", "any", "max",
			[]Verdict{ok("static_extreme"), ok("jev_or")}, Allow, false},
		// solo stopper (§4.1: static_normal always stops).
		{"solo stop", "classify", "any", "max",
			[]Verdict{flag("static_normal")}, Stop, true},
		{"solo stop beats prompt", "classify", "any", "max",
			[]Verdict{flag("static_normal"), flag("static_extreme")}, Stop, true},
		{"solo stop skips nothing in Decide", "manual", "any", "max",
			[]Verdict{flag("static_normal")}, Stop, true},
		// non-solo aggregate.
		{"extreme prompts", "classify", "any", "max",
			[]Verdict{ok("static_normal"), flag("static_extreme")}, Prompt, true},
		{"model flag prompts", "classify", "any", "max",
			[]Verdict{ok("static_normal"), flag("jev_or")}, Prompt, true},
		{"aggregate any: one of two", "classify", "any", "max",
			[]Verdict{flag("jev_or"), ok("clef_or")}, Prompt, true},
		{"aggregate all: one of two", "classify", "all", "max",
			[]Verdict{flag("jev_or"), ok("clef_or")}, Allow, true},
		{"aggregate all: two of two", "classify", "all", "max",
			[]Verdict{flag("jev_or"), flag("clef_or")}, Prompt, true},
		{"aggregate 2:3 not enough", "classify", "2:3", "max",
			[]Verdict{flag("jev_or"), ok("clef_or"), ok("static_extreme")}, Allow, true},
		{"aggregate 2:3 enough", "classify", "2:3", "max",
			[]Verdict{flag("jev_or"), flag("clef_or"), ok("static_extreme")}, Prompt, true},
		// aggregate_action overrides the flagged classifiers' on_flag.
		{"aggregate action stop", "classify", "any", "stop",
			[]Verdict{flag("jev_or")}, Stop, true},
		{"aggregate action prompt", "classify", "any", "prompt",
			[]Verdict{flag("jev_or")}, Prompt, true},
		{"max uses harshest on_flag", "classify", "any", "max",
			[]Verdict{flag("static_extreme"), flag("jev_or")}, Prompt, true},
		// state: log and off verdicts are never applied.
		{"log state ignored", "classify", "any", "max",
			[]Verdict{flag("watcher")}, Allow, false},
		{"off state ignored", "classify", "any", "max",
			[]Verdict{flag("gone")}, Allow, false},
		{"log state cannot stop", "classify", "any", "max",
			[]Verdict{flag("watcher"), flag("static_extreme")}, Prompt, true},
		// unknown classifiers are ignored (config may omit model ones).
		{"unknown classifier", "classify", "any", "max",
			[]Verdict{flag("not_in_config")}, Allow, false},
		// errors follow on_error (default ignore).
		{"error ignored by default", "classify", "any", "max",
			[]Verdict{boom("jev_or")}, Allow, false},
		{"error plus real flag", "classify", "any", "max",
			[]Verdict{boom("jev_or"), flag("static_extreme")}, Prompt, true},
		// auto mode never applies anything (verdicts still logged by engine).
		{"auto allows flagged", "auto", "any", "max",
			[]Verdict{flag("static_normal"), flag("jev_or")}, Allow, true},
		{"auto allows errors", "auto", "any", "max", []Verdict{boom("jev_or")}, Allow, false},
		{"auto flags bit without flags", "auto", "any", "max", []Verdict{ok("jev_or")}, Allow, false},
		// manual mode prompts even when clean.
		{"manual clean", "manual", "any", "max", nil, Prompt, false},
		{"manual clean keeps flag bit", "manual", "any", "max",
			[]Verdict{flag("static_extreme")}, Prompt, true},
		{"manual still stops", "manual", "any", "max",
			[]Verdict{flag("static_normal")}, Stop, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pol(tc.mode, tc.agg, tc.aggAct)
			got := Decide(p, tc.vs)
			if got.Action != tc.want {
				t.Errorf("Decide(%s, %d verdicts) = %q (%s), want %q",
					tc.mode, len(tc.vs), got.Action, got.Reason, tc.want)
			}
			if got.Flagged != tc.flag {
				t.Errorf("Flagged = %v, want %v", got.Flagged, tc.flag)
			}
			if got.Action != Allow && got.Reason == "" {
				t.Error("non-allow decision without a reason")
			}
		})
	}
}

func TestDecideOnErrorResponse(t *testing.T) {
	for _, c := range []struct{ onError, want string }{
		{"stop", Stop}, {"prompt", Prompt}, {"ignore", Allow},
	} {
		p := pol("classify", "any", "max")
		p.Classifiers["jev_or"] = ClassifierConfig{State: "on", OnError: c.onError, Tools: []string{"*"}}
		if got := Decide(p, []Verdict{boom("jev_or")}); got.Action != c.want {
			t.Errorf("on_error=%s: action %q, want %q", c.onError, got.Action, c.want)
		}
	}
}

func TestDecideAggregateMaxUsesHarshestOnFlag(t *testing.T) {
	p := pol("classify", "any", "max")
	// A non-solo classifier configured to demand stop on flag.
	c := p.Classifiers["jev_or"]
	c.OnFlag = "stop"
	p.Classifiers["jev_or"] = c
	if got := Decide(p, []Verdict{flag("jev_or")}); got.Action != Stop {
		t.Errorf("max with stop on_flag = %q, want stop", got.Action)
	}
	// Same set, but the only flag comes from a prompter → prompt.
	if got := Decide(p, []Verdict{flag("static_extreme")}); got.Action != Prompt {
		t.Errorf("max with prompt on_flag = %q, want prompt", got.Action)
	}
}

func TestClassifiersJudgesTools(t *testing.T) {
	p := pol("classify", "any", "max")
	p.Classifiers["jev_or"] = ClassifierConfig{State: "on", Tools: []string{"bash", "write"}}
	for tool, want := range map[string]bool{"bash": true, "write": true, "read": false, "spawn": false} {
		if got := p.Classifiers["jev_or"].Judges(tool); got != want {
			t.Errorf("Judges(%s) = %v, want %v", tool, got, want)
		}
	}
	p.Classifiers["jev_or"] = ClassifierConfig{State: "off", Tools: []string{"*"}}
	if p.Classifiers["jev_or"].Judges("bash") {
		t.Error("state=off must not judge")
	}
	p.Classifiers["jev_or"] = ClassifierConfig{State: "on"}
	if !p.Classifiers["jev_or"].Judges("anything") {
		t.Error("empty tools list means all tools")
	}
}

func TestDecideAutoSafeKeepsOnlyStops(t *testing.T) {
	p := pol("auto-safe", "any", "max")
	stop := Decide(p, []Verdict{{Classifier: "static_normal", Flagged: true}, {Classifier: "static_extreme", Flagged: true}})
	if stop.Action != Stop {
		t.Errorf("solo static stop = %+v", stop)
	}
	prompt := Decide(p, []Verdict{{Classifier: "static_normal"}, {Classifier: "static_extreme", Flagged: true}})
	if prompt.Action != Allow || !prompt.Flagged {
		t.Errorf("extreme flag = %+v, want flagged allow", prompt)
	}
	p.Classifiers["static_extreme"] = ClassifierConfig{State: "on", OnFlag: "stop", Tools: []string{"*"}}
	if d := Decide(p, []Verdict{{Classifier: "static_normal"}, {Classifier: "static_extreme", Flagged: true}}); d.Action != Stop {
		t.Errorf("static_extreme on_flag=stop = %+v, want stop", d)
	}
}
