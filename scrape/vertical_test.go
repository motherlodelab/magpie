package scrape_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/vertical"
)

// fakeVerticalFetcher is the 15-line scrape-side copy of the vertical fake
// (rule of three: 2 call sites, copy beats a shared harness).
type fakeVerticalFetcher struct {
	mu     sync.Mutex
	bodies map[string]fakeResp
	order  []string
}

type fakeResp struct {
	status int
	body   []byte
	err    error
}

func (f *fakeVerticalFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, req.URL)
	// Longest match wins; a trailing "$" anchors to the URL end.
	best, bestLen := "", -1
	for sub := range f.bodies {
		key, anchored := sub, false
		if strings.HasSuffix(key, "$") {
			key, anchored = strings.TrimSuffix(key, "$"), true
		}
		matched := strings.Contains(req.URL, key)
		if anchored {
			matched = strings.HasSuffix(req.URL, key)
		}
		if matched && len(key) > bestLen {
			best, bestLen = sub, len(key)
		}
	}
	if bestLen < 0 {
		return nil, fmt.Errorf("fake fetcher: unexpected URL %s (order=%v)", req.URL, f.order)
	}
	r := f.bodies[best]
	if r.err != nil {
		return nil, r.err
	}
	st := r.status
	if st == 0 {
		st = 200
	}
	return &fetch.FetchResponse{URL: req.URL, FinalURL: req.URL, StatusCode: st, HTML: r.body}, nil
}

func verticalFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vertical", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRun_VerticalAutoHit_ZeroLLM is the phase's load-bearing test: a github
// URL with Vertical:"auto" returns the vertical record through the fake
// fetcher while the LLM constructor reports any call as failure.
func TestRun_VerticalAutoHit_ZeroLLM(t *testing.T) {
	db := openScrapeDB(t)
	sch := testSchema(t)
	// Seed a cache row the hit MUST ignore (no-read proof).
	if err := db.PutSelectors("github.com", selector.SchemaHash(sch), `{"fields":{}}`, 3); err != nil {
		t.Fatalf("PutSelectors: %v", err)
	}
	fake := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"github.com/o/r":  {body: []byte(`<html><head><title>o/r</title></head><body><p>` + strings.Repeat("filler ", 60) + `</p></body></html>`)},
		"api.github.com/": {body: verticalFixtureBytes(t, "github-repo.json")},
	}}
	deps := scrape.Deps{
		DB:      db,
		Fetcher: fake,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			t.Error("ExtractorFor called on vertical hit — zero-LLM contract violated")
			return nil, errors.New("must not be called")
		},
		APIKeyFor: func(string) string { return "" }, // no key: hit must not need one
	}
	res, err := scrape.Run(context.Background(), deps, "https://github.com/o/r", scrape.Options{
		Render: "static", Schema: sch, UseCache: true, Vertical: "auto",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Vertical != "github_repo" {
		t.Errorf("Vertical = %q, want github_repo", res.Vertical)
	}
	want := map[string]any{
		"kind": "repo", "full_name": "o/r", "description": "demo",
		"stars": float64(42), "forks": float64(7), "language": "Go",
		"license": "mit", "open_issues": float64(3), "url": "https://github.com/o/r",
	}
	if !reflect.DeepEqual(res.Record, want) {
		t.Errorf("Record mismatch:\n got %#v\nwant %#v", res.Record, want)
	}
	if res.FromCache {
		t.Error("FromCache = true on vertical hit — vertical output must never ride the selector cache")
	}
	// No-write proof via seed identity: the row must still be the seed.
	got, ok, err := db.GetSelectors("github.com", selector.SchemaHash(sch))
	if err != nil || !ok || got != `{"fields":{}}` {
		t.Errorf("seeded cache row disturbed by vertical hit: got %q ok=%v err=%v", got, ok, err)
	}
	if len(fake.order) != 2 || !strings.Contains(fake.order[1], "api.github.com/repos/o/r") {
		t.Errorf("request order = %v, want [raw, api.github.com/repos/o/r]", fake.order)
	}
}

func TestRun_VerticalAutoMiss(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	// Fetcher nil here: miss on a localhost origin also proves the nil-default path.
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, "test-key"), scrapeOrigin(t, scrapeHTML), scrape.Options{
		Schema: testSchema(t), Render: "static", Provider: "fake", UseCache: true, Vertical: "auto",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Vertical != "" {
		t.Errorf("Vertical = %q on miss, want empty", res.Vertical)
	}
	if res.Record["title"] != "Widget" {
		t.Errorf("miss record = %v, want LLM Widget", res.Record)
	}
	if got := fx.total(); got != 1 {
		t.Errorf("miss made %d extractor calls, want 1", got)
	}
}

func TestRun_VerticalMissMarkdown(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{
		Render: "static", Vertical: "auto",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Record != nil || res.Vertical != "" {
		t.Errorf("markdown miss: Record=%v Vertical=%q, want nil/empty", res.Record, res.Vertical)
	}
	if res.Markdown == "" {
		t.Error("markdown miss returned empty markdown")
	}
}

func TestRun_VerticalExplicitMismatch(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	fake := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"github.com/o/r": {body: []byte(`<html><head><title>o/r</title></head><body><p>` + strings.Repeat("filler ", 60) + `</p></body></html>`)},
	}}
	deps := scrape.Deps{
		DB:      db,
		Fetcher: fake,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return "" },
	}
	_, err := scrape.Run(context.Background(), deps, "https://github.com/o/r", scrape.Options{
		Render: "static", Vertical: "reddit",
	})
	if !errors.Is(err, vertical.ErrURLMismatch) {
		t.Fatalf("err = %v, want ErrURLMismatch", err)
	}
	if !strings.Contains(err.Error(), "does not handle") {
		t.Errorf("err = %q, want 'does not handle'", err)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("mismatch made %d extractor calls, want 0", got)
	}
}

