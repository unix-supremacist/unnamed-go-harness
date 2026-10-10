package perm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCl is a scripted classifier that counts its runs.
type fakeCl struct {
	name    string
	flagged bool
	reason  string
	err     error
	runs    int32 // atomic: auto mode classifies on another goroutine
}

func (f *fakeCl) Name() string { return f.name }
func (f *fakeCl) Classify(_ context.Context, _ Call) Verdict {
	atomic.AddInt32(&f.runs, 1)
	return Verdict{Classifier: f.name, Flagged: f.flagged, Reason: f.reason, Err: f.err}
}

func engineFor(t *testing.T, p Policy, in io.Reader, interactive bool, cls ...Classifier) (*Engine, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	log, err := NewLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(log.Close)
	e := NewEngine(p, NewBroker(in, io.Discard, interactive, p.HeadlessPrompt), log)
	e.Add(cls...)
	return e, path
}

func TestEngineSoloStopSkipsModelClassifiers(t *testing.T) {
	norm := &fakeCl{name: "static_normal", flagged: true, reason: "recursive rm on /"}
	model := &fakeCl{name: "jev_or"}
	e, path := engineFor(t, pol("classify", "any", "max"), nil, false, norm, model)
	oc := e.Check(context.Background(), Call{Tool: "bash", Command: "rm -rf /"})
	if oc.Action != Stop {
		t.Fatalf("outcome = %+v, want stop", oc)
	}
	if atomic.LoadInt32(&model.runs) != 0 {
		t.Errorf("model classifier ran %d times after a solo stop, want 0", model.runs)
	}
	waitForFile(t, path, `"action":"stop"`)
}

func TestEngineHeadlessPromptDenies(t *testing.T) {
	ext := &fakeCl{name: "static_extreme", flagged: true, reason: "rm with recursive/force flags"}
	e, path := engineFor(t, pol("classify", "any", "max"), nil, false, ext)
	oc := e.Check(context.Background(), Call{Tool: "bash", Command: "rm -rf ./build"})
	if oc.Action != Deny {
		t.Fatalf("outcome = %+v, want deny", oc)
	}
	if !strings.Contains(oc.Reason, "no human attached") {
		t.Errorf("reason = %q, want the headless wording", oc.Reason)
	}
	waitForFile(t, path, `"answer":"n"`)
}

func TestEngineInteractiveAnswers(t *testing.T) {
	for in, want := range map[string]string{
		"y\n": Allow, "n\n": Deny, "s\n": Stop,
	} {
		ext := &fakeCl{name: "static_extreme", flagged: true, reason: "rm -f"}
		e, _ := engineFor(t, pol("classify", "any", "max"), strings.NewReader(in), true, ext)
		oc := e.Check(context.Background(), Call{Tool: "bash", Command: "rm -rf ./build"})
		if oc.Action != want {
			t.Errorf("stdin %q → %q (%s), want %q", in, oc.Action, oc.Reason, want)
		}
		if oc.Action == Deny && oc.Reason != "denied by user" {
			t.Errorf("deny reason = %q, want \"denied by user\"", oc.Reason)
		}
	}
}

func TestEngineManualModePromptsOnCleanCall(t *testing.T) {
	ext := &fakeCl{name: "static_extreme"}
	e, _ := engineFor(t, pol("manual", "any", "max"), nil, false, ext)
	oc := e.Check(context.Background(), Call{Tool: "read", Args: []byte(`{"path":"a.txt"}`)})
	if oc.Action != Deny {
		t.Fatalf("manual + clean + headless = %+v, want deny (prompt → headless_prompt)", oc)
	}
}

func TestEngineSessionAllowSkipsRepeatPrompt(t *testing.T) {
	ext := &fakeCl{name: "static_extreme", flagged: true, reason: "rm -f"}
	e, _ := engineFor(t, pol("classify", "any", "max"), strings.NewReader("a\n"), true, ext)
	call := Call{Tool: "bash", Command: "rm -rf ./build"}
	if oc := e.Check(context.Background(), call); oc.Action != Allow {
		t.Fatalf("first = %+v, want allow", oc)
	}
	// The second check must not prompt again (stdin is exhausted → deny).
	if oc := e.Check(context.Background(), call); oc.Action != Allow || !strings.Contains(oc.Reason, "session") {
		t.Fatalf("second = %+v, want session allow", oc)
	}
}

