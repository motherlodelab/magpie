package research_test

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/store"
)

const runID = "r1" // every test has its own store

// prompt projects to p on gpt-4o-mini: 1000 tokens × $0.15/1M (newRun
// asserts it).
var (
	prompt = strings.Repeat("x", 4000)
	p      = extract.ProjectedCostWithFallback("gpt-4o-mini", prompt)
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

// newRun opens a store with a begun run and a Budget over normalized
// options, after asserting p's absolute value — every scenario reasons in
// dollars, so a price-table bump must fail here, loudly, not silently
// re-mean "$1".
func newRun(t *testing.T, maxUSD float64, tools int) (*store.DB, *research.Budget) {
	t.Helper()
	if math.Abs(p-0.00015) > 1e-12 {
		t.Fatalf("anchor moved: gpt-4o-mini projects %v for 1000 tokens, want 0.00015 — the price table changed", p)
	}
	db := openStore(t)
	if err := db.BeginRun(runID, "research"); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	o := mustNorm(t, research.Options{Provider: "openai", Model: "gpt-4o-mini", MaxCostUSD: maxUSD, MaxToolCalls: tools, Effort: "quick"})
	return db, research.NewBudget(db, runID, o)
}

// spend simulates spend the way production produces it: llm_calls rows
// accumulating into run_history.usd_estimate.
func spend(t *testing.T, db *store.DB, x float64) {
	t.Helper()
	if err := db.LogLLMCall(runID, store.LLMCall{Provider: "openai", Model: "gpt-4o-mini", USDEstimate: x, Purpose: "extract"}); err != nil {
		t.Fatalf("LogLLMCall: %v", err)
	}
}

func admitResearch(b *research.Budget) (func(), error) {
	return b.AdmitResearch("openai", "gpt-4o-mini", prompt)
}

func TestBudget_ResearchShare(t *testing.T) {
	t.Parallel()
	db, b := newRun(t, 1, 0)
	spend(t, db, 0.80)
	if _, err := admitResearch(b); err != nil {
		t.Fatalf("at 0.80 of $1: AdmitResearch = %v, want admitted (0.80 + p ≤ 0.85)", err)
	}
	spend(t, db, 0.06)
	if _, err := admitResearch(b); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("at 0.86: AdmitResearch = %v, want ErrWriteNow", err)
	}
	if _, err := b.AdmitWrite("openai", "gpt-4o-mini", prompt); err != nil {
		t.Errorf("at 0.86: AdmitWrite = %v, want admitted (the reserve is the write's)", err)
	}
	if err := b.Boundary(); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("at 0.86: Boundary = %v, want ErrWriteNow", err)
	}
}

// TestBudget_AdmitFinish: the judge that finishes an extracted page is
// admitted after write-now (its facts are paid for), but never past the
// research share (review of #60).
func TestBudget_AdmitFinish(t *testing.T) {
	t.Parallel()
	db, b := newRun(t, 1, 0)
	b.WriteNow()
	if _, err := admitResearch(b); !errors.Is(err, research.ErrWriteNow) {
		t.Fatalf("after write-now: AdmitResearch = %v, want ErrWriteNow", err)
	}
	release, err := research.AdmitFinish(b, "openai", "gpt-4o-mini", prompt)
	if err != nil {
		t.Fatalf("after write-now: AdmitFinish = %v, want admitted", err)
	}
	release()
	spend(t, db, 0.86)
	if _, err := research.AdmitFinish(b, "openai", "gpt-4o-mini", prompt); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("past the research share: AdmitFinish = %v, want ErrWriteNow", err)
	}
}

// TestBudget_WriteCeiling: the write's refusal is the existing exit-code
// sentinel, never the research-stage control flow — asserted both ways, so
// a swapped wrap can't pass.
func TestBudget_WriteCeiling(t *testing.T) {
	t.Parallel()
	db, b := newRun(t, 1, 0)
	spend(t, db, 0.99999)
	_, err := b.AdmitWrite("openai", "gpt-4o-mini", strings.Repeat("x", 400_000)) // p×100
	if !errors.Is(err, crawl.ErrCostCeiling) {
		t.Errorf("AdmitWrite = %v, want crawl.ErrCostCeiling", err)
	}
	if errors.Is(err, research.ErrWriteNow) {
		t.Errorf("AdmitWrite = %v, must not be ErrWriteNow", err)
	}
}

