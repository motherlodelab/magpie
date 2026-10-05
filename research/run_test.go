package research_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/scrape"
)

func TestRun_EndToEnd(t *testing.T) {
	pages := e2ePages(t)
	e := newEnv(t, pages, e2eSerp, defaultScript)
	var events []research.Event // unguarded on purpose: -race proves OnEvent is serial
	rep, err := research.Run(context.Background(), e.deps, e2eJob(e, func(j *research.Job) {
		j.OnEvent = func(ev research.Event) { events = append(events, ev) }
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Status != "done" {
		t.Fatalf("status = %q, want done", rep.Status)
	}
	rr, err := e.db.GetResearchRun(rep.RunID)
	if err != nil || rr.Status != "done" || rr.Report != rep.Markdown {
		t.Errorf("research_runs = %q (%v); report stored == returned: %v", rr.Status, err, rr.Report == rep.Markdown)
	}

	// Every verdict path, one Errorf each.
	a := factBy(t, rep.Facts, uAlpha, "rose 12%")
	if a.Status != "verified" || !strings.Contains(a.Note, "corroborated by") || !strings.Contains(a.Note, "beta.example") {
		t.Errorf("A = %s %q, want verified + corroborated by …beta.example", a.Status, a.Note)
	}
	if m := factBy(t, rep.Facts, uAlpha, "doubled"); m.Status != "dropped" || m.Note != "quote not found in pinned snapshot" {
		t.Errorf("M = %s %q, want dropped by the quote check", m.Status, m.Note)
	}
	b := factBy(t, rep.Facts, uBeta, "Analysts")
	if b.Status != "verified" {
		t.Errorf("B = %s %q, want verified", b.Status, b.Note)
	}
	g := factBy(t, rep.Facts, uGamma, "Lab tests")
	if g.Status != "verified" {
		t.Errorf("G = %s %q, want verified", g.Status, g.Note)
	}
	if p := factBy(t, rep.Facts, uDelta, "ten years"); p.Status != "contested" || !strings.Contains(p.Note, g.FactID) || !strings.Contains(p.Note, "gamma.example") {
		t.Errorf("P = %s %q, want contested naming %s (gamma.example)", p.Status, p.Note, g.FactID)
	}
	if z := factBy(t, rep.Facts, uZeta, "continue"); z.Status != "softened" || z.Note != cZSoft {
		t.Errorf("Z = %s %q, want softened %q", z.Status, z.Note, cZSoft)
	}
	if o := factBy(t, rep.Facts, uEpsilon, "2019"); o.Status != "dropped" || !strings.Contains(o.Note, "outside") {
		t.Errorf("O = %s %q, want dropped outside the range", o.Status, o.Note)
	}
	for _, f := range rep.Facts {
		if _, ok := pages[f.URL]; !ok {
			t.Errorf("fact %s pinned to %q, not a requested corpus URL", f.FactID, f.URL)
		}
	}

	// Go dropped M and O before any model judged them.
	for _, c := range e.llm.recorded("judge") {
		if strings.Contains(c.Material, qM) || strings.Contains(c.Material, qO) {
			t.Errorf("the judge saw a quote Go had dropped:\n%s", c.Material)
		}
	}

	if len(rep.Unreadable) != 1 || rep.Unreadable[0].URL != uLogin || rep.Unreadable[0].Issue != "login-required" {
		t.Errorf("Unreadable = %+v, want the login wall as login-required", rep.Unreadable)
	}

	// The gap jumped angle 1's queue (per-sub order; cross-sub order is free).
	var a1 []string
	for _, q := range e.search.queries() {
		if slices.Contains(angle1Queries, q) {
			a1 = append(a1, q)
		}
	}
	if gi, si := slices.Index(a1, gapQuery), slices.Index(a1, "alpha index regions"); gi < 0 || si < 0 || gi > si {
		t.Errorf("angle 1 queries %v: the gap must come before the next seed query", a1)
	}

	// The resolver kept A's citation only, with A's pin, and no writer URL.
	if !slices.Equal(rep.Cited, []string{a.FactID}) {
		t.Errorf("Cited = %v, want only A (%s)", rep.Cited, a.FactID)
	}
	src := fmt.Sprintf("[^1]: %s — \"%s\" (checked %s)", uAlpha, qA, a.CheckedAt.UTC().Format(time.RFC3339Nano))
	if !strings.Contains(rep.Markdown, src) || strings.Contains(rep.Markdown, "evil.example") || strings.Count(rep.Markdown, "[^") != 2 {
		t.Errorf("report:\n%s\nwant exactly one footnote %q and no evil.example", rep.Markdown, src)
	}

	info, err := e.db.GetRun(rep.RunID)
	if err != nil {
		t.Fatal(err)
	}
	calls, _ := e.db.LLMCalls(rep.RunID) //nolint:errcheck // asserted via len below
	if want := float64(len(calls)) * e.llm.perCall; info.Command != "research" || info.Status != "finished" || math.Abs(info.USDEstimate-want) > 1e-9 {
		t.Errorf("run_history = %s/%s $%v, want research/finished $%v", info.Command, info.Status, info.USDEstimate, want)
	}
	if want := int64(e.web.total()); info.FetchPages != want || info.PagesOK != 6 || info.PagesErr != 1 {
		t.Errorf("fetch_pages %d pages %d/%d, want %d fetched (the wall answered too), 6 ok, 1 unreadable", info.FetchPages, info.PagesOK, info.PagesErr, want)
	}
	if runs, _ := e.db.ListRuns(0); len(runs) != 1 { //nolint:errcheck // len asserts
		t.Errorf("run_history rows = %d, want 1: reads must join the research run", len(runs))
	}
	purposes := map[string]bool{}
	for _, c := range calls {
		purposes[c.Purpose] = true
	}
	for _, p := range []string{"plan", "extract", "judge", "replan", "write"} {
		if !purposes[p] {
			t.Errorf("llm_calls purposes %v lack %q", purposes, p)
		}
	}
	if len(events) < 2 || events[0].Stage != "plan" || events[len(events)-1].Stage != "done" {
		t.Errorf("events: first %+v, last %+v; want plan … done", events[0], events[len(events)-1])
	}
	if last := events[len(events)-1]; last.CapUSD != 1 || last.SpentUSD <= 0 {
		t.Errorf("done event spend %v / cap %v, want both set", last.SpentUSD, last.CapUSD)
	}
	e.settle(t)
}

func TestRun_PlanBounded(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, func(task, material string) (string, error) {
		if task == "plan" {
			return planJSON(
				research.Angle{Question: angle1Q, Queries: []string{"alpha index 2025"}},
				research.Angle{Question: angle2Q, Queries: []string{"delta battery claims"}},
				research.Angle{Question: "third", Queries: []string{"delta rebuttal"}},
				research.Angle{Question: "fourth", Queries: []string{"delta older reports"}},
			), nil
		}
		return defaultScript(task, material)
	})
	rep, err := research.Run(context.Background(), e.deps, e.job(e2eAsk, func(j *research.Job) { j.Options.Effort = "quick" }))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Plan.Angles) != 1 {
		t.Errorf("plan angles = %d, want 1 (quick)", len(rep.Plan.Angles))
	}
	for _, q := range e.search.queries() {
		if !slices.Contains(angle1Queries, q) {
			t.Errorf("searched %q, outside the one quick angle", q)
		}
	}
}

func TestRun_Approve(t *testing.T) {
	t.Run("edits reach the search", func(t *testing.T) {
		e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
		_, err := research.Run(context.Background(), e.deps, e.job(e2eAsk, func(j *research.Job) {
			j.Approve = func(p research.Plan) (research.Plan, error) {
				p.Angles[0].Queries = []string{"edited alpha query"}
				return p, nil
			}
		}))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		qs := e.search.queries()
		if !slices.Contains(qs, "edited alpha query") || slices.Contains(qs, "alpha index 2025") {
			t.Errorf("queries %v, want the edited one instead of the draft's", qs)
		}
	})
	t.Run("an error stops before research", func(t *testing.T) {
		e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
		no := errors.New("operator said no")
		rep, err := research.Run(context.Background(), e.deps, e.job(e2eAsk, func(j *research.Job) {
			j.Approve = func(research.Plan) (research.Plan, error) { return research.Plan{}, no }
		}))
		if !errors.Is(err, no) {
			t.Fatalf("err = %v, want the Approve error", err)
		}
		if qs := e.search.queries(); len(qs) != 0 {
			t.Errorf("searched %v after a declined plan", qs)
		}
		if _, gerr := e.db.GetResearchRun(rep.RunID); gerr == nil {
			t.Error("a declined plan left a research_runs row")
		}
		if info, ierr := e.db.GetRun(rep.RunID); ierr != nil || info.Status != "interrupted" {
			t.Errorf("run_history = %q (%v), want interrupted", info.Status, ierr)
		}
	})
	t.Run("resume skips it", func(t *testing.T) {
		e := newEnv(t, e2ePages(t), e2eSerp, defaultScript)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rep, _ := research.Run(ctx, e.deps, e.job(e2eAsk, func(j *research.Job) { //nolint:errcheck // the cancel is the point
			j.OnEvent = func(ev research.Event) {
				if ev.Stage == "read" {
					cancel()
				}
			}
		}))
		_, err := research.Run(context.Background(), e.deps, e.job(e2eAsk, func(j *research.Job) {
			j.RunID = rep.RunID
			j.Approve = func(p research.Plan) (research.Plan, error) {
				t.Error("Approve called on resume")
				return p, nil
			}
		}))
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	})
}

func TestRun_Validation(t *testing.T) {
	keyless := func(no ...string) func(string) string {
		return func(p string) string {
			if slices.Contains(no, p) {
				return ""
			}
			return "k"
		}
	}
	for _, tc := range []struct {
		name    string
		mod     func(*research.Job, *scrape.Deps)
		missing bool   // errors.Is ErrMissingKey
		names   string // the error names it
	}{
		{"empty question", func(j *research.Job, _ *scrape.Deps) { j.Question = "  " }, false, "question"},
		{"2001 runes", func(j *research.Job, _ *scrape.Deps) { j.Question = strings.Repeat("é", 2001) }, false, "question"},
		{"no options", func(j *research.Job, _ *scrape.Deps) { j.Options = research.Options{} }, false, "provider"},
		{"writer key", func(_ *research.Job, d *scrape.Deps) { d.APIKeyFor = keyless("openai") }, true, "openai"},
		{"judge key", func(j *research.Job, d *scrape.Deps) {
			j.Options.JudgeProvider, j.Options.JudgeModel = "anthropic", "claude-x"
			d.APIKeyFor = keyless("anthropic")
		}, true, "anthropic"},
		{"brave without a key", func(j *research.Job, d *scrape.Deps) {
			j.Options.Search = []string{"brave"}
			d.APIKeyFor = keyless("brave")
		}, true, "brave"},
		{"searxng without its URL", func(*research.Job, *scrape.Deps) { t.Setenv("MAGPIE_SEARXNG_URL", "") }, false, "MAGPIE_SEARXNG_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil, nil, defaultScript)
			j, d := e.job(e2eAsk, nil), e.deps
			tc.mod(&j, &d)
			_, err := research.Run(context.Background(), d, j)
			if err == nil || !strings.Contains(err.Error(), tc.names) || errors.Is(err, scrape.ErrMissingKey) != tc.missing {
				t.Errorf("err = %v, want one naming %q (ErrMissingKey: %v)", err, tc.names, tc.missing)
			}
			if runs, _ := e.db.ListRuns(0); len(runs) != 0 { //nolint:errcheck // len asserts
				t.Errorf("a validation failure stored %d run rows", len(runs))
			}
			if n := len(e.llm.calls); n != 0 {
				t.Errorf("a validation failure made %d model calls", n)
			}
		})
	}
}