func TestEngineAutoModeAllowsAndLogsInBackground(t *testing.T) {
	norm := &fakeCl{name: "static_normal", flagged: true, reason: "recursive rm on /"}
	e, path := engineFor(t, pol("auto", "any", "max"), nil, false, norm)
	start := time.Now()
	oc := e.Check(context.Background(), Call{Tool: "bash", Command: "rm -rf /"})
	if oc.Action != Allow {
		t.Fatalf("auto outcome = %+v, want allow", oc)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("auto mode waited %s, must not block", time.Since(start))
	}
	waitForFile(t, path, `"action":"allow"`)
	waitForFile(t, path, `"mode":"auto"`)
	if atomic.LoadInt32(&norm.runs) == 0 {
		t.Error("auto mode must still run classifiers in the background")
	}
}

func TestEngineClassifierErrorUsesOnError(t *testing.T) {
	p := pol("classify", "any", "max")
	c := p.Classifiers["jev_or"]
	c.OnError = "stop"
	p.Classifiers["jev_or"] = c
	bad := &fakeCl{name: "jev_or", err: context.DeadlineExceeded}
	e, _ := engineFor(t, p, nil, false, bad)
	if oc := e.Check(context.Background(), Call{Tool: "bash", Command: "ls"}); oc.Action != Stop {
		t.Fatalf("on_error=stop outcome = %+v, want stop", oc)
	}
	// Default on_error=ignore lets the run continue.
	p2 := pol("classify", "any", "max")
	e2, _ := engineFor(t, p2, nil, false, &fakeCl{name: "jev_or", err: context.DeadlineExceeded})
	if oc := e2.Check(context.Background(), Call{Tool: "bash", Command: "ls"}); oc.Action != Allow {
		t.Fatalf("on_error=ignore outcome = %+v, want allow", oc)
	}
}

func TestEngineOffClassifierDoesNotRun(t *testing.T) {
	p := pol("classify", "any", "max")
	c := p.Classifiers["jev_or"]
	c.State = "off"
	p.Classifiers["jev_or"] = c
	off := &fakeCl{name: "jev_or", flagged: true}
	e, _ := engineFor(t, p, nil, false, off)
	if oc := e.Check(context.Background(), Call{Tool: "bash", Command: "ls"}); oc.Action != Allow {
		t.Fatalf("outcome = %+v, want allow", oc)
	}
	if atomic.LoadInt32(&off.runs) != 0 {
		t.Errorf("off classifier ran %d times", off.runs)
	}
}

// waitForFile polls the decision log until it contains want (auto mode logs
// from a goroutine) or fails the test.
func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("decision log never contained %q; got:\n%s", want, b)
}

// Plain auto mode applies nothing, not even static stops: it is the
// "ignore every classifier" mode. auto-safe is the unattended mode.
func TestEngineAutoDoesNotApplyStaticStops(t *testing.T) {
	norm := &fakeCl{name: "static_normal", flagged: true, reason: "recursive rm on ~"}
	e, _ := engineFor(t, pol("auto", "any", "max"), nil, false, norm)
	if oc := e.Check(context.Background(), Call{Tool: "bash", Command: "rm -rf ~"}); oc.Action != Allow {
		t.Fatalf("auto = %+v, want allow", oc)
	}
	e.WaitBackground(time.Second)
}

func TestEngineAutoSafe(t *testing.T) {
	slow := &slowCl{fakeCl: fakeCl{name: "jev_or", flagged: true, reason: "irreversible 0.9"}, d: 300 * time.Millisecond}
	for _, c := range []struct {
		name          string
		normal, ext   bool
		want          string
		reasonContain string
	}{
		{"static stop applies", true, true, Stop, "recursive rm"},
		{"extreme flag does not prompt or deny", false, true, Allow, "auto-safe"},
		{"clean call", false, false, Allow, "auto-safe"},
	} {
		t.Run(c.name, func(t *testing.T) {
			norm := &fakeCl{name: "static_normal", flagged: c.normal, reason: "recursive rm on ~"}
			ext := &fakeCl{name: "static_extreme", flagged: c.ext, reason: "rm with recursive/force flags"}
			e, path := engineFor(t, pol("auto-safe", "any", "max"), nil, false, norm, ext, slow)
			start := time.Now()
			oc := e.Check(context.Background(), Call{Tool: "bash", Command: "rm -rf x"})
			if oc.Action != c.want || !strings.Contains(oc.Reason, c.reasonContain) {
				t.Fatalf("outcome = %+v, want %s (%s)", oc, c.want, c.reasonContain)
			}
			if time.Since(start) > 200*time.Millisecond {
				t.Errorf("auto-safe waited %s on model classifiers", time.Since(start))
			}
			if c.want == Allow {
				waitForFile(t, path, `"mode":"auto-safe"`)
				waitForFile(t, path, "jev_or")
			}
			e.WaitBackground(2 * time.Second)
		})
	}
}