func TestRun_VerticalUnknownName(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	fake := &fakeVerticalFetcher{bodies: map[string]fakeResp{}}
	deps := scrape.Deps{
		DB:      db,
		Fetcher: fake,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return "" },
	}
	_, err := scrape.Run(context.Background(), deps, "https://github.com/o/r", scrape.Options{
		Render: "static", Vertical: "tumblr",
	})
	if err == nil || !strings.Contains(err.Error(), "tumblr") {
		t.Fatalf("err = %v, want error naming tumblr", err)
	}
	if len(fake.order) != 0 {
		t.Errorf("unknown name caused %d fetches, want 0 (validation first)", len(fake.order))
	}
}

func TestRun_VerticalErrorPropagates(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	fake := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"github.com/o/r":  {body: []byte(`<html><head><title>o/r</title></head><body><p>` + strings.Repeat("filler ", 60) + `</p></body></html>`)},
		"api.github.com/": {status: 500, body: []byte("outage")},
	}}
	deps := scrape.Deps{
		DB:      db,
		Fetcher: fake,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return "" },
	}
	_, err := scrape.Run(context.Background(), deps, "https://github.com/o/r", scrape.Options{
		Render: "static", Vertical: "auto",
	})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want hard HTTP 500", err)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("extractor error fell back to LLM (%d calls) — fail loudly instead", got)
	}
}

func TestRun_VerticalEmptyIsBitIdentical(t *testing.T) {
	origin := scrapeOrigin(t, scrapeHTML)
	mk := func() (scrape.Result, error) {
		fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
		db := openScrapeDB(t)
		return scrape.Run(context.Background(), fakeDeps(db, fx, ""), origin, scrape.Options{Render: "static"})
	}
	explicit := func() (scrape.Result, error) {
		fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
		db := openScrapeDB(t)
		return scrape.Run(context.Background(), fakeDeps(db, fx, ""), origin, scrape.Options{Render: "static", Vertical: ""})
	}
	a, err := mk()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := explicit()
	if err != nil {
		t.Fatalf("Run Vertical:\"\": %v", err)
	}
	if a.Markdown != b.Markdown || a.Rendered != b.Rendered {
		t.Error("Vertical:\"\" run differs from default run — insertion point leaked")
	}
}

