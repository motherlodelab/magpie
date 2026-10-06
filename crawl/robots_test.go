package crawl

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/fetch"
)

// robotsFake answers robots.txt by URL: a status and body, or an error.
type robotsFake struct {
	mu    sync.Mutex
	code  int
	body  string
	err   error
	calls int
	last  fetch.FetchRequest
}

func (f *robotsFake) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	return &fetch.FetchResponse{URL: req.URL, StatusCode: f.code, HTML: []byte(f.body)}, nil
}

// TestChecker_UseFetcher: robots.txt through the run's fetcher maps status
// codes exactly as the client path does, once per host.
func TestChecker_UseFetcher(t *testing.T) {
	ctx := context.Background()
	f := &robotsFake{code: 200, body: "User-agent: *\nDisallow: /x\nCrawl-delay: 2\n"}
	c := NewChecker()
	c.UseFetcher(f)
	if ok, err := c.Allowed(ctx, "https://a.example/x"); ok || err != nil {
		t.Errorf("Allowed(/x) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := c.Allowed(ctx, "https://a.example/y"); !ok || err != nil {
		t.Errorf("Allowed(/y) = %v, %v; want true, nil", ok, err)
	}
	if d := c.CrawlDelay(ctx, "https://a.example/y"); d != 2*time.Second {
		t.Errorf("CrawlDelay = %v, want 2s", d)
	}
	if f.calls != 1 {
		t.Errorf("robots fetched %d times for one host, want 1 (cached)", f.calls)
	}
	if f.last.URL != "https://a.example/robots.txt" || !slices.Contains(f.last.Headers, "User-Agent: "+robotsUA) {
		t.Errorf("request = %s %q, want robots.txt with the robots UA", f.last.URL, f.last.Headers)
	}

	for _, tc := range []struct {
		name        string
		f           *robotsFake
		ok, unreach bool
	}{
		{"404 allows all", &robotsFake{code: 404}, true, false},
		{"503 is unreachable", &robotsFake{code: 503}, false, true},
		{"fetch error is unreachable", &robotsFake{err: errors.New("dial tcp: refused")}, false, true},
	} {
		c := NewChecker()
		c.UseFetcher(tc.f)
		ok, err := c.Allowed(ctx, "https://b.example/page")
		if ok != tc.ok || errors.Is(err, ErrRobotsUnreachable) != tc.unreach {
			t.Errorf("%s: Allowed = %v, %v; want %v (unreachable %v)", tc.name, ok, err, tc.ok, tc.unreach)
		}
	}
}
