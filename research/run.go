package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// DefaultMaxCostUSD is the cap surfaces apply when the user set none
// (--max-cost, max_cost_usd and config all zero): every run has a cap.
// $1 is DR7's Standard-effort median bar.
const DefaultMaxCostUSD = 1.0

// Loop knobs. ponytail: each is a recommended default, not a measured
// optimum; DR7's trajectory metrics replace them.
const (
	hitsPerSearch    = 10   // scrape.Search's own default limit
	readsPerSearch   = 3    // top K of each search's fused ranking
	staleSearches    = 3    // consecutive searches with zero usable facts end a sub-researcher ⌗
	maxPageWords     = 3000 // the extract material cap (≈ EstimateRun's extract input)
	maxFactsPerPage  = 8
	maxGapsPerPage   = 3
	maxPivotsPerPage = 2
	minQuoteRunes    = 20 // shorter matches anywhere
	maxQuoteRunes    = 600
	llmErrorStreak   = 3 // consecutive failed LLM calls abort: a dead key must not burn the tool budget
	maxWriterFacts   = 80
	maxQuestionRunes = 2000 // spec §6.1's bridge bound, enforced once here
	maxFindings      = 40   // recent findings the replan sees
)

// Pacing per host and per "search:"+backend (DDG blocks bursts): the
// crawl.HostLimiters defaults. Vars so tests can lift them (SetLimits).
var limitRPS, limitBurst = 1.0, 3

// Job is one research run's inputs.
type Job struct {
	RunID    string  // "" = minted here; set = the caller minted it (desktop Start), or resume when research_runs has it
	Question string  // required, ≤ 2000 runes; on resume the stored question wins
	Options  Options // Run normalizes it; persisted as research_runs.options
	// Approve sees the drafted plan before any search. It returns the plan
	// to run (edits are re-validated) or an error that stops the run before
	// research spends. nil = run the draft. Skipped on resume.
	Approve func(Plan) (Plan, error)
	OnEvent func(Event) // nil = silent; called serially (never concurrently); must not block
	Control *Control    // nil = no steering
}

// Control steers a run in flight from any goroutine (desktop Steer /
// Write now, the CLI's first Ctrl-C). The loop reads it at stage
// boundaries; the zero value is ready to use.
type Control struct {
	mu       sync.Mutex
	writeNow bool
	steer    string
}

// WriteNow stops researching; the run writes from what's verified.
func (c *Control) WriteNow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeNow = true
}

// Steer sets the operator's steering message (latest wins; blank is
// ignored). It is persisted as research_runs.steer and read by replan,
// extract and the writer.
func (c *Control) Steer(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := strings.TrimSpace(msg); s != "" {
		c.steer = s
	}
}

func (c *Control) read() (writeNow bool, steer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeNow, c.steer
}

// Event is one activity-log line. Stage is one of plan, search, read,
// unreadable, facts, replan, steer, write, warn, done.
type Event struct {
	Stage    string
	Detail   string  // human line: the query and backend, the title, the error
	URL      string  // read, unreadable, facts
	Issue    string  // unreadable: login-required|access-denied|unavailable|empty|challenge|error
	SpentUSD float64 // run_history.usd_estimate at emit time
	CapUSD   float64
	Facts    int // ledger rows so far
	Usable   int // verified + softened (+ contested once settled) so far
}

// Report is a finished (or stopped/failed) run.
type Report struct {
	RunID      string
	Status     string // done|stopped|failed (research_runs.status)
	Plan       Plan
	Markdown   string   // resolved; "" unless done
	Facts      []Fact   // the whole ledger after verification, dropped rows included (the audit trail)
	Cited      []string // fact IDs in footnote order
	Unreadable []Unreadable
	SpentUSD   float64
}

// Unreadable is one page the run couldn't read, and why.
type Unreadable struct{ URL, Issue, Detail string }