// TestBudget_InFlight: research share = 3.5p. The recomputed share may sit
// an ulp off 3.5p, but the decisions are at 3p (admit) and 4p (refuse) —
// margins of 0.5p, far above float noise, so no epsilon fudge.
func TestBudget_InFlight(t *testing.T) {
	t.Parallel()
	_, b := newRun(t, 3.5*p/(1-research.WriteReserve), 0)
	var releases []func()
	for i := range 3 {
		release, err := admitResearch(b)
		if err != nil {
			t.Fatalf("admit %d: %v, want admitted", i+1, err)
		}
		releases = append(releases, release)
	}
	if _, err := admitResearch(b); !errors.Is(err, research.ErrWriteNow) {
		t.Fatalf("4th admit = %v, want ErrWriteNow (3p in flight + p > 3.5p)", err)
	}
	releases[0]()
	if _, err := admitResearch(b); err != nil {
		t.Fatalf("after one release: %v, want admitted", err)
	}
	releases[0]() // a double release frees nothing
	if _, err := admitResearch(b); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("after a double release: %v, want ErrWriteNow (sync.Once)", err)
	}
}

// TestBudget_ConcurrentAdmits: the cap must hold when every sub-researcher
// admits at once. admit() holds b.mu across the RunCost read and the pending
// update, so the k-th admission sees pending=(k-1)*p under ANY schedule —
// this assert is exact by construction, not probabilistic. -race -count=3
// exists for the race detector, not for interleaving luck.
func TestBudget_ConcurrentAdmits(t *testing.T) {
	t.Parallel()
	_, b := newRun(t, 3.5*p/(1-research.WriteReserve), 0) // research share = 3.5p

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, refused := 0, 0
	for range n {
		wg.Go(func() {
			_, err := admitResearch(b) // no releases: every admit stays in flight
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case errors.Is(err, research.ErrWriteNow):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if admitted != 3 {
		t.Errorf("admitted = %d, want exactly 3 (⌊3.5p/p⌋)", admitted)
	}
	if refused != n-3 {
		t.Errorf("refused = %d, want %d", refused, n-3)
	}
}

func TestBudget_Tools(t *testing.T) {
	t.Parallel()
	_, b := newRun(t, 1, 3)
	for i := range 3 {
		if err := b.Tool(); err != nil {
			t.Fatalf("Tool %d: %v, want ok", i+1, err)
		}
	}
	if err := b.Boundary(); err == nil {
		t.Errorf("Boundary with 3/3 tools spent = nil, want ErrWriteNow")
	}
	if err := b.Tool(); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("4th Tool = %v, want ErrWriteNow", err)
	}
	if err := b.Boundary(); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("Boundary = %v, want ErrWriteNow", err)
	}
}

func TestBudget_WriteNow(t *testing.T) {
	t.Parallel()
	_, b := newRun(t, 1, 0)
	if err := b.Boundary(); err != nil {
		t.Fatalf("fresh Boundary = %v, want nil", err)
	}
	b.WriteNow()
	if err := b.Tool(); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("Tool = %v, want ErrWriteNow", err)
	}
	if _, err := admitResearch(b); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("AdmitResearch = %v, want ErrWriteNow", err)
	}
	if _, err := b.AdmitResearch("codex", "gpt-5", prompt); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("flat-rate AdmitResearch = %v, want ErrWriteNow (the stop holds for every provider)", err)
	}
	if err := b.Boundary(); !errors.Is(err, research.ErrWriteNow) {
		t.Errorf("Boundary = %v, want ErrWriteNow", err)
	}
	if _, err := b.AdmitWrite("openai", "gpt-4o-mini", prompt); err != nil {
		t.Errorf("AdmitWrite = %v, want admitted (write-now asks for exactly this)", err)
	}
}

// TestBudget_FlatRate: codex with a METERED model name — the projection is
// nonzero, so only the flat-rate short-circuit can admit at 5× the cap.
func TestBudget_FlatRate(t *testing.T) {
	t.Parallel()
	db, b := newRun(t, 1, 0)
	spend(t, db, 5.0)
	if _, err := b.AdmitResearch("codex", "gpt-4o-mini", prompt); err != nil {
		t.Errorf("AdmitResearch = %v, want admitted (flat rate)", err)
	}
	if _, err := b.AdmitWrite("codex", "gpt-4o-mini", prompt); err != nil {
		t.Errorf("AdmitWrite = %v, want admitted (flat rate)", err)
	}
}