func TestRun_StaleStop(t *testing.T) {
	pages, serp := map[string]page{}, map[string][]string{}
	var queries []string
	for i := range 5 {
		u, q := fmt.Sprintf("https://empty%d.example/page", i), fmt.Sprintf("empty query %d", i)
		pages[u] = page{200, pad(t, "Empty", "Nothing on this page bears on the question that was asked, though it is long enough to read.")}
		serp[q] = []string{u}
		queries = append(queries, q)
	}
	e := newEnv(t, pages, serp, func(task, material string) (string, error) {
		if task == "plan" {
			return planJSON(research.Angle{Question: "Anything?", Queries: queries}), nil
		}
		return defaultScript(task, material) // unknown sources extract zero facts
	})
	if _, err := research.Run(context.Background(), e.deps, e.job("Anything?", func(j *research.Job) { j.Options.Effort = "quick" })); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if qs := e.search.queries(); len(qs) != 3 {
		t.Errorf("searches = %v, want exactly 3 before the stale stop", qs)
	}
}

func TestRun_NothingSurvived(t *testing.T) {
	e := newEnv(t, e2ePages(t), e2eSerp, func(task, material string) (string, error) {
		if task == "extract" {
			return extractJSON([]xfact{{Claim: "A claim the page never makes.", Quote: qM, Confidence: 0.5}}, nil, "none"), nil
		}
		return defaultScript(task, material)
	})
	rep, err := research.Run(context.Background(), e.deps, e.job(e2eAsk, nil))
	if err != nil || rep.Status != "done" {
		t.Fatalf("Run = %q, %v; want done", rep.Status, err)
	}
	if !strings.HasPrefix(rep.Markdown, "No claim survived verification") {
		t.Errorf("report = %q, want the honest empty report", rep.Markdown)
	}
	if n := len(e.llm.recorded("write")); n != 0 {
		t.Errorf("write calls = %d, want 0 (nothing to write from)", n)
	}
	if n := len(e.llm.recorded("judge")); n != 0 {
		t.Errorf("judge calls = %d, want 0 (Go dropped every misquote first)", n)
	}
}

