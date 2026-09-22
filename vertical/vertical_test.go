package vertical_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/vertical"
)

// fakeVerticalFetcher serves fixture bytes by URL substring; zero network.
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
	// Longest matching substring wins: deterministic when keys overlap
	// (e.g. "old.reddit.com/r/" vs ".json" on a retry URL). A key with a
	// trailing "$" anchors to the URL end instead.
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
	if bestLen >= 0 {
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
	return nil, fmt.Errorf("fake fetcher: unexpected URL %s (order=%v)", req.URL, f.order)
}

func (f *fakeVerticalFetcher) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

func verticalFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vertical", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return u
}

func TestList_ExactNameSet(t *testing.T) {
	want := map[string]bool{
		"github_repo": true, "pypi": true, "npm": true, "crates_io": true,
		"reddit": true, "hackernews": true, "arxiv": true, "youtube": true,
		"shopify_product": true, "ecommerce_product": true,
		// Phase H additions.
		"stackoverflow": true, "trustpilot": true, "dockerhub": true,
		"huggingface": true, "og": true,
		// Upwork jobs (typed challenge surface; meta contract).
		"upwork_job": true,
		// Phase V additions.
		"job_posting": true, "event": true, "local_business": true,
		"article": true, "rss": true,
		// Phase W additions.
		"etsy_listing": true, "woocommerce_product": true,
		"substack_post": true, "dev_to_article": true,
		"ebay_item": true, "amazon_product": true,
	}
	got := map[string]bool{}
	for _, info := range vertical.List() {
		got[info.Name] = true
		if info.Label == "" || info.Desc == "" {
			t.Errorf("extractor %q missing Label/Desc", info.Name)
		}
		if len(info.Patterns) == 0 {
			t.Errorf("extractor %q has no Patterns", info.Name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for n := range want {
		if !got[n] {
			t.Errorf("List() missing %q", n)
		}
	}
}

// TestW_AutoDispatch pins the Phase W dispatch contract: the five new
// host-bound autos return the RIGHT name per URL (registration-order
// surprises are a real failure mode), and the permissive woo shape never
// auto-fires.
func TestW_AutoDispatch(t *testing.T) {
	t.Parallel()
	hits := map[string]string{
		"amazon_product": "https://www.amazon.com/dp/B08N5WRWNW",
		"ebay_item":      "https://www.ebay.com/itm/1234567890",
		"etsy_listing":   "https://www.etsy.com/listing/1283746291/mug",
		"substack_post":  "https://demo.substack.com/p/hello-world",
		"dev_to_article": "https://dev.to/alexdev/go-generics",
	}
	for want, raw := range hits {
		ex, ok := vertical.MatchURL(raw)
		if !ok {
			t.Errorf("MatchURL(%s) missed, want %s", raw, want)
			continue
		}
		if ex.Info.Name != want {
			t.Errorf("MatchURL(%s) = %s, want %s", raw, ex.Info.Name, want)
		}
	}
	// The OptIn woo shape must miss auto-dispatch (shop.example is nobody's host).
	if ex, ok := vertical.MatchURL("https://shop.example/product/x"); ok {
		t.Errorf("MatchURL(woo shape) = %s — must stay OptIn-only", ex.Info.Name)
	}
}

func TestLookup_All(t *testing.T) {
	for _, info := range vertical.List() {
		ex, ok := vertical.Lookup(info.Name)
		if !ok {
			t.Errorf("Lookup(%q) = false", info.Name)
			continue
		}
		if ex.Info.Name != info.Name {
			t.Errorf("Lookup(%q).Name = %q", info.Name, ex.Info.Name)
		}
	}
	if _, ok := vertical.Lookup("tumblr"); ok {
		t.Error("Lookup(tumblr) = true, want false")
	}
}

func TestMatchURL_StrictOnly(t *testing.T) {
	hits := map[string]string{
		"github_repo": "https://github.com/o/r",
		"pypi":        "https://pypi.org/project/demo-pkg/",
		"npm":         "https://www.npmjs.com/package/demo-npm",
		"crates_io":   "https://crates.io/crates/demo-crate",
		"reddit":      "https://www.reddit.com/r/demo/comments/abc/demo/",
		"hackernews":  "https://news.ycombinator.com/item?id=123",
		"arxiv":       "https://arxiv.org/abs/2401.12345v2",
		"youtube":     "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
	}
	for want, raw := range hits {
		ex, ok := vertical.MatchURL(raw)
		if !ok {
			t.Errorf("MatchURL(%s) missed, want %s", raw, want)
			continue
		}
		if ex.Info.Name != want {
			t.Errorf("MatchURL(%s) = %s, want %s", raw, ex.Info.Name, want)
		}
	}
	misses := []string{
		"https://example.com/x",
		"https://shop.example/products/w", // shopify MUST NOT auto-fire
		"https://blog.example/some/post",  // ecommerce MUST NOT auto-fire
		"https://github.com/o",            // owner-only, no repo
		"not-a-url",
	}
	for _, raw := range misses {
		if ex, ok := vertical.MatchURL(raw); ok {
			t.Errorf("MatchURL(%s) = %s, want miss", raw, ex.Info.Name)
		}
	}
}

func TestOptInNeverSteals(t *testing.T) {
	// Permissive OptIn matchers must never auto-fire; strict extractors
	// still win their own URLs (dispatch order: strict first, OptIn skipped).
	for _, raw := range []string{
		"https://blog.example/some/post",
		"https://shop.example/",
		"https://shop.example/products/w", // shopify-shaped but OptIn: still a miss
	} {
		if ex, ok := vertical.MatchURL(raw); ok {
			t.Errorf("MatchURL(%s) auto-fired %s — OptIn must never auto-match", raw, ex.Info.Name)
		}
	}
	// A github URL auto-matches the STRICT github_repo extractor — the
	// guarantee is that the permissives don't shadow it.
	if ex, ok := vertical.MatchURL("https://github.com/o/r"); !ok || ex.Info.Name != "github_repo" {
		t.Errorf("MatchURL(github URL) = (%v, %v), want github_repo strict hit", ex.Info.Name, ok)
	}
	// Explicit opt-in path stays alive.
	shop, ok := vertical.Lookup("shopify_product")
	if !ok {
		t.Fatal("Lookup(shopify_product) = false")
	}
	if !shop.Match(mustURL(t, "https://shop.example/products/w")) {
		t.Error("shopify_product.Match(product URL) = false, want true")
	}
	eco, ok := vertical.Lookup("ecommerce_product")
	if !ok {
		t.Fatal("Lookup(ecommerce_product) = false")
	}
	if !eco.Match(mustURL(t, "https://blog.example/some/post")) {
		t.Error("ecommerce_product.Match(blog URL) = false, want true")
	}
	_, ok = vertical.Lookup("shopify_product")
	if !ok || !shop.OptIn || !eco.OptIn {
		t.Error("shopify_product/ecommerce_product must be OptIn")
	}
}

func TestFetchJSON_Vectors(t *testing.T) {
	ctx := context.Background()
	t.Run("ok", func(t *testing.T) {
		fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
			"pypi.org": {body: verticalFixture(t, "pypi.json")},
		}}
		ex, _ := vertical.Lookup("pypi")
		got, err := ex.Extract(ctx, fx, mustURL(t, "https://pypi.org/project/demo-pkg/"))
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if got["name"] != "demo-pkg" {
			t.Errorf("name = %v", got["name"])
		}
	})
	t.Run("non2xx", func(t *testing.T) {
		fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
			"pypi.org": {status: 500, body: []byte("boom")},
		}}
		ex, _ := vertical.Lookup("pypi")
		_, err := ex.Extract(ctx, fx, mustURL(t, "https://pypi.org/project/demo-pkg/"))
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err = %v, want HTTP 500", err)
		}
	})
	t.Run("badJSON", func(t *testing.T) {
		fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
			"pypi.org": {body: []byte("{nope")},
		}}
		ex, _ := vertical.Lookup("pypi")
		if _, err := ex.Extract(ctx, fx, mustURL(t, "https://pypi.org/project/demo-pkg/")); err == nil {
			t.Error("bad JSON: want error")
		}
	})
}

