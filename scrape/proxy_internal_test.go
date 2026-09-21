package scrape

// Batch B: the per-run Proxy rides extractor sub-fetches — extractors
// build their own FetchRequest and can't see run options, so
// verticalFetcher is the injection point (main page gets it via
// fetchURL). cannedFetcher/fakeBrowser live in the wrapper tests, same
// package.

import (
	"context"
	"net/http"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

func TestVerticalFetcher_InjectsProxy(t *testing.T) {
	const proxy = "socks5://p.example:1080"
	okResp := &fetch.FetchResponse{URL: "https://x.test/a", StatusCode: 200, HTML: []byte("<p>ok</p>"), Headers: http.Header{}}

	t.Run("sub-fetch carries the run proxy", func(t *testing.T) {
		var seen fetch.FetchRequest
		static := capturingFetcher{fn: func(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
			seen = req
			return okResp, nil
		}}
		f := verticalFetcher{static: static, o: Options{Proxy: proxy}}
		if _, err := f.Fetch(context.Background(), fetch.FetchRequest{URL: "https://x.test/a"}); err != nil {
			t.Fatal(err)
		}
		if seen.Proxy != proxy {
			t.Errorf("sub-fetch Proxy = %q, want %q (extractor can't see run options)", seen.Proxy, proxy)
		}
	})

	t.Run("escalation browser opts carry the run proxy", func(t *testing.T) {
		challenged := &fetch.ChallengeError{Vendor: "cloudflare", StatusCode: 403, URL: "https://x.test/a"}
		var gotOpts Options
		f := verticalFetcher{static: cannedFetcher{err: challenged}, o: Options{Proxy: proxy}}
		f.browser = func(_ context.Context, _ string, o Options) (*fetch.FetchResponse, error) {
			gotOpts = o
			return okResp, nil
		}
		if _, err := f.Fetch(context.Background(), fetch.FetchRequest{URL: "https://x.test/a"}); err != nil {
			t.Fatal(err)
		}
		if gotOpts.Proxy != proxy {
			t.Errorf("escalation Proxy = %q, want %q (the browser must ride the same egress)", gotOpts.Proxy, proxy)
		}
	})
}

// capturingFetcher records the request a caller hands the fetcher
// (cannedFetcher swallows it).
type capturingFetcher struct {
	fn func(context.Context, fetch.FetchRequest) (*fetch.FetchResponse, error)
}

func (f capturingFetcher) Fetch(ctx context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	return f.fn(ctx, req)
}
