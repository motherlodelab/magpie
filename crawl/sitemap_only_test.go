package crawl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/store"
)

// sitemapOnlyOrigin is newSiteOrigin plus a robots-declared sitemap whose
// listing is built from baseURL (known only after the server starts).
// Pages link /off so a broken link-following guard shows up as /off hits.
type sitemapOnlyOrigin struct {
	srv     *httptest.Server
	baseURL string
	pages   map[string]string
	listing []string // sitemap-listed paths
	mu      sync.Mutex
	hits    map[string]int
}

func (o *sitemapOnlyOrigin) count(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[path]
}

func newSitemapOnlyOrigin(t *testing.T, robots string, pages map[string]string, listing ...string) *sitemapOnlyOrigin {
	t.Helper()
	o := &sitemapOnlyOrigin{pages: pages, listing: listing, hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.hits[r.URL.Path]++
		o.mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte(robots)) //nolint:errcheck // httptest local
		case "/sitemap.xml":
			abs := make([]string, len(o.listing))
			for i, p := range o.listing {
				abs[i] = o.baseURL + p
			}
			_, _ = w.Write([]byte(urlsetOf(abs...))) //nolint:errcheck // httptest local
		default:
			if html, ok := o.pages[r.URL.Path]; ok {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(html)) //nolint:errcheck // httptest local
				return
			}
			http.NotFound(w, r)
		}
	})
	o.srv = httptest.NewServer(mux)
	o.baseURL = o.srv.URL
	t.Cleanup(o.srv.Close)
	return o
}

const allowAllRobots = "User-agent: *\nDisallow:\nSitemap: /sitemap.xml\n"

func sitemapOnlyRun(t *testing.T, o *sitemapOnlyOrigin, mutate func(*Options)) (Result, error) {
	t.Helper()
	db := openCrawlDB(t)
	fx := &fakeExtractor{script: map[string]map[string]any{"default": crawlTruth}}
	opts := Options{
		SeedURL: o.srv.URL + "/", Schema: mustTestSchema(t),
		MaxPages: 10, MaxDepth: 0, SameHost: true,
		FetchWorkers: 2, Rate: 1000, Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		DB: db, Extractor: fx, SitemapOnly: true,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return Run(context.Background(), opts)
}

// TestSitemapOnly_FrontierExact — US-4: the frontier is exactly the
// sitemap URLs; the (unlisted) seed and the off-sitemap link target are
// never fetched.
func TestSitemapOnly_FrontierExact(t *testing.T) {
	o := newSitemapOnlyOrigin(t, allowAllRobots,
		map[string]string{"/a": itemPage("/off"), "/b": itemPage("/off"), "/c": itemPage("/off")},
		"/a", "/b", "/c")
	res, err := sitemapOnlyRun(t, o, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PagesOK != 3 {
		t.Errorf("pages_ok = %d, want 3", res.PagesOK)
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		if n := o.count(p); n != 1 {
			t.Errorf("%s fetches = %d, want exactly 1", p, n)
		}
	}
	if n := o.count("/"); n != 0 {
		t.Errorf("seed / fetched %d times; sitemap-only must not enqueue the unlisted seed", n)
	}
	if n := o.count("/off"); n != 0 {
		t.Errorf("/off fetched %d times; link-following must be off in sitemap-only mode", n)
	}
}

// TestSitemapOnly_NoInScopeURLs — a sitemap whose URLs are all
// scope-filtered away leaves an empty frontier: loud typed failure,
// never a 0-page success.
func TestSitemapOnly_NoInScopeURLs(t *testing.T) {
	o := newSitemapOnlyOrigin(t, allowAllRobots,
		map[string]string{"/x": itemPage(), "/y": itemPage()},
		"/x", "/y")
	_, err := sitemapOnlyRun(t, o, func(o *Options) { o.PathPrefix = "/docs" })
	if !errors.Is(err, ErrSitemapOnlyEmpty) {
		t.Fatalf("err = %v, want ErrSitemapOnlyEmpty (errors.Is)", err)
	}
}

// TestSitemapOnly_ExpansionErrorFatal — in sitemap-only mode the
// expansion IS the frontier, so an expansion error is fatal (no
// seed-only fallback, no warn-and-proceed).
func TestSitemapOnly_ExpansionErrorFatal(t *testing.T) {
	// robots advertises /missing.xml which 404s.
	o := newSitemapOnlyOrigin(t, "User-agent: *\nDisallow:\nSitemap: /missing.xml\n",
		map[string]string{"/": itemPage()})
	var db *store.DB
	_, err := sitemapOnlyRun(t, o, func(opts *Options) { db, opts.RunID = opts.DB, "fatal" })
	if err == nil {
		t.Fatal("expansion error must be fatal in sitemap-only mode")
	}
	if errors.Is(err, ErrSitemapOnlyEmpty) {
		t.Errorf("err = %v, want the underlying expansion error (not the empty sentinel)", err)
	}
	// QA B3: a fatal error after the row opened must never leave it `running`.
	if r, gerr := db.GetRun("fatal"); gerr != nil || r.Status != "error" {
		t.Errorf("run row = %q (%v), want status error", r.Status, gerr)
	}
}

// TestRun_CanceledBeforeSeed — QA B3: a Stop that lands before the seed's
// robots fetch is an interruption, never `robots_blocked`, never `running`.
func TestRun_CanceledBeforeSeed(t *testing.T) {
	o := newSitemapOnlyOrigin(t, allowAllRobots, map[string]string{"/": itemPage()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := openCrawlDB(t)
	_, err := Run(ctx, Options{
		SeedURL: o.srv.URL + "/", Schema: mustTestSchema(t), RunID: "stopped",
		Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"), DB: db,
		Extractor: &fakeExtractor{script: map[string]map[string]any{"default": crawlTruth}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if r, gerr := db.GetRun("stopped"); gerr != nil || r.Status != "interrupted" {
		t.Errorf("run row = %q (%v), want status interrupted", r.Status, gerr)
	}
}

// TestSitemapOnly_WarnProceedIntact — the same expansion error WITHOUT
// sitemap-only still warns and proceeds seed-only (existing behavior).
func TestSitemapOnly_WarnProceedIntact(t *testing.T) {
	o := newSitemapOnlyOrigin(t, "User-agent: *\nDisallow:\nSitemap: /missing.xml\n",
		map[string]string{"/onlypage": itemPage()})
	var res Result
	var err error
	stderr := captureStderr(t, func() {
		res, err = sitemapOnlyRun(t, o, func(opts *Options) {
			opts.SitemapOnly = false
			opts.SeedURL = o.srv.URL + "/onlypage"
		})
	})
	if err != nil {
		t.Fatalf("Run: %v (expansion errors must warn, not abort, without sitemap-only)", err)
	}
	if res.PagesOK != 1 {
		t.Errorf("pages_ok = %d, want 1 (seed-only)", res.PagesOK)
	}
	if !strings.Contains(stderr, "continuing seed-only") {
		t.Errorf("stderr %q missing 'continuing seed-only' warning", stderr)
	}
}

// TestValidateSitemapOnly — the contradiction predicate (both edges call it).
func TestValidateSitemapOnly(t *testing.T) {
	for _, tt := range []struct {
		only, no bool
		wantErr  bool
	}{
		{false, false, false},
		{true, false, false},
		{false, true, false},
		{true, true, true},
	} {
		err := ValidateSitemapOnly(tt.only, tt.no)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateSitemapOnly(%v,%v) = %v, wantErr %v", tt.only, tt.no, err, tt.wantErr)
		}
	}
}