// run is one Run's loop state. Locks: mu guards everything mutable below
// it; leadMu serializes replans; evMu serializes OnEvent and is never held
// with mu. Budget has its own lock.
type run struct {
	d        scrape.Deps
	id       string
	o        Options
	budget   *Budget
	lim      *crawl.HostLimiters
	backends []string
	onEvent  func(Event)
	ctl      *Control
	plan     Plan // the scope; replans append angles under leadMu

	leadMu  sync.Mutex
	active  int // sub-researchers running
	replans int
	settled bool // the replan said done

	evMu  sync.Mutex
	spent float64

	mu             sync.Mutex
	issued         []string
	visited        map[string]bool
	pivots         map[string]*pivot
	findings       []string
	facts, usable  int
	steer          string
	judgeP, judgeM string
	errStreak      int
	extractors     map[exKey]extract.Extractor
}

// Run researches j.Question: scope (and Approve) → sub-researchers that
// search, rank, read, pin, extract and verify inline (with a gap queue and
// a replan per report) → settle pivotal claims → one writer pass → Go
// resolves its fact IDs into footnotes pinned to stored snapshots.
//
// Everything checkable without I/O is checked before the run row exists:
// such an error stores nothing. After that the run always finishes its
// rows: done/finished on success, stopped/interrupted when ctx ends it
// (ctx.Err() is returned), failed/error otherwise (a refused write wraps
// crawl.ErrCostCeiling; facts are kept, so a resume can write later). A
// Job.RunID naming an unfinished research run resumes it: the stored plan
// (no scope call), the ledger so far, no page re-read, spend cumulative.
// ponytail: issued queries, replanned angles and pivot bookkeeping aren't
// persisted — a resume may re-issue a query, and a pivotal fact keeps the
// provisional single-source verdict stored when it was marked.
func Run(ctx context.Context, d scrape.Deps, j Job) (rep Report, err error) {
	question := strings.TrimSpace(j.Question)
	r, err := prepare(d, j, question)
	if err != nil {
		return Report{}, err
	}
	rep.RunID = r.id

	rr, rerr := d.DB.GetResearchRun(r.id)
	resumed := rerr == nil && j.RunID != ""
	if resumed {
		if rr.Status == "done" {
			return Report{}, fmt.Errorf("research: run %s is done; a follow-up is a new run", r.id)
		}
		question = rr.Question
		if err := r.resume(rr); err != nil {
			return Report{}, err
		}
	} else if err := d.DB.BeginRun(r.id, "research"); err != nil {
		return Report{}, err
	}
	r.budget = NewBudget(d.DB, r.id, r.o)

	hasRow, declined := resumed, false
	defer func() { rep, err = r.finish(ctx, rep, err, hasRow, declined) }()

	if !resumed {
		if r.plan, err = r.scope(ctx, question, j.Approve); err != nil {
			var de declinedError
			if declined = errors.As(err, &de); declined {
				err = de.error
			}
			return rep, err
		}
		planJSON, perr := json.Marshal(r.plan)
		optsJSON, oerr := json.Marshal(r.o)
		if err = errors.Join(perr, oerr); err != nil {
			return rep, err
		}
		if err = d.DB.PutResearchRun(store.ResearchRun{RunID: r.id, Question: question, Plan: string(planJSON), Options: string(optsJSON)}); err != nil {
			return rep, err
		}
		hasRow = true
	}
	r.emit(Event{Stage: "plan", Detail: planLine(r.plan, resumed)})

	g, gctx := errgroup.WithContext(ctx)
	r.leadMu.Lock()
	for _, a := range r.plan.Angles {
		r.spawn(g, gctx, a)
	}
	r.leadMu.Unlock()
	if err = g.Wait(); err != nil {
		return rep, err
	}
	if err = ctx.Err(); err != nil {
		return rep, err
	}
	if err = r.settlePivots(); err != nil {
		return rep, err
	}

	if err = d.DB.SetResearchState(r.id, "writing", r.steering()); err != nil {
		return rep, err
	}
	r.emit(Event{Stage: "write"})
	md, cited, err := r.write(ctx, question)
	if err != nil {
		return rep, err
	}
	if err = d.DB.FinishResearchRun(r.id, "done", md); err != nil {
		return rep, err
	}
	rep.Markdown, rep.Cited = md, cited
	return rep, nil
}