func TestRun_VerticalExplicitHit(t *testing.T) {
	db := openScrapeDB(t)
	fake := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"github.com/o/r":  {body: []byte(`<html><head><title>o/r</title></head><body><p>` + strings.Repeat("filler ", 60) + `</p></body></html>`)},
		"api.github.com/": {body: verticalFixtureBytes(t, "github-repo.json")},
	}}
	deps := scrape.Deps{
		DB:      db,
		Fetcher: fake,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			t.Error("ExtractorFor called on explicit vertical hit")
			return nil, errors.New("must not be called")
		},
		APIKeyFor: func(string) string { return "" },
	}
	res, err := scrape.Run(context.Background(), deps, "https://github.com/o/r", scrape.Options{
		Render: "static", Vertical: "github_repo",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Vertical != "github_repo" || res.Record["full_name"] != "o/r" {
		t.Errorf("explicit hit = (%q, %v), want github_repo record", res.Vertical, res.Record)
	}
}

func TestRun_VerticalExplicitOptIn(t *testing.T) {
	db := openScrapeDB(t)
	fake := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"/products/w$":  {body: []byte(`<html><head><title>w</title></head><body><p>` + strings.Repeat("filler ", 60) + `</p></body></html>`)},
		"products/w.js": {body: verticalFixtureBytes(t, "shopify-product.json")},
	}}
	deps := scrape.Deps{
		DB:      db,
		Fetcher: fake,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			t.Error("ExtractorFor called on explicit opt-in hit")
			return nil, errors.New("must not be called")
		},
		APIKeyFor: func(string) string { return "" },
	}
	res, err := scrape.Run(context.Background(), deps, "https://shop.example/products/w", scrape.Options{
		Render: "static", Vertical: "shopify_product",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Vertical != "shopify_product" || res.Record["title"] != "Demo T-Shirt" {
		t.Errorf("opt-in hit = (%q, %v), want shopify record", res.Vertical, res.Record)
	}
}

// TestRun_VerticalSurvivesBlockedPage: the main page fetch is result
// context for a vertical, never a gate — a typed challenge on it must not
// kill a run whose extractor fetches its own targets (reddit-class: the
// HTML page is blocked, the record comes from elsewhere).
func TestRun_VerticalSurvivesBlockedPage(t *testing.T) {
	target := "https://escalation.test/watch?v=abc123def45"
	ex := vertical.Extractor{
		Info:  vertical.Info{Name: "escalationtest", Label: "Escalation test", Desc: "test-only"},
		Match: func(u *url.URL) bool { return u.Host == "escalation.test" },
		Extract: func(_ context.Context, _ vertical.Fetcher, _ *url.URL) (map[string]any, error) {
			return map[string]any{"title": "Widget"}, nil
		},
	}
	if err := vertical.Register(ex); err != nil {
		t.Fatalf("register: %v", err)
	}
	db := openScrapeDB(t)
	deps := fakeDeps(db, &fakeExtractor{}, "")
	deps.Fetcher = &fakeGatedFetcher{errs: map[string]error{target: &fetch.ChallengeError{Vendor: "cloudflare", StatusCode: 403, URL: target}}}
	res, err := scrape.Run(context.Background(), deps, target, scrape.Options{
		Render: "auto", Vertical: "escalationtest",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Record["title"] != "Widget" || res.Vertical != "escalationtest" {
		t.Errorf("record = (%q, %v), want the vertical record", res.Vertical, res.Record)
	}
}

// TestRun_VerticalSurvivesQualityGate: a 200 JS-shell main page fails the
// clean quality gate — that also must not kill a vertical run
// (youtube-class: the watch page renders as a consent shell under static
// fetch while the extractor's own targets carry the data).
func TestRun_VerticalSurvivesQualityGate(t *testing.T) {
	target := "https://escalation.test/shell"
	ex := vertical.Extractor{
		Info:  vertical.Info{Name: "escalationtest2", Label: "Escalation test", Desc: "test-only"},
		Match: func(u *url.URL) bool { return u.Host == "escalation.test" },
		Extract: func(_ context.Context, _ vertical.Fetcher, _ *url.URL) (map[string]any, error) {
			return map[string]any{"title": "Widget"}, nil
		},
	}
	if err := vertical.Register(ex); err != nil {
		t.Fatalf("register: %v", err)
	}
	db := openScrapeDB(t)
	deps := fakeDeps(db, &fakeExtractor{}, "")
	deps.Fetcher = &fakeGatedFetcher{bodies: map[string][]byte{target: []byte("<html><head><title>shell</title></head><body><p>hi</p></body></html>")}}
	res, err := scrape.Run(context.Background(), deps, target, scrape.Options{
		Render: "auto", Vertical: "escalationtest2",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Record["title"] != "Widget" {
		t.Errorf("record = %v, want the vertical record", res.Record)
	}
}