// slowCl is a fakeCl that takes d to answer, like a network classifier.
type slowCl struct {
	fakeCl
	d time.Duration
}

func (s *slowCl) Classify(ctx context.Context, c Call) Verdict {
	time.Sleep(s.d)
	return s.fakeCl.Classify(ctx, c)
}

func TestEngineTierPanels(t *testing.T) {
	mk := func() map[string]*fakeCl {
		return map[string]*fakeCl{"jev_or": {name: "jev_or"}, "clef_or": {name: "clef_or"}, "clef_flash_or": {name: "clef_flash_or"}, "luna_or": {name: "luna_or"}}
	}
	for _, c := range []struct {
		tier    string
		extreme bool
		want    string // classifiers that must run, sorted
	}{
		{"trivial", false, ""},
		{"low", false, "jev_or"},
		{"middle", false, "jev_or"},
		{"high", false, "jev_or"},
		{"trivial", true, "jev_or"}, // a static flag lifts the call to high
	} {
		models := mk()
		p := pol("classify", "any", "max")
		for name := range models {
			p.Classifiers[name] = ClassifierConfig{State: "on", OnFlag: "prompt", Tools: []string{"*"}, TimeoutMS: 3000}
		}
		p.Tiers = DefaultTiers()
		cls := []Classifier{&fakeCl{name: "static_normal"}, &fakeCl{name: "static_extreme", flagged: c.extreme, reason: "rm with recursive/force flags"}}
		for _, m := range models {
			cls = append(cls, m)
		}
		e, path := engineFor(t, p, strings.NewReader("y\n"), true, cls...)
		e.Triage = func(Call) (string, string) { return c.tier, "test" }
		e.Check(context.Background(), Call{Tool: "bash", Command: "x", Agent: "root"})
		var ran []string
		for name, m := range models {
			if atomic.LoadInt32(&m.runs) > 0 {
				ran = append(ran, name)
			}
		}
		sort.Strings(ran)
		if got := strings.Join(ran, ","); got != c.want {
			t.Errorf("tier %s (extreme=%v): ran %q, want %q", c.tier, c.extreme, got, c.want)
		}
		wantTier := c.tier
		if c.extreme {
			wantTier = "high"
		}
		waitForFile(t, path, `"tier":"`+wantTier+`"`)
	}
}

func TestEngineAutoSafeMirrorsClassifyPanels(t *testing.T) {
	for _, c := range []struct {
		tier      string
		jev, clef bool
	}{{"trivial", false, false}, {"low", true, false}, {"high", true, false}} {
		jev, clef := &fakeCl{name: "jev_or", flagged: true}, &fakeCl{name: "clef_or", flagged: true}
		p := pol("auto-safe", "any", "max")
		p.Tiers = DefaultTiers()
		e, path := engineFor(t, p, nil, false, &fakeCl{name: "static_normal"}, &fakeCl{name: "static_extreme"}, jev, clef)
		e.Triage = func(Call) (string, string) { return c.tier, "test" }
		start := time.Now()
		oc := e.Check(context.Background(), Call{Tool: "bash", Command: "x", Agent: "root"})
		if oc.Action != Allow || time.Since(start) > 200*time.Millisecond {
			t.Fatalf("auto-safe must never pause: %+v after %s", oc, time.Since(start))
		}
		e.WaitBackground(2 * time.Second)
		if ran := atomic.LoadInt32(&jev.runs) > 0; ran != c.jev {
			t.Errorf("%s: jev ran=%v, want %v", c.tier, ran, c.jev)
		}
		if ran := atomic.LoadInt32(&clef.runs) > 0; ran != c.clef {
			t.Errorf("%s: clef ran=%v, want %v", c.tier, ran, c.clef)
		}
		waitForFile(t, path, `"tier":"`+c.tier+`"`)
	}
}
