package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// ProviderHelp is the --provider value list, derived from the one home
// for the provider set (extract.Providers) so help text can't drift from
// what BuildExtractor accepts.
var ProviderHelp = strings.Join(extract.ProviderNames(), "|")

// newExtractor builds the provider adapter with run-logging attached —
// a thin wrapper over extract.BuildExtractor (the one home for the
// provider switch) that owns the llm_calls log closure and the usage
// exit code for unknown providers.
func newExtractor(provider, key, model string, sch *extract.Schema, db *store.DB, runID string) (extract.Extractor, error) {
	// Normalize once: adapters log this id into llm_calls, so raw flag
	// casing must never fork the accounting.
	provider = strings.ToLower(provider)
	log := func(purpose string, u extract.TokenUsage) {
		if err := db.LogLLMCall(runID, store.LLMCall{
			Provider: provider, Model: model,
			PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
			USDEstimate: u.USDEstimate, Purpose: purpose,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: log llm call: %v\n", err)
		}
	}
	ex, err := extract.BuildExtractor(provider, key, model, runID, sch, log)
	if errors.Is(err, extract.ErrUnknownProvider) {
		return nil, fail(2, "%v", err)
	}
	return ex, err
}

// openCmdDB opens the cache DB from resolved config. Called after all
// flag validation so usage errors fail before any I/O or file creation.
// Caller defers closeDB(db). One home so new commands stop copying the
// open + nolint-close pair (it already drifted 4×).
func openCmdDB(cfg config.Config) (*store.DB, error) {
	return store.Open(cfg.CacheDB)
}

// closeDB closes the command DB; end-of-command close errors are unactionable.
func closeDB(db *store.DB) {
	_ = db.Close() //nolint:errcheck // end of command; close error unactionable
}

// setKeyHint is the single home for the API-key setup hint, shared by
// the pre-flight checks and the pipeline error path.
const setKeyHint = "set via --api-key flag, MAGPIE_* env, or `magpie config set-key`"

// scrapeDeps builds the pipeline Deps over db with the CLI extractor
// wiring. Shared so batch's goroutine fan-out uses the exact same
// constructors as single scrapes (thread-safety contract: see Deps).
func scrapeDeps(db *store.DB, cfg config.Config) scrape.Deps {
	return scrape.Deps{
		DB: db,
		ExtractorFor: func(p, key, m string, s *extract.Schema, runID string) (extract.Extractor, error) {
			return newExtractor(p, key, m, s, db, runID)
		},
		APIKeyFor: cfg.APIKey,
	}
}

// missingKeyErr builds the pre-flight missing-key failure. It wraps
// scrape.ErrMissingKey so exitFor decides the code (7) — the message
// matches the historical text exactly.
func missingKeyErr(provider string) error {
	return fmt.Errorf("%w for %s: %s", scrape.ErrMissingKey, provider, setKeyHint)
}

// keyHint attaches the set-key hint to a missing-key error from the
// pipeline (which already names the provider), preserving the chain so
// exitFor still maps it to 7. All other errors pass through unchanged —
// their messages self-describe (quality, cost ceiling, robots, SSRF).
func keyHint(err error) error {
	if errors.Is(err, scrape.ErrMissingKey) {
		return fmt.Errorf("%w: %s", err, setKeyHint)
	}
	return err
}

// autoProviders filters scrape.AutoProviderOrder to usable providers:
// keyed entries need a configured key, keyless (ollama/codex) always
// qualify. The canonical order lives in scrape (Summarize needs it too);
// this is the CLI view over the same list.
func autoProviders(cfg config.Config) []string {
	var out []string
	for _, p := range scrape.AutoProviderOrder {
		if needsAPIKey(p) && cfg.APIKey(p) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// checkCostCeiling fails closed: an unknown running total aborts before any
// LLM spend. promptText is the full prompt that ProjectedCost prices.
// Flat-rate providers (codex, opencode-go) bill the subscription, not the
// call, so the ceiling exempts them immediately instead of projecting a
// bogus per-token floor.
func checkCostCeiling(db *store.DB, runID, provider, model, promptText string, maxCost float64) error {
	if maxCost <= 0 {
		return nil
	}
	if extract.IsFlatRateProvider(provider) {
		return nil
	}
	running, err := db.RunCost(runID)
	if err != nil {
		return err // fail closed: never spend against an unknown total
	}
	proj := extract.ProjectedCost(model, promptText)
	// When price is unknown (0), any positive ceiling with real content aborts:
	// estimate prompt tokens × a reference floor so a near-zero ceiling trips.
	if proj == 0 {
		if toks := extract.EstimatePromptTokens(promptText); toks > 0 {
			proj = float64(toks) / 1e6 * 2.00 // reference input price floor
		}
	}
	if running+proj > maxCost {
		// crawl.ErrCostCeiling's text ("cost ceiling exceeded") ends the
		// message — don't repeat it in front.
		return fmt.Errorf("running %.6f + projected %.6f > max %.6f: %w", running, proj, maxCost, crawl.ErrCostCeiling)
	}
	return nil
}

type markdownOut struct {
	URL            string             `json:"url"`
	FinalURL       string             `json:"final_url"`
	Title          string             `json:"title"`
	Markdown       string             `json:"markdown"`
	StructuredData json.RawMessage    `json:"structured_data"`
	XHR            []fetch.XHRCapture `json:"xhr,omitempty"`
}

type usageOut struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	USDEstimate      float64 `json:"usd_estimate"`
	Attempts         int     `json:"attempts"`
}

type extractedOut struct {
	URL       string         `json:"url"`
	FinalURL  string         `json:"final_url"`
	Title     string         `json:"title"`
	Extracted map[string]any `json:"extracted"`
	Usage     usageOut       `json:"usage"`
	FromCache bool           `json:"from_cache"`
}

func marshalOut(v any, what string) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("%s: marshal output: %w", what, err)
	}
	return string(b), nil
}

func writeOut(path, s string) error {
	if path == "" {
		fmt.Println(s)
		return nil
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return fmt.Errorf("write out: %w", err)
	}
	return nil
}

// needsAPIKey reports whether a provider needs an API key: ollama is local,
// codex shells out to the user's own logged-in CLI.
func needsAPIKey(p string) bool {
	switch strings.ToLower(p) {
	case "ollama", "codex":
		return false
	}
	return true
}

func uuidNew() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%x-%d", b, os.Getpid())
	}
	// ponytail: timestamp+pid fallback on RNG failure; collision needs same-ns fork + broken RNG.
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}