// prepare validates the job with no I/O — the question, Normalized, the
// writer's and the judge's keys, the search backends — and builds the
// run.
func prepare(d scrape.Deps, j Job, question string) (*run, error) {
	if d.DB == nil {
		return nil, fmt.Errorf("research: nil DB")
	}
	if question == "" {
		return nil, fmt.Errorf("research: question: required")
	}
	if n := utf8.RuneCountInString(question); n > maxQuestionRunes {
		return nil, fmt.Errorf("research: question: %d runes, want ≤ %d", n, maxQuestionRunes)
	}
	o, err := j.Options.Normalized()
	if err != nil {
		return nil, err
	}
	r := &run{d: d, id: j.RunID, o: o, onEvent: j.OnEvent, ctl: j.Control,
		lim: crawl.NewHostLimiters(limitRPS, limitBurst), visited: map[string]bool{},
		pivots: map[string]*pivot{}, extractors: map[exKey]extract.Extractor{}}
	r.judgeP, r.judgeM = o.judge()
	for _, p := range []string{o.Provider, r.judgeP} {
		if extract.NeedsAPIKey(p) && r.key(p) == "" {
			return nil, fmt.Errorf("research: provider %s: %w", p, scrape.ErrMissingKey)
		}
	}
	avail := scrape.SearchProvidersFor(r.key)
	r.backends = avail
	if len(o.Search) > 0 {
		r.backends = o.Search
	}
	for _, b := range r.backends {
		switch {
		case slices.Contains(avail, b):
		case b == "searxng":
			return nil, fmt.Errorf("research: search: searxng needs MAGPIE_SEARXNG_URL (a self-hosted instance)")
		default:
			return nil, fmt.Errorf("research: search provider %s: %w", b, scrape.ErrMissingKey)
		}
	}
	if r.id == "" {
		r.id = store.NewRunID()
	}
	return r, nil
}

// resume reopens an unfinished run: run_history back to running, the
// stored steer and plan, and the visited set from the read ledger's done
// and error rows (a crashed pending row is simply read again).
func (r *run) resume(rr store.ResearchRun) error {
	if err := json.Unmarshal([]byte(rr.Plan), &r.plan); err != nil {
		return fmt.Errorf("research: resume %s: plan: %w", r.id, err)
	}
	if err := r.d.DB.ResumeRun(r.id); err != nil {
		return err
	}
	if err := r.d.DB.SetResearchState(r.id, "running", rr.Steer); err != nil {
		return err
	}
	r.steer = rr.Steer
	urls, err := r.d.DB.CrawlURLs(r.id)
	if err != nil {
		return err
	}
	for _, c := range urls {
		if c.Status == "done" || c.Status == "error" {
			r.visited[c.URL] = true
		}
	}
	facts, err := r.d.DB.Facts(r.id)
	if err != nil {
		return err
	}
	for _, f := range facts {
		r.facts++
		if slices.Contains(usableStatus, f.Status) {
			r.usable++
			r.findings = append(r.findings, finding(f.FactID, f.Claim, f.URL))
		}
	}
	return nil
}

// declinedError marks an Approve error: the run row finishes interrupted
// (the operator stopped it), not error. Run returns the inner error.
type declinedError struct{ error }

// scope drafts the plan within the effort's fan-out and passes it through
// Approve. Go bounds the fan-out, not the prompt (spec §9 risk 5).
func (r *run) scope(ctx context.Context, question string, approve func(Plan) (Plan, error)) (Plan, error) {
	n := efforts[r.o.Effort].SubResearchers
	var p Plan
	if err := r.call(ctx, false, "plan", planSchema, "Question: "+question, planInstr(r.o.Effort, n), &p); err != nil {
		return Plan{}, err
	}
	p = tidyPlan(p, n)
	if err := p.Validate(r.o); err != nil {
		return Plan{}, err
	}
	if approve == nil {
		return p, nil
	}
	edited, err := approve(p)
	if err != nil {
		return Plan{}, declinedError{err}
	}
	edited = tidyPlan(edited, n)
	return edited, edited.Validate(r.o)
}

