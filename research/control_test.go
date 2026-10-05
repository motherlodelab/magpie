package research_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/research"
)

func TestRun_WriteNow(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	ctl := &research.Control{}
	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) {
		j.Control = ctl
		j.OnEvent = func(ev research.Event) {
			if ev.Stage == "facts" && ev.Usable > 0 {
				ctl.WriteNow() // from the callback: Control is safe from any goroutine
			}
		}
	}))
	if err != nil || rep.Status != "done" {
		t.Fatalf("Run = %q, %v; want done", rep.Status, err)
	}
	if n := e.web.total(); n >= len(e2ePages(t)) {
		t.Errorf("fetched %d pages, want fewer than the full run's %d", n, len(e2ePages(t)))
	}
	if len(e.llm.recorded("write")) != 1 {
		t.Error("write-now must still write the report")
	}
	// A page already extracted when Write now landed still gets judged.
	for _, f := range rep.Facts {
		if f.Status == "unverified" {
			t.Errorf("fact %s at %s left unverified by write-now", f.FactID, f.URL)
		}
	}
	e.settle(t)
}

// TestRun_ControlBeforeStart: Control set before Run waits for the run's
// row: the plan still runs, the steer is persisted at the first boundary.
func TestRun_ControlBeforeStart(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	ctl := &research.Control{}
	ctl.Steer("only 2025 data")
	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) { j.Control = ctl }))
	if err != nil || rep.Status != "done" {
		t.Fatalf("Run = %q, %v; want done", rep.Status, err)
	}
	if rr, _ := e.db.GetResearchRun(rep.RunID); rr.Steer != "only 2025 data" { //nolint:errcheck // asserted
		t.Errorf("steer = %q, want the pre-start steer persisted", rr.Steer)
	}
}

func TestRun_BudgetStop(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	e.llm.perCall = 0.3 // plan, extract, judge = 0.9 ≥ the 0.85 research share
	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) { j.Options.Effort = "quick" }))
	if err != nil || rep.Status != "done" {
		t.Fatalf("Run = %q, %v; want done", rep.Status, err)
	}
	if len(e.llm.recorded("write")) != 1 {
		t.Error("the write reserve must still pay for the report")
	}
	info, err := e.db.GetRun(rep.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if limit := 1 + e.llm.perCall; info.USDEstimate > limit+1e-9 {
		t.Errorf("spend $%v > cap + the write's own output $%v", info.USDEstimate, limit)
	}
}

// TestRun_Cancel: a stop from the operator (desktop Stop, the CLI's second
// Ctrl-C) ends the run as stopped — no report, facts kept, run_history
// interrupted — and leaves no goroutine behind.
func TestRun_Cancel(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events []research.Event // unguarded on purpose: -race proves serial OnEvent
	j := e.job(e2eAsk, func(j *research.Job) {
		j.OnEvent = func(ev research.Event) {
			events = append(events, ev)
			if ev.Stage == "read" {
				cancel() // non-blocking; the loop sees ctx at its next wait or boundary
			}
		}
	})
	rep, err := research.Run(ctx, e.deps, j)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep.Status != "stopped" || rep.Markdown != "" {
		t.Errorf("report = %q status, %d bytes of markdown; want stopped, none", rep.Status, len(rep.Markdown))
	}
	rr, gerr := e.db.GetResearchRun(rep.RunID)
	if gerr != nil || rr.Status != "stopped" || rr.Report != "" {
		t.Errorf("research_runs = %+v (%v), want stopped with no report", rr, gerr)
	}
	info, ierr := e.db.GetRun(rep.RunID)
	if ierr != nil || info.Status != "interrupted" {
		t.Errorf("run_history status = %q (%v), want interrupted", info.Status, ierr)
	}
	if len(e.llm.recorded("write")) != 0 {
		t.Error("a stopped run must not write")
	}
	e.settle(t)
}

