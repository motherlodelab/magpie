package scrape_test

import (
	"context"
	"testing"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// TestPrompt_CostFallback: PromptText only reports USD when the provider
// does (OpenRouter's usage.cost), so Prompt applies extract.CostFor before
// logging — a metered provider gets the price-table cost in both the result
// and the llm_calls row, a flat-rate one stays 0.
func TestPrompt_CostFallback(t *testing.T) {
	want := extract.EstimateCost("gpt-4o-mini", 10, 5)
	if want <= 0 {
		t.Fatalf("price table has no gpt-4o-mini cost (%v)", want)
	}
	for provider, usd := range map[string]float64{"openai": want, "codex": 0} {
		fx := &fakePrompterExtractor{t: t, promptScript: []string{"ok"}}
		db := openScrapeDB(t)
		runID := store.NewRunID()
		if err := db.BeginRun(runID, "summarize"); err != nil {
			t.Fatal(err)
		}
		d := scrape.Deps{
			DB: db,
			ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
				return fx, nil
			},
			APIKeyFor: func(string) string { return "test-key" },
		}
		res, err := scrape.Prompt(context.Background(), d, runID, scrape.PromptOptions{
			Provider: provider, Model: "gpt-4o-mini", System: "s", User: "u", Purpose: "summarize",
		})
		if err != nil {
			t.Fatalf("%s: Prompt: %v", provider, err)
		}
		if res.Usage.USDEstimate != usd {
			t.Errorf("%s: result USD = %v, want %v", provider, res.Usage.USDEstimate, usd)
		}
		calls, err := db.LLMCalls(runID)
		if err != nil || len(calls) != 1 {
			t.Fatalf("%s: llm_calls = %v (%v), want one row", provider, calls, err)
		}
		if calls[0].USDEstimate != usd {
			t.Errorf("%s: row USD = %v, want %v", provider, calls[0].USDEstimate, usd)
		}
	}
}
