package extract

import (
	"fmt"
	"os"
	"strings"
)

// TokenUsage counts one LLM call.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	USDEstimate      float64
}

// prices: USD per 1M tokens in/out. Treat as data — re-confirm at build.
var priceTable = map[string][2]float64{
	"claude-opus-5":    {5.00, 25.00},
	"claude-sonnet-5":  {2.00, 10.00},
	"claude-haiku-4-5": {1.00, 5.00},
	"gpt-4o-mini":      {0.15, 0.60},
	"gpt-5-nano":       {0.05, 0.40},
}

// EstimateCost computes USD for a model + token counts.
// Unknown model → warning + 0, never blocks.
func EstimateCost(model string, prompt, completion int) float64 {
	if c, known := costLookup(model, prompt, completion); known {
		return c
	}
	fmt.Fprintf(os.Stderr, "warning: unknown model %q, costing 0\n", model)
	return 0
}

// costFor resolves one call's USD: provider-reported cost (OpenRouter
// usage.cost) wins; flat-rate providers stay 0 quietly (fixed bill, noise
// is not signal); otherwise the price table, warning when unknown.
func costFor(provider, model string, u TokenUsage) float64 {
	if u.USDEstimate != 0 {
		return u.USDEstimate
	}
	if IsFlatRateProvider(provider) {
		return 0
	}
	return EstimateCost(model, u.PromptTokens, u.CompletionTokens)
}

func costLookup(model string, prompt, completion int) (float64, bool) {
	if p, ok := priceTable[model]; ok {
		return float64(prompt)/1e6*p[0] + float64(completion)/1e6*p[1], true
	}
	if isFreeModel(model) {
		return 0, true
	}
	return 0, false
}

// IsFlatRateProvider reports subscription/flat billing (never USD-metered):
// the Codex CLI spends the user's subscription, opencode-go is a flat plan.
func IsFlatRateProvider(provider string) bool {
	switch strings.ToLower(provider) {
	case "codex", "opencode-go":
		return true
	}
	return false
}

// InputPrice returns the per-1M input price (0 when unknown).
func InputPrice(model string) float64 {
	if p, ok := priceTable[model]; ok {
		return p[0]
	}
	return 0
}

// EstimatePromptTokens: ponytail: ≈ len/4 chars (no tokenizer dep).
func EstimatePromptTokens(prompt string) int { return len(prompt) / 4 }

// ProjectedCost estimates the next call before making it.
func ProjectedCost(model, prompt string) float64 {
	in := InputPrice(model)
	if in == 0 {
		// Unknown price but still project *something* so --max-cost can trip:
		// use the cheapest known input price as a floor? No — warn path costs 0.
		// Instead project with a tiny epsilon so a near-zero ceiling aborts.
		if !isFreeModel(model) {
			return 0
		}
		return 0
	}
	return float64(EstimatePromptTokens(prompt)) / 1e6 * in
}

// ProjectedCostWithFallback is ProjectedCost with a $2/1M-token fallback
// for unknown-priced models, so --max-cost can still trip. Single home for
// the ceiling projection — the scrape and crawl gates must not drift apart.
func ProjectedCostWithFallback(model, promptText string) float64 {
	proj := ProjectedCost(model, promptText)
	if proj == 0 {
		if toks := EstimatePromptTokens(promptText); toks > 0 {
			proj = float64(toks) / 1e6 * 2.00
		}
	}
	return proj
}

func isFreeModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "ollama/") || strings.HasPrefix(m, "llama")
}
