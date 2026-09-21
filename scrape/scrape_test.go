package scrape_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"
)

type fakeExtractor struct {
	mu     sync.Mutex
	calls  int
	script map[string]any
	err    error
}

func (f *fakeExtractor) Name() string { return "fake" }

func (f *fakeExtractor) Extract(ctx context.Context, in extract.ExtractInput) (extract.ExtractResult, error) {
	if err := ctx.Err(); err != nil {
		return extract.ExtractResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return extract.ExtractResult{}, f.err
	}
	raw, merr := json.Marshal(f.script)
	if merr != nil {
		return extract.ExtractResult{}, merr
	}
	return extract.ExtractResult{Record: f.script, Raw: raw, Provider: "fake", Model: "fake", Attempts: 1}, nil
}

func (f *fakeExtractor) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const scrapeHTML = `<html><head><title>Widget</title></head><body><h1>Widget</h1>` +
	`<p>Plenty of honest descriptive prose keeps the static renderer in charge without any browser escalation at all.</p>` +
	`<p>A second paragraph of harmless filler text pushes visible length safely past the two-hundred character threshold.</p>` +
	`</body></html>`

func scrapeOrigin(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body)) //nolint:errcheck // httptest local; short write unactionable
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func openScrapeDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
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

func testSchema(t *testing.T) *extract.Schema {
	t.Helper()
	sch, err := extract.ParseSchema([]byte(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`))
	if err != nil {
		t.Fatalf("ParseSchema: %v", err)
	}
	return sch
}

func fakeDeps(db *store.DB, fx *fakeExtractor, key string) scrape.Deps {
	return scrape.Deps{
		DB: db,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return key },
	}
}

func TestRun_MarkdownOnly(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{Render: "static"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Markdown == "" {
		t.Error("markdown-only Run returned empty markdown")
	}
	if res.Record != nil {
		t.Errorf("markdown-only Run record = %v, want nil", res.Record)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("markdown-only Run made %d extractor calls, want 0", got)
	}
}

func TestRun_Schema(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, "test-key"), scrapeOrigin(t, scrapeHTML), scrape.Options{
		Schema: testSchema(t), Render: "static", Provider: "fake", UseCache: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Record["title"] != "Widget" {
		t.Errorf("record = %v, want title Widget", res.Record)
	}
	if got := fx.total(); got != 1 {
		t.Errorf("schema Run made %d extractor calls, want 1", got)
	}
}

func TestRun_MissingKey(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	_, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{
		Schema: testSchema(t), Render: "static", Provider: "openai",
	})
	if !errors.Is(err, scrape.ErrMissingKey) {
		t.Errorf("err = %v, want ErrMissingKey", err)
	}
}

func TestRun_CostCeiling(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	_, err := scrape.Run(context.Background(), fakeDeps(db, fx, "test-key"), scrapeOrigin(t, scrapeHTML), scrape.Options{
		Schema: testSchema(t), Render: "static", Provider: "openai", MaxCost: 1e-9,
	})
	if !errors.Is(err, crawl.ErrCostCeiling) {
		t.Errorf("err = %v, want ErrCostCeiling", err)
	}
}

func TestRun_CacheHitZeroCalls(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	sch := testSchema(t)
	origin := scrapeOrigin(t, scrapeHTML)
	u, uerr := url.Parse(origin)
	if uerr != nil {
		t.Fatalf("parse origin: %v", uerr)
	}
	domain := strings.ToLower(u.Host)
	doc, merr := json.Marshal(selector.SelectorDoc{
		SchemaHash: selector.SchemaHash(sch), Domain: domain,
		Fields: map[string]selector.FieldSelector{"title": {Type: "css", Expr: "title"}},
	})
	if merr != nil {
		t.Fatalf("marshal doc: %v", merr)
	}
	if err := db.PutSelectors(domain, selector.SchemaHash(sch), string(doc), 3); err != nil {
		t.Fatalf("PutSelectors: %v", err)
	}
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, "test-key"), origin, scrape.Options{
		Schema: sch, Render: "static", Provider: "fake", UseCache: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.FromCache {
		t.Error("seeded cache Run FromCache = false, want true")
	}
	if got := fx.total(); got != 0 {
		t.Errorf("cache-hit Run made %d extractor calls, want 0", got)
	}
	// UseCache: false bypasses the seeded doc.
	fx2 := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	if _, err := scrape.Run(context.Background(), fakeDeps(db, fx2, "test-key"), origin, scrape.Options{
		Schema: sch, Render: "static", Provider: "fake", UseCache: false,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fx2.total(); got != 1 {
		t.Errorf("no-cache Run made %d extractor calls, want 1", got)
	}
}

func TestRun_PageFormatLLM(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{Render: "static", PageFormat: "llm"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Rendered, "## Links") && !strings.Contains(res.Rendered, "# Widget") {
		t.Errorf("llm Rendered missing envelope:\n%s", res.Rendered)
	}
	if res.Markdown == "" {
		t.Error("Markdown empty despite page format")
	}
}

func TestRun_PageFormatJSON(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{Render: "static", PageFormat: "json"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(res.Rendered), &doc); err != nil {
		t.Fatalf("rendered json invalid: %v", err)
	}
	for _, k := range []string{"url", "final_url", "title", "markdown", "structured_data", "content", "metadata", "links", "word_count"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("rendered json missing key %q", k)
		}
	}
}

func TestRun_PageFormatBogus(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	if _, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{Render: "static", PageFormat: "bogus"}); err == nil {
		t.Error("bogus page format: want error")
	}
}

func TestRun_ScopePassthrough(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	shell := `<html><head><title>Shell</title></head><body><nav>nav-text-here</nav><article><h1>Scoped Headline</h1><p>Enough honest prose in the scoped article branch to survive trafilatura extraction cleanly.</p></article></body></html>`
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, shell), scrape.Options{
		Render: "static", Scope: clean.Scope{Include: []string{"article"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Markdown, "Scoped Headline") {
		t.Errorf("scoped markdown lost article:\n%s", res.Markdown)
	}
	if strings.Contains(res.Markdown, "nav-text-here") {
		t.Errorf("scoped markdown kept nav:\n%s", res.Markdown)
	}
}

func TestRun_ProfileCookiesPassthrough(t *testing.T) {
	var ua, ck string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua, ck = r.Header.Get("User-Agent"), r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(scrapeHTML)) //nolint:errcheck // httptest local; short write unactionable
	}))
	t.Cleanup(srv.Close)
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	if _, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), srv.URL, scrape.Options{Render: "static", Profile: "chrome", Cookies: "a=b"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(ua, "Chrome/126") {
		t.Errorf("server saw UA %q, want chrome", ua)
	}
	if ck != "a=b" {
		t.Errorf("server saw Cookie %q, want a=b", ck)
	}
}

func qualityOrigin(t *testing.T, status int, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "quality", file+".html"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = w.Write(raw) //nolint:errcheck // httptest local; short write unactionable
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRun_QualityPassthrough(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	_, err := scrape.Run(context.Background(), fakeDeps(db, fx, "k"), qualityOrigin(t, 403, "challenge-akamai"), scrape.Options{Render: "static"})
	if !errors.Is(err, clean.ErrQuality) {
		t.Fatalf("err = %v, want ErrQuality", err)
	}
	if strings.Contains(err.Error(), "fetch: HTTP") {
		t.Errorf("err %q still carries old non-2xx text", err)
	}
	if !strings.Contains(err.Error(), "access-denied") {
		t.Errorf("err %q lacks issue", err)
	}
}

func TestRun_QualityNoCacheWrite(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	sch := testSchema(t)
	origin := qualityOrigin(t, 403, "challenge-akamai")
	_, err := scrape.Run(context.Background(), fakeDeps(db, fx, "k"), origin, scrape.Options{
		Schema: sch, Render: "static", Provider: "fake", UseCache: true,
	})
	if !errors.Is(err, clean.ErrQuality) {
		t.Fatalf("err = %v, want ErrQuality", err)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("quality failure made %d extractor calls, want 0", got)
	}
	// No selector-cache row for the blocked host.
	u, uerr := url.Parse(origin)
	if uerr != nil {
		t.Fatal(uerr)
	}
	if _, ok, gerr := db.GetSelectors(strings.ToLower(u.Host), selector.SchemaHash(sch)); gerr != nil || ok {
		t.Errorf("GetSelectors = (%v, %v), want not-found", ok, gerr)
	}
}

// TestScrape_LogFetchCounters (Phase D): one markdown-only scrape lands
// exactly one fetch row of the served body's length on the run record.
func TestScrape_LogFetchCounters(t *testing.T) {
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	db := openScrapeDB(t)
	res, err := scrape.Run(context.Background(), fakeDeps(db, fx, ""), scrapeOrigin(t, scrapeHTML), scrape.Options{Render: "static"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := db.GetRun(res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if info.FetchPages != 1 {
		t.Errorf("fetch_pages = %d, want 1", info.FetchPages)
	}
	if info.FetchBytes != int64(len(scrapeHTML)) {
		t.Errorf("fetch_bytes = %d, want %d (deterministic fixture)", info.FetchBytes, len(scrapeHTML))
	}
	if info.FetchMs < 0 {
		t.Errorf("fetch_ms = %d, want ≥0", info.FetchMs)
	}
}

// --- Phase G: raw/screenshot formats + typed challenge surfacing. ---

// fakeGatedFetcher serves canned bodies (or errors) per URL — unlike the
// httptest origins this fakes the FETCH seam so ChallengeError and the
// Proxy field can be injected verbatim.
type fakeGatedFetcher struct {
	bodies map[string][]byte
	errs   map[string]error
	proxy  string
}

func (f *fakeGatedFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	if err, ok := f.errs[req.URL]; ok {
		return nil, err
	}
	b, ok := f.bodies[req.URL]
	if !ok {
		return nil, fmt.Errorf("fetch: no body for %s", req.URL)
	}
	return &fetch.FetchResponse{
		URL: req.URL, FinalURL: req.URL, StatusCode: 200,
		HTML: b, Headers: http.Header{}, Proxy: f.proxy,
	}, nil
}
func (f *fakeGatedFetcher) CanHandle(fetch.FetchRequest) bool { return true }
func (f *fakeGatedFetcher) Close() error                      { return nil }

// TestScrape_Raw: page-format raw returns the decoded body byte-equal —
// not a markdown rendering of it.
func TestScrape_Raw(t *testing.T) {
	raw := "<html><body><p>**not markdown**</p><p>second paragraph of filler for the quality gate to score as prose.</p><p>a third paragraph of honest descriptive words keeps this page rich enough to classify clean.</p></body></html>"
	url := "https://raw.example/page"
	db := openScrapeDB(t)
	d := fakeDeps(db, &fakeExtractor{}, "")
	d.Fetcher = &fakeGatedFetcher{bodies: map[string][]byte{url: []byte(raw)}}
	res, err := scrape.Run(t.Context(), d, url, scrape.Options{PageFormat: "raw", Render: "static"})
	if err != nil {
		t.Fatalf("Run raw: %v", err)
	}
	if res.Rendered != raw {
		t.Errorf("raw content not byte-equal:\n got %q\nwant %q", res.Rendered, raw)
	}
}

// TestScrape_ChallengeRawTyped: a typed challenge never leaks bytes —
// render=static surfaces the ChallengeError with the vendor named.
func TestScrape_ChallengeRawTyped(t *testing.T) {
	url := "https://blocked.example/page"
	db := openScrapeDB(t)
	d := fakeDeps(db, &fakeExtractor{}, "")
	d.Fetcher = &fakeGatedFetcher{errs: map[string]error{url: &fetch.ChallengeError{Vendor: "cloudflare", StatusCode: 403, URL: url}}}
	_, err := scrape.Run(t.Context(), d, url, scrape.Options{PageFormat: "raw", Render: "static"})
	var ce *fetch.ChallengeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *fetch.ChallengeError", err)
	}
	if ce.Vendor != "cloudflare" || !strings.Contains(err.Error(), "cloudflare") {
		t.Errorf("err = %v, want vendor named", err)
	}
}

// TestScrape_ChallengeAutoEscalation: render=auto gets one rod attempt;
// when the browser can't launch in the sandbox the TYPED error stays
// primary (launch noise must never mask the vendor).
func TestScrape_ChallengeAutoEscalation(t *testing.T) {
	url := "https://blocked.example/auto"
	db := openScrapeDB(t)
	d := fakeDeps(db, &fakeExtractor{}, "")
	d.Fetcher = &fakeGatedFetcher{errs: map[string]error{url: &fetch.ChallengeError{Vendor: "datadome", StatusCode: 200, URL: url}}}
	_, err := scrape.Run(t.Context(), d, url, scrape.Options{Render: "auto"})
	var ce *fetch.ChallengeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *fetch.ChallengeError", err)
	}
	if !strings.Contains(err.Error(), "datadome") {
		t.Errorf("err = %v, want vendor as primary message", err)
	}
}

// TestScrape_ChallengeProxySurfaced: the run row records the redacted
// proxy that served the page (run_history.proxy contract).
func TestScrape_ChallengeProxySurfaced(t *testing.T) {
	url := "https://proxied.example/page"
	db := openScrapeDB(t)
	d := fakeDeps(db, &fakeExtractor{}, "")
	d.Fetcher = &fakeGatedFetcher{bodies: map[string][]byte{url: []byte(scrapeHTML)}, proxy: "10.0.0.9:3128"}
	res, err := scrape.Run(t.Context(), d, url, scrape.Options{Render: "static"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := db.GetRun(res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Proxy != "10.0.0.9:3128" {
		t.Errorf("run_history.proxy = %q, want 10.0.0.9:3128", info.Proxy)
	}
}

// TestScrape_ScreenshotValidation: screenshot+static is a pre-I/O
// OptionsError (exit 2); the PNG bytes themselves are browser-tier.
func TestScrape_ScreenshotValidation(t *testing.T) {
	err := scrape.ValidateOptions(scrape.Options{PageFormat: "screenshot", Render: "static"})
	var oe *scrape.OptionsError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want *OptionsError", err)
	}
	for _, want := range []string{"screenshot", "static"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	// auto (default) and browser renders are accepted.
	if err := scrape.ValidateOptions(scrape.Options{PageFormat: "screenshot"}); err != nil {
		t.Errorf("screenshot+auto: %v", err)
	}
	// Run with the conflict fails before any I/O (nil DB must NOT win —
	// validation precedes it for format/render combos via Run order).
	_, err = scrape.Run(t.Context(), scrape.Deps{DB: openScrapeDB(t)}, "https://example.com", scrape.Options{PageFormat: "screenshot", Render: "static"})
	if !errors.As(err, &oe) {
		t.Errorf("Run err = %v, want OptionsError", err)
	}
}

// TestRun_HeadersDeliveredStatic — M0a E2E: run headers ride the static
// fetch all the way to the origin, winning over the profile bundle
// (inline echo — scrape_test can't see fetch_test's helpers).
func TestRun_HeadersDeliveredStatic(t *testing.T) {
	var got http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(scrapeHTML)) //nolint:errcheck // httptest local
	}))
	t.Cleanup(origin.Close)

	db := openScrapeDB(t)
	_, err := scrape.Run(context.Background(), fakeDeps(db, nil, ""), origin.URL, scrape.Options{
		Render:  "static",
		Profile: "chrome",
		Headers: []string{"Authorization: Bearer tok", "Accept-Language: de-DE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", got.Get("Authorization"))
	}
	if got.Get("Accept-Language") != "de-DE" {
		t.Errorf("Accept-Language = %q, want the run header to beat the profile bundle", got.Get("Accept-Language"))
	}
}

// TestRun_ProxyThreadsToStatic — Batch B: Options.Proxy reaches the
// static fetch E2E. A dead loopback proxy makes every fetch fail (the
// option rode the request); the identical run without it succeeds.
// fakeSOCKS5 is fetch-test-internal, and duplicating it is the exact
// fork this suite forbids — the dead-port pin proves the threading both
// ways without a second proxy fake.
func TestRun_ProxyThreadsToStatic(t *testing.T) {
	origin := scrapeOrigin(t, scrapeHTML)
	db := openScrapeDB(t)

	if _, err := scrape.Run(context.Background(), fakeDeps(db, nil, ""), origin, scrape.Options{
		Render: "static", Proxy: "http://127.0.0.1:1",
	}); err == nil {
		t.Error("scrape through a dead per-run proxy must fail (option never reached the fetch layer?)")
	}
	res, err := scrape.Run(context.Background(), fakeDeps(db, nil, ""), origin, scrape.Options{Render: "static"})
	if err != nil {
		t.Fatalf("same run without Proxy: %v", err)
	}
	if res.Title != "Widget" {
		t.Errorf("title = %q, want the origin page", res.Title)
	}
}

// TestValidateOptions_Proxy — Batch B: bad per-run proxies surface
// pre-I/O as OptionsError (ErrProxyConfig wording family); valid forms
// pass.
func TestValidateOptions_Proxy(t *testing.T) {
	for _, bad := range []string{"ftp://p.example:3128", "host:port", "http://"} {
		err := scrape.ValidateOptions(scrape.Options{Proxy: bad})
		var oe *scrape.OptionsError
		if !errors.As(err, &oe) {
			t.Errorf("Proxy %q: err = %v, want *OptionsError", bad, err)
		}
	}
	for _, good := range []string{"http://proxy.example:3128", "socks5://127.0.0.1:9050", "socks5h://gate.example:1080", "127.0.0.1:3128:user:pass"} {
		if err := scrape.ValidateOptions(scrape.Options{Proxy: good}); err != nil {
			t.Errorf("Proxy %q: %v", good, err)
		}
	}
}