// TestRegister covers the embedder seam (placed after the exact-set
// tests above: registry appends are global, so those must see the
// unpolluted built-in list; only the UA test below follows and it never
// touches the registry).
func TestRegister(t *testing.T) {
	fake := vertical.Extractor{
		Info:  vertical.Info{Name: "zz_test_fake", Label: "Fake", Desc: "test-only", Patterns: []string{"https://fake.test/x"}},
		Match: func(u *url.URL) bool { return u.Host == "fake.test" },
		Extract: func(_ context.Context, _ vertical.Fetcher, _ *url.URL) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		},
	}
	if err := vertical.Register(fake); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := vertical.Lookup("zz_test_fake"); !ok {
		t.Fatal("Lookup(zz_test_fake) = false, want true")
	}
	found := false
	for _, info := range vertical.List() {
		if info.Name == "zz_test_fake" {
			found = true
		}
	}
	if !found {
		t.Error("List() missing registered extractor")
	}
	if err := vertical.Register(fake); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate Register err = %v, want already-registered", err)
	}
	for name, mutate := range map[string]func(*vertical.Extractor){
		"empty name":  func(e *vertical.Extractor) { e.Info.Name = "" },
		"nil Match":   func(e *vertical.Extractor) { e.Match = nil },
		"nil Extract": func(e *vertical.Extractor) { e.Extract = nil },
	} {
		bad := fake
		bad.Info.Name = "zz_test_bad"
		mutate(&bad)
		if err := vertical.Register(bad); err == nil {
			t.Errorf("Register(%s) = nil error, want validation failure", name)
		}
	}
	// A registered OptIn extractor stays explicit-only: a permissive
	// matcher must never make MatchURL fire it.
	adopt := fake
	adopt.Info.Name = "zz_test_optin"
	adopt.OptIn = true
	adopt.Match = func(u *url.URL) bool { return true } // permissive on purpose
	if err := vertical.Register(adopt); err != nil {
		t.Fatalf("Register(optin): %v", err)
	}
	if ex, ok := vertical.MatchURL("https://example.com/anything"); ok {
		t.Errorf("MatchURL fired %s — registered OptIn must stay explicit-only", ex.Info.Name)
	}
}

// TestStaticFetcher_UserAgent is the ONE real-client test: a recording
// httptest server (localhost, allowed) asserts the StaticFetcher sends the
// User-Agent that crates.io policy requires. UA value from fetch/http.go:25.
func TestStaticFetcher_UserAgent(t *testing.T) {
	var observed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(verticalFixture(t, "crates.json")) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)
	sf, err := fetch.NewStaticFetcher()
	if err != nil {
		t.Fatalf("NewStaticFetcher: %v", err)
	}
	resp, err := sf.Fetch(context.Background(), fetch.FetchRequest{URL: srv.URL + "/x"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if observed != "github.com/motherlodelab/magpie/1.0 (+https://github.com/you/magpie)" {
		t.Errorf("User-Agent = %q, want magpie UA", observed)
	}
}

// extractInline serves an inline JSON-LD literal through the fake
// fetcher; shared by the events/business/article variant tables.
func extractInline(t *testing.T, ex vertical.Extractor, host, jsonld string) map[string]any {
	t.Helper()
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		host: {body: []byte(`<html><head><script type="application/ld+json">` + jsonld + `</script></head></html>`)},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://"+host+"/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return rec
}
