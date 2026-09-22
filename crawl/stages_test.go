package crawl

// Tests for the pipeline-stage money guard (stages.go): checkCeiling is
// the "never exceed --max-cost" gate and its abort path was effectively
// uncovered (16.7%) when the file split surfaced it.

import (
	"context"
	"errors"
	"testing"

	"github.com/motherlodelab/magpie/store"
)

func newCeilingContext(t *testing.T, maxCost float64) *crawlContext {
	t.Helper()
	c := &crawlContext{db: openCrawlDB(t), opts: Options{MaxCost: maxCost}}
	c.runID = "r-ceiling"
	if err := c.db.BeginRun(c.runID, "crawl"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckCeiling_Disabled(t *testing.T) {
	// MaxCost unset: no ceiling, no cost query at all.
	c := newCeilingContext(t, 0)
	if err := c.checkCeiling("a prompt of any size at all"); err != nil {
		t.Errorf("checkCeiling with MaxCost 0 = %v, want nil", err)
	}
}

func TestCheckCeiling_FailClosed(t *testing.T) {
	// No run row → RunCost errors → the gate must fail CLOSED (return the
	// error), never open the spend path because the cost is unknown.
	c := &crawlContext{db: openCrawlDB(t), opts: Options{MaxCost: 1.00}, runID: "no-such-run"}
	err := c.checkCeiling("price")
	if err == nil {
		t.Fatal("checkCeiling with unreadable run cost = nil, want error (fail closed)")
	}
	if errors.Is(err, ErrCostCeiling) {
		t.Errorf("unreadable cost surfaced as ErrCostCeiling; want the underlying store error")
	}
}

func TestCheckCeiling_ProjectionTrips(t *testing.T) {
	c := newCeilingContext(t, 1e-9) // any projection exceeds this
	if err := c.checkCeiling("extract the price from this page"); !errors.Is(err, ErrCostCeiling) {
		t.Errorf("checkCeiling = %v, want ErrCostCeiling", err)
	}
}

func TestCheckCeiling_RunningCostTrips(t *testing.T) {
	c := newCeilingContext(t, 0.50)
	if err := c.db.LogLLMCall(c.runID, store.LLMCall{Provider: "fake", Model: "m", USDEstimate: 0.50, Purpose: "synth"}); err != nil {
		t.Fatal(err)
	}
	// Already spent the whole budget: even a one-token prompt must trip.
	if err := c.checkCeiling("price"); !errors.Is(err, ErrCostCeiling) {
		t.Errorf("checkCeiling at full budget = %v, want ErrCostCeiling", err)
	}
}

func TestCheckCeiling_FlatRateExempt(t *testing.T) {
	// The --max-cost flag contract ("flat-rate providers codex, opencode-go
	// exempt") is shared with scrape.CheckCostCeiling — crawl must honor it
	// too; a flat bill can never trip a USD ceiling.
	c := newCeilingContext(t, 1e-9)
	c.opts.Provider = "opencode-go"
	if err := c.checkCeiling("extract the price from this page"); err != nil {
		t.Errorf("checkCeiling flat-rate = %v, want nil (exempt)", err)
	}
}

func TestCheckCeiling_HeadroomPasses(t *testing.T) {
	c := newCeilingContext(t, 100.00)
	if err := c.db.LogLLMCall(c.runID, store.LLMCall{Provider: "fake", Model: "m", USDEstimate: 0.50, Purpose: "synth"}); err != nil {
		t.Fatal(err)
	}
	if err := c.checkCeiling("extract the price from this page"); err != nil {
		t.Errorf("checkCeiling with headroom = %v, want nil", err)
	}
}

// TestRun_CostCeilingAborts pins the end-to-end path: the first LLM spend
// attempt over the ceiling fails the run with ErrCostCeiling.
func TestRun_CostCeilingAborts(t *testing.T) {
	srv := relocateOrigin(t, fixtureHTML(t, "product-B.html"))
	fx := &fakeExtractor{script: map[string]map[string]any{"default": crawlTruth}}
	_, err := Run(context.Background(), Options{
		SeedURL: srv.URL + "/", Schema: mustTestSchema(t),
		MaxPages: 3, MaxDepth: 1, SameHost: true,
		FetchWorkers: 1, Rate: 1000, Format: "jsonl",
		Out: t.TempDir() + "/r.jsonl",
		DB:  openCrawlDB(t), Extractor: fx, MaxCost: 1e-9,
	})
	if !errors.Is(err, ErrCostCeiling) {
		t.Fatalf("Run err = %v, want ErrCostCeiling", err)
	}
}
