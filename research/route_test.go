package research_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/scrape"
)

type robotsFetcher map[string]string // robots.txt URL → body; "!err" = a fetch error

func (f robotsFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	body, ok := f[req.URL]
	switch {
	case body == "!err":
		return nil, errors.New("dial tcp: no route to host")
	case !ok:
		return &fetch.FetchResponse{URL: req.URL, StatusCode: 404}, nil
	}
	return &fetch.FetchResponse{URL: req.URL, StatusCode: 200, HTML: []byte(body)}, nil
}

// TestRoute: one read's routing — the hook, robots, the bucket — row by row.
func TestRoute(t *testing.T) {
	deps := scrape.Deps{APIKeyFor: func(string) string { return "k" },
		ExtractorFor: func(string, string, string, *extract.Schema, string) (extract.Extractor, error) { return nil, nil },
		Fetcher: robotsFetcher{
			"https://pub.example/robots.txt":  "User-agent: *\nDisallow: /private\n",
			"https://down.example/robots.txt": "!err",
			"https://slow.example/robots.txt": "User-agent: *\nCrawl-delay: 2\n",
		}}
	job := func(h *sessHook) research.Job {
		return research.Job{Question: "q", Session: h.fn, Options: research.Options{Provider: "openai", Model: "m", MaxCostUSD: 1,
			Sources: research.SourcePolicy{Sessions: []string{"paper.example"}}}}
	}
	floor := rate.Every(research.SessionGap())
	for _, tc := range []struct {
		name, url  string
		miss, fail bool
		key        string // "" = skipped (issue set)
		authed     bool
		issue      string
		check      func(t *testing.T, detail string, limit rate.Limit)
	}{
		{name: "session ok", url: "https://paper.example/a", key: "session:paper.example", authed: true,
			check: func(t *testing.T, _ string, l rate.Limit) {
				if l != floor {
					t.Errorf("limit = %v, want the session floor %v", l, floor)
				}
			}},
		{name: "www", url: "https://www.paper.example/a", key: "session:paper.example", authed: true},
		{name: "subdomain", url: "https://news.paper.example/a", key: "session:paper.example", authed: true},
		{name: "hook miss reads public", url: "https://paper.example/a", miss: true, key: "paper.example",
			check: func(t *testing.T, _ string, l rate.Limit) {
				if l == floor {
					t.Errorf("limit = %v: a logged-out read must not take the session floor", l)
				}
			}},
		{name: "hook error", url: "https://paper.example/a", fail: true, issue: "error",
			check: func(t *testing.T, d string, _ rate.Limit) {
				if !strings.HasPrefix(d, "session: ") || !strings.Contains(d, "keyring locked") || strings.Contains(d, "s3cret") {
					t.Errorf("detail = %q, want the hook's error behind \"session: \" and no cookie", d)
				}
			}},
		{name: "robots disallow", url: "https://pub.example/private/x", issue: "robots"},
		{name: "robots unreachable", url: "https://down.example/a", issue: "robots",
			check: func(t *testing.T, d string, _ rate.Limit) {
				if !strings.Contains(d, "unreachable") {
					t.Errorf("detail = %q, want unreachable", d)
				}
			}},
		{name: "crawl-delay", url: "https://slow.example/a", key: "slow.example",
			check: func(t *testing.T, _ string, l rate.Limit) {
				if l > rate.Every(2*time.Second) {
					t.Errorf("limit = %v, want ≤ %v (Crawl-delay: 2)", l, rate.Every(2*time.Second))
				}
			}},
		{name: "public allowed", url: "https://pub.example/open", key: "pub.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &sessHook{miss: tc.miss, fail: tc.fail}
			key, authed, issue, detail, limit, err := research.Route(context.Background(), deps, job(h), tc.url)
			if err != nil {
				t.Fatal(err)
			}
			if issue != tc.issue || authed != tc.authed || (tc.issue == "" && key != tc.key) {
				t.Errorf("route = (key %q, authed %v, issue %q, %q), want (%q, %v, %q)", key, authed, issue, detail, tc.key, tc.authed, tc.issue)
			}
			if tc.check != nil {
				tc.check(t, detail, limit)
			}
		})
	}

	j := job(&sessHook{})
	j.Session = nil
	if _, _, _, _, _, err := research.Route(context.Background(), deps, j, "https://paper.example/a"); err == nil || !strings.Contains(err.Error(), "this surface has no session source") {
		t.Errorf("no session source: err = %v", err)
	}
}