func TestRun_Resume(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := research.Run(ctx, e.deps, e2eJob(e, func(j *research.Job) {
		j.OnEvent = func(ev research.Event) {
			if ev.Stage == "facts" && ev.URL == uAlpha {
				cancel()
			}
		}
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v, want canceled", err)
	}
	a := factBy(t, first.Facts, uAlpha, "rose 12%")

	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) {
		j.RunID = first.RunID
		j.Options.Provider, j.Options.Model = "anthropic", "claude-x" // ignored: the stored options win
	}))
	if err != nil || rep.Status != "done" {
		t.Fatalf("resume = %q, %v; want done", rep.Status, err)
	}
	if n := len(e.llm.recorded("plan")); n != 1 {
		t.Errorf("plan calls = %d, want 1 (resume reuses the stored plan)", n)
	}
	if n := e.web.fetches(uAlpha); n != 1 {
		t.Errorf("alpha fetched %d times, want 1 (resume never re-reads a done page)", n)
	}
	if again := factBy(t, rep.Facts, uAlpha, "rose 12%"); again.FactID != a.FactID {
		t.Errorf("A is %s after resume, was %s", again.FactID, a.FactID)
	}
	// The stored options win: the caller's other provider is ignored.
	for _, c := range e.llm.calls {
		if c.Provider != "openai" {
			t.Errorf("a %s call ran on %s/%s, want the stored openai pair", c.Task, c.Provider, c.Model)
		}
	}
	e.settle(t)
}

func TestRun_JudgeFallback(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	e.llm.down["anthropic"] = true
	var warns []string
	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) {
		j.Options.JudgeProvider, j.Options.JudgeModel = "anthropic", "claude-x"
		j.OnEvent = func(ev research.Event) {
			if ev.Stage == "warn" && strings.Contains(ev.Detail, "judge anthropic/claude-x") {
				warns = append(warns, ev.Detail)
			}
		}
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(warns) != 1 {
		t.Errorf("judge fallback warnings = %v, want exactly 1", warns)
	}
	judges := e.llm.recorded("judge")
	if last := judges[len(judges)-1]; last.Provider != "openai" || last.Model != "gpt-4o-mini" {
		t.Errorf("later judge calls ran on %s/%s, want the writer's pair", last.Provider, last.Model)
	}
	if a := factBy(t, rep.Facts, uAlpha, "rose 12%"); a.Status == "unverified" {
		t.Error("facts got no verdict after the fallback")
	}
}

func TestRun_LLMErrorStreak(t *testing.T) {
	dead := errors.New("fake: model gone")
	// One search, three pages: three extract failures in a row, no success
	// (a replan) in between to reset the streak.
	e := newEnv(t, e2ePages(t), map[string][]string{"streak": {uAlpha, uBeta, uDelta}}, func(task, material string) (string, error) {
		switch task {
		case "plan":
			return planJSON(research.Angle{Question: angle1Q, Queries: []string{"streak"}}), nil
		case "extract":
			return "", dead
		}
		return defaultScript(task, material)
	})
	rep, err := research.Run(context.Background(), e.deps, e.job(e2eAsk, func(j *research.Job) { j.Options.Effort = "quick" }))
	if !errors.Is(err, dead) {
		t.Fatalf("err = %v, want the model's error after the streak", err)
	}
	if n := len(e.llm.recorded("extract")); n != 3 {
		t.Errorf("extract calls = %d, want the streak's 3", n)
	}
	rr, _ := e.db.GetResearchRun(rep.RunID) //nolint:errcheck // status asserted
	info, _ := e.db.GetRun(rep.RunID)       //nolint:errcheck // status asserted
	if rr.Status != "failed" || info.Status != "error" || rep.Status != "failed" {
		t.Errorf("statuses research %q / history %q / report %q, want failed/error/failed", rr.Status, info.Status, rep.Status)
	}
	e.settle(t)
}

// TestRun_Steer: the operator's steer is persisted, logged once, and read
// by every extract after it.
func TestRun_Steer(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
	ctl := &research.Control{}
	steers := 0
	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) {
		j.Control = ctl
		j.OnEvent = func(ev research.Event) {
			switch ev.Stage {
			case "read":
				ctl.Steer("focus on regional data")
			case "steer":
				steers++
			}
		}
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rr, gerr := e.db.GetResearchRun(rep.RunID); gerr != nil || rr.Steer != "focus on regional data" {
		t.Errorf("research_runs.steer = %q (%v), want the operator's steer", rr.Steer, gerr)
	}
	if steers != 1 {
		t.Errorf("steer events = %d, want 1 (latest wins, logged once)", steers)
	}
	extracts := e.llm.recorded("extract")
	if last := extracts[len(extracts)-1]; !strings.Contains(last.Material, "Operator steering: focus on regional data") {
		t.Errorf("the last extract never saw the steer:\n%s", last.Material)
	}
}