// tidyPlan truncates to n angles, drops blank questions, and gives an
// angle with no queries its question as its query.
func tidyPlan(p Plan, n int) Plan {
	var out []Angle
	for _, a := range p.Angles {
		a.Question = oneLine(a.Question)
		if a.Question == "" {
			continue
		}
		var qs []string
		for _, q := range a.Queries {
			if q = oneLine(q); q != "" {
				qs = append(qs, q)
			}
		}
		if len(qs) == 0 {
			qs = []string{a.Question}
		}
		out = append(out, Angle{Question: a.Question, Queries: qs})
	}
	p.Brief = strings.TrimSpace(p.Brief)
	p.Angles = out[:min(len(out), n)]
	return p
}

func planLine(p Plan, resumed bool) string {
	qs := make([]string, len(p.Angles))
	for i, a := range p.Angles {
		qs[i] = a.Question
	}
	s := fmt.Sprintf("%d angle(s): %s", len(p.Angles), strings.Join(qs, " · "))
	if resumed {
		s = "resumed — " + s
	}
	return s
}

// finish is Run's one exit: it maps the outcome onto research_runs and
// run_history, emits done, and fills the report from the store (so a
// resumed run includes earlier work).
func (r *run) finish(ctx context.Context, rep Report, err error, hasRow, declined bool) (Report, error) {
	status, hist := "done", "finished"
	switch {
	case err == nil:
	case ctx.Err() != nil || declined:
		status, hist = "stopped", "interrupted"
	default:
		status, hist = "failed", "error"
	}
	var ferr error
	if err != nil && hasRow {
		ferr = r.d.DB.FinishResearchRun(r.id, status, "")
	}
	ok, er, cerr := r.readCounts()
	ferr = errors.Join(ferr, cerr, r.d.DB.FinishRun(r.id, ok, er, hist))
	rep.Status, rep.Plan = status, r.plan
	if rep.Facts, cerr = r.d.DB.Facts(r.id); cerr != nil {
		ferr = errors.Join(ferr, cerr)
	}
	urls, cerr := r.d.DB.CrawlURLs(r.id)
	ferr = errors.Join(ferr, cerr)
	for _, c := range urls {
		if c.Status == "error" {
			issue, detail, _ := strings.Cut(c.Msg, ": ")
			rep.Unreadable = append(rep.Unreadable, Unreadable{URL: c.URL, Issue: issue, Detail: detail})
		}
	}
	rep.SpentUSD, cerr = r.d.DB.RunCost(r.id)
	ferr = errors.Join(ferr, cerr)
	r.emit(Event{Stage: "done", Detail: status})
	if err == nil && ferr != nil {
		rep.Status = "failed"
	}
	return rep, errors.Join(err, ferr)
}

// readCounts is the read ledger's (done, error) totals — pages_ok and
// pages_err, resumed reads included.
func (r *run) readCounts() (ok, er int, err error) {
	urls, err := r.d.DB.CrawlURLs(r.id)
	for _, c := range urls {
		switch c.Status {
		case "done":
			ok++
		case "error":
			er++
		}
	}
	return ok, er, err
}

// emit sends one event: counts are read under mu, released, then the
// callback runs under evMu alone, so OnEvent never blocks the loop's lock
// and is never called concurrently.
func (r *run) emit(ev Event) {
	if r.onEvent == nil {
		return
	}
	r.mu.Lock()
	ev.Facts, ev.Usable = r.facts, r.usable
	r.mu.Unlock()
	r.evMu.Lock()
	defer r.evMu.Unlock()
	if spent, err := r.d.DB.RunCost(r.id); err == nil {
		r.spent = spent // on error, keep the last value
	}
	ev.SpentUSD, ev.CapUSD = r.spent, r.o.MaxCostUSD
	r.onEvent(ev)
}

func (r *run) warn(format string, args ...any) {
	r.emit(Event{Stage: "warn", Detail: fmt.Sprintf(format, args...)})
}

func (r *run) steering() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steer
}
