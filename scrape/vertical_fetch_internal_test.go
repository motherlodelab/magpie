package scrape

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

// cannedFetcher is the minimal vertical.Fetcher fake for wrapper tests
// (fakeGatedFetcher lives in the external test package).
type cannedFetcher struct {
	body *fetch.FetchResponse
	err  error
}

func (f cannedFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

// Verticals fetch their own targets through the escalating fetcher; the
// wrapper's contract is pinned here (package-internal: the browser func
// is the injectable seam). Run-level tolerance lives in vertical_test.go.

func fakeBrowser(resp *fetch.FetchResponse, err error, calls *int) func(context.Context, string, Options) (*fetch.FetchResponse, error) {
	return func(context.Context, string, Options) (*fetch.FetchResponse, error) {
		*calls++
		return resp, err
	}
}

// TestVerticalFetcher_Escalation: a typed challenge gets exactly one rod
// escalation; browser noise never masks the typed error; non-challenge
// errors and successes pass through untouched.
func TestVerticalFetcher_Escalation(t *testing.T) {
	challenged := &fetch.ChallengeError{Vendor: "cloudflare", StatusCode: 403, URL: "https://x.test/a"}
	okResp := &fetch.FetchResponse{URL: "https://x.test/a", StatusCode: 200, HTML: []byte("<p>ok</p>"), Headers: http.Header{}}

	t.Run("challenge escalates to browser", func(t *testing.T) {
		calls := 0
		f := verticalFetcher{static: cannedFetcher{err: challenged}, browser: fakeBrowser(okResp, nil, &calls)}
		resp, err := f.Fetch(context.Background(), fetch.FetchRequest{URL: "https://x.test/a"})
		if err != nil || string(resp.HTML) != "<p>ok</p>" {
			t.Fatalf("Fetch = %v, %v — want browser response", resp, err)
		}
		if calls != 1 {
			t.Errorf("browser calls = %d, want 1", calls)
		}
	})

	t.Run("browser noise never masks the typed error", func(t *testing.T) {
		calls := 0
		f := verticalFetcher{static: cannedFetcher{err: challenged}, browser: fakeBrowser(nil, errors.New("rod boom"), &calls)}
		_, err := f.Fetch(context.Background(), fetch.FetchRequest{URL: "https://x.test/a"})
		var ce *fetch.ChallengeError
		if !errors.As(err, &ce) || ce.Vendor != "cloudflare" {
			t.Fatalf("err = %v, want the cloudflare ChallengeError", err)
		}
	})

	t.Run("non-challenge error passes through", func(t *testing.T) {
		calls := 0
		plain := errors.New("dns fail")
		f := verticalFetcher{static: cannedFetcher{err: plain}, browser: fakeBrowser(okResp, nil, &calls)}
		_, err := f.Fetch(context.Background(), fetch.FetchRequest{URL: "https://x.test/a"})
		if !errors.Is(err, plain) {
			t.Fatalf("err = %v, want the plain error", err)
		}
		if calls != 0 {
			t.Errorf("browser called %d times, want 0", calls)
		}
	})

	t.Run("success passes through without escalation", func(t *testing.T) {
		calls := 0
		f := verticalFetcher{static: cannedFetcher{body: &fetch.FetchResponse{URL: "https://x.test/a", StatusCode: 200, HTML: []byte("ok"), Headers: http.Header{}}}, browser: fakeBrowser(nil, nil, &calls)}
		resp, err := f.Fetch(context.Background(), fetch.FetchRequest{URL: "https://x.test/a"})
		if err != nil || string(resp.HTML) != "ok" {
			t.Fatalf("Fetch = %v, %v — want static response", resp, err)
		}
		if calls != 0 {
			t.Errorf("browser called %d times, want 0", calls)
		}
	})
}