func TestBudget_UnknownRun(t *testing.T) {
	t.Parallel()
	o := mustNorm(t, valid())
	b := research.NewBudget(openStore(t), "never-begun", o)
	_, err := admitResearch(b)
	if err == nil || errors.Is(err, research.ErrWriteNow) {
		t.Errorf("AdmitResearch on an unknown run = %v, want a store error that is not ErrWriteNow", err)
	}
	if err := b.Boundary(); err == nil || errors.Is(err, research.ErrWriteNow) {
		t.Errorf("Boundary on an unknown run = %v, want a store error that is not ErrWriteNow", err)
	}
}

// TestEstimateRun_Metered pins the call model (counts × token sizes × role
// routing) against the real price table — not the table's dollar values.
func TestEstimateRun_Metered(t *testing.T) {
	t.Parallel()
	got := research.EstimateRun(mustNorm(t, valid())) // standard: 3 sub-researchers, 45 tools → 22 reads
	E := func(in, out int) float64 { return extract.EstimateCost("gpt-4o-mini", in, out) }
	want := 1*E(1500, 800) + 3*E(3000, 800) + 22*E(3000, 600) + 22*E(1500, 300) + 1*E(12000, 3000)
	if math.Abs(got.USD-want) > 1e-12 {
		t.Errorf("USD = %v, want %v (hand sum)", got.USD, want)
	}
	if got.LLMCalls != 49 || got.ToolCalls != 45 || got.Unpriced {
		t.Errorf("got %+v, want 49 LLM calls, 45 tool calls, priced", got)
	}
}

func TestEstimateRun_Effort(t *testing.T) {
	t.Parallel()
	var ests []research.Estimate
	for _, effort := range []string{"quick", "standard", "deep"} {
		o := valid()
		o.Effort = effort
		ests = append(ests, research.EstimateRun(mustNorm(t, o)))
	}
	q, s, d := ests[0], ests[1], ests[2]
	if !(q.USD < s.USD && s.USD < d.USD) {
		t.Errorf("USD quick %v, standard %v, deep %v — want increasing", q.USD, s.USD, d.USD)
	}
	if q.LLMCalls != 13 || s.LLMCalls != 49 || d.LLMCalls != 107 {
		t.Errorf("LLM calls = %d/%d/%d, want 13/49/107", q.LLMCalls, s.LLMCalls, d.LLMCalls)
	}
}

// TestEstimateRun_Billing: the D3 switch. The unpriced row also proves no
// stderr warning by construction — EstimateCost (the only writer) sits in
// the metered arm, which a zero InputPrice never reaches.
func TestEstimateRun_Billing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, model string
		unpriced        bool
	}{
		{"codex", "gpt-4o-mini", false},   // flat
		{"ollama", "llama3.2", false},     // local
		{"openai", "no-such-model", true}, // unpriced
	} {
		o := valid()
		o.Provider, o.Model = tc.provider, tc.model
		got := research.EstimateRun(mustNorm(t, o))
		if got.USD != 0 || got.Unpriced != tc.unpriced {
			t.Errorf("%s/%s: %+v, want USD 0, Unpriced %v", tc.provider, tc.model, got, tc.unpriced)
		}
	}
}

func TestEstimateRun_Judge(t *testing.T) {
	t.Parallel()
	base := research.EstimateRun(mustNorm(t, valid()))
	delta := 22 * (extract.EstimateCost("claude-sonnet-5", 1500, 300) - extract.EstimateCost("gpt-4o-mini", 1500, 300))
	for _, j := range []struct{ provider, model string }{
		{"anthropic", "claude-sonnet-5"}, // the judge's own pair
		{"", "claude-sonnet-5"},          // same provider, different model
	} {
		o := valid()
		o.JudgeProvider, o.JudgeModel = j.provider, j.model
		got := research.EstimateRun(mustNorm(t, o))
		if math.Abs(got.USD-base.USD-delta) > 1e-12 {
			t.Errorf("judge %q/%q: USD %v − base %v = %v, want %v", j.provider, j.model, got.USD, base.USD, got.USD-base.USD, delta)
		}
		if got.LLMCalls != base.LLMCalls {
			t.Errorf("judge %q/%q: LLM calls %d, want %d", j.provider, j.model, got.LLMCalls, base.LLMCalls)
		}
	}
}
