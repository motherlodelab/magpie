// Vertical dispatch plumbing: the challenge-escalating fetcher handed to
// zero-LLM extractors, and the extractor runner. Split from scrape.go —
// same package, no import changes.

package scrape

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/vertical"
)

// verticalFetcher wraps the static fetcher with the main path's G.2
// semantics for extractors' own fetches: a typed challenge gets exactly
// one rod escalation (a real browser often clears it; honor o.CDP). The
// typed error stays primary when the browser can't clear it — launch
// noise never masks the vendor. Actions/CaptureXHR are stripped: they were
// authored for the main page, not the extractor's sub-fetch URLs. Zero-value
// browser is fetchBrowser; tests inject a fake via the browser field.
type verticalFetcher struct {
	static  vertical.Fetcher
	o       Options
	browser func(ctx context.Context, rawURL string, o Options) (*fetch.FetchResponse, error)
}

func (f verticalFetcher) Fetch(ctx context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	// Extractors build their own FetchRequest and can't see run options —
	// the per-run egress rides every sub-fetch (main page already got it
	// via fetchURL).
	req.Proxy = f.o.Proxy
	resp, err := f.static.Fetch(ctx, req)
	if err == nil {
		return resp, nil
	}
	var ce *fetch.ChallengeError
	if !errors.As(err, &ce) {
		return nil, err
	}
	browser := f.browser
	if browser == nil {
		browser = fetchBrowser
	}
	if bresp, berr := browser(ctx, req.URL, Options{CDP: f.o.CDP, Lang: f.o.Lang, Proxy: f.o.Proxy}); berr == nil {
		return bresp, nil
	}
	return nil, err
}

// runVertical runs one zero-LLM extractor over a challenge-escalating
// fetcher (profiles exist for challenge-prone HTML pages, not registry
// APIs). No selector cache interaction (vertical output isn't
// selector-derived; caching it would poison schema-keyed lookups), no
// LLM. Extractor errors are hard errors — never a silent LLM fallback.
// Run's deferred finish owns the run-row outcome; this stays pure.
func runVertical(ctx context.Context, vf vertical.Fetcher, rawURL string, ex vertical.Extractor, base Result) (Result, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return Result{}, fmt.Errorf("scrape: vertical %s: %w", ex.Info.Name, err)
	}
	m, err := ex.Extract(ctx, vf, u)
	if err != nil {
		return Result{}, err
	}
	base.Record = m
	base.Vertical = ex.Info.Name
	return base, nil
}
