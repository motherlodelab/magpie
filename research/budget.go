package research

import (
	"errors"
	"fmt"
	"sync"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/store"
)

// WriteReserve is the share of MaxCostUSD held back for the forced write:
// research stops at 85% so a report always gets written ⌗ (Jina's rule).
const WriteReserve = 0.15

// ErrWriteNow ends the research phase early: the operator asked, the tool
// calls are spent, or the research share of the cap is. The loop's one
// errors.Is arm moves to the write stage; Run never returns it.
var ErrWriteNow = errors.New("research: write now")

// Budget is one run's spend governor, shared by the lead and every
// sub-researcher (safe for concurrent use). The run_history row must exist
// (BeginRun) — its usd_estimate is the running spend.
//
// Admission is check-and-reserve under one mutex: running + in flight +
// projected ≤ cap. A bare check-then-call (scrape.CheckCostCeiling) lets N
// concurrent callers see the same running spend and overshoot by N calls.
//
// ponytail: the projection is input-side only (the core-wide gate,
// extract.ProjectedCostWithFallback, so research trips the way scrape and
// crawl do). An admitted call's output tokens and an Extract's repair
// attempts land after admission, so actual spend can pass a cap by the
// in-flight calls' output: the reserve absorbs that for research calls, and
// the write can pass the full cap by its own output — the same "no call
// starts past the cap" contract --max-cost has. Upgrade: project output as
// max-tokens × output price once extract exposes an output price.
type Budget struct {
	db       *store.DB
	runID    string
	maxUSD   float64
	maxTools int

	mu       sync.Mutex
	pending  float64 // projected USD of admitted calls not yet released
	tools    int
	writeNow bool
}

// NewBudget governs runID's spend; o must come from Normalized.
func NewBudget(db *store.DB, runID string, o Options) *Budget {
	return &Budget{db: db, runID: runID, maxUSD: o.MaxCostUSD, maxTools: o.MaxToolCalls}
}

func (b *Budget) researchCap() float64 { return b.maxUSD * (1 - WriteReserve) }

// AdmitResearch admits one research-stage LLM call (plan, replan, extract,
// judge) against MaxCostUSD × (1 − WriteReserve); refusal is ErrWriteNow.
// On success, call release exactly once after the call returns — the
// adapter has logged every attempt's cost by then, so the brief
// double-count is conservative; extra calls are no-ops.
func (b *Budget) AdmitResearch(provider, model, prompt string) (release func(), err error) {
	return b.admit(b.researchCap(), ErrWriteNow, true, provider, model, prompt)
}

// admitFinish admits the call that finishes a page already extracted (its
// judge): against the research share like AdmitResearch, but write-now
// doesn't refuse it — the page's facts are stored and paid for, and a
// refusal would strand them unverified.
func (b *Budget) admitFinish(provider, model, prompt string) (release func(), err error) {
	return b.admit(b.researchCap(), ErrWriteNow, false, provider, model, prompt)
}

// AdmitWrite admits the forced write against the full MaxCostUSD; refusal
// wraps crawl.ErrCostCeiling (the existing exit-code arm). It ignores
// write-now — that is what write-now asks for. Release as AdmitResearch.
func (b *Budget) AdmitWrite(provider, model, prompt string) (release func(), err error) {
	return b.admit(b.maxUSD, crawl.ErrCostCeiling, false, provider, model, prompt)
}