// TestSchemas_StrictCompatible: OpenAI strict mode takes a schema only when
// every object closes additionalProperties and requires all its properties.
func TestSchemas_StrictCompatible(t *testing.T) {
	t.Parallel()
	var walk func(name, path string, v any)
	walk = func(name, path string, v any) {
		switch n := v.(type) {
		case map[string]any:
			if n["type"] == "object" {
				props, _ := n["properties"].(map[string]any)
				req, _ := n["required"].([]any)
				keys := make([]string, 0, len(props))
				for k := range props {
					keys = append(keys, k)
				}
				got := make([]string, 0, len(req))
				for _, r := range req {
					got = append(got, r.(string))
				}
				slices.Sort(keys)
				slices.Sort(got)
				if n["additionalProperties"] != false || !slices.Equal(keys, got) {
					t.Errorf("%s%s: additionalProperties %v, required %v vs properties %v", name, path, n["additionalProperties"], got, keys)
				}
			}
			for k, c := range n {
				walk(name, path+"/"+k, c)
			}
		case []any:
			for i, c := range n {
				walk(name, fmt.Sprintf("%s/%d", path, i), c)
			}
		}
	}
	for name, sch := range research.Schemas {
		walk(name, "", sch.Raw)
	}
	if err := research.Schemas["extract"].Validate([]byte(`{"facts":[],"gaps":[],"stance":"maybe"}`)); err == nil {
		t.Error("an off-enum stance passed the extract schema")
	}
}