func (b *Budget) admit(limit float64, sentinel error, research bool, provider, model, prompt string) (func(), error) {
	flat := extract.IsFlatRateProvider(provider)
	var proj float64
	if !flat {
		proj = extract.ProjectedCostWithFallback(model, prompt)
	}
	// Held across the RunCost read on purpose: check-and-reserve must be
	// atomic, and a local SQLite read takes microseconds.
	b.mu.Lock()
	defer b.mu.Unlock()
	// Write-now before the flat-rate pass: the operator's stop holds for
	// every provider, not just metered ones.
	if research && b.writeNow {
		return nil, fmt.Errorf("write requested: %w", ErrWriteNow)
	}
	if flat { // a fixed bill: no cap to spend against (CheckCostCeiling's rule)
		return func() {}, nil
	}
	running, err := b.db.RunCost(b.runID)
	if err != nil {
		return nil, err // an unknown run is a bug, not a stop signal
	}
	if running+b.pending+proj > limit {
		return nil, fmt.Errorf("running %.6f + in flight %.6f + projected %.6f > %.6f: %w",
			running, b.pending, proj, limit, sentinel)
	}
	b.pending += proj
	var once sync.Once // a double release must not open a hole in the cap
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.pending -= proj
		})
	}, nil
}

// Tool counts one search or page read; ErrWriteNow when write-now is set or
// MaxToolCalls is spent.
func (b *Budget) Tool() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.stoppedLocked(); err != nil {
		return err
	}
	b.tools++
	return nil
}

// WriteNow is the operator's "stop and write now".
func (b *Budget) WriteNow() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writeNow = true
}

// Boundary is checked at every stage boundary: ErrWriteNow on write-now,
// tool calls spent, or running spend at the research share. A RunCost
// error is returned as-is.
func (b *Budget) Boundary() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.stoppedLocked(); err != nil {
		return err
	}
	running, err := b.db.RunCost(b.runID)
	if err != nil {
		return err
	}
	if c := b.researchCap(); running >= c {
		return fmt.Errorf("running %.6f >= research share %.6f: %w", running, c, ErrWriteNow)
	}
	return nil
}

// stoppedLocked reports write-now or spent tool calls; b.mu must be held.
func (b *Budget) stoppedLocked() error {
	if b.writeNow {
		return fmt.Errorf("write requested: %w", ErrWriteNow)
	}
	if b.tools >= b.maxTools {
		return fmt.Errorf("tool calls %d/%d spent: %w", b.tools, b.maxTools, ErrWriteNow)
	}
	return nil
}

// Estimate is a pre-run projection — a cap is the guarantee, this is not.
type Estimate struct {
	LLMCalls  int
	ToolCalls int     // the run's tool-call cap
	USD       float64 // priced calls only: flat and local providers cost 0
	Unpriced  bool    // a role's model has no price on file (the gate still projects $2/1M for it)
}

// EstimateRun prices one run's call model; o must come from Normalized.
// Each role is priced with the desktop D3 billing switch — flat, local,
// unpriced, metered — and metered calls with extract.EstimateCost (input +
// output; output bills ~5× input, so an input-only projection would
// halve the estimate). EstimateCost is reached only for priced models, so
// it never prints its unknown-model warning.
// ponytail: token sizes are guesses (the extract page is the same 12 000
// chars as desktop D3's typical page) until DR7 measures per-stage medians.
func EstimateRun(o Options) Estimate {
	e := efforts[o.Effort]
	reads := max(1, o.MaxToolCalls/2) // searches cost no LLM; about half the tool calls are reads
	jp, jm := o.judge()
	est := Estimate{ToolCalls: o.MaxToolCalls}
	for _, c := range []struct {
		n               int
		provider, model string
		in, out         int
	}{
		{1, o.Provider, o.Model, 1500, 800},                // plan
		{e.SubResearchers, o.Provider, o.Model, 3000, 800}, // replan
		{reads, o.Provider, o.Model, 3000, 600},            // extract
		{reads, jp, jm, 1500, 300},                         // judge (facts batched per source)
		{1, o.Provider, o.Model, 12000, 3000},              // write
	} {
		est.LLMCalls += c.n
		switch {
		case extract.IsFlatRateProvider(c.provider), !extract.NeedsAPIKey(c.provider): // flat or local
		case extract.InputPrice(c.model) == 0:
			est.Unpriced = true
		default:
			est.USD += float64(c.n) * extract.EstimateCost(c.model, c.in, c.out)
		}
	}
	return est
}
