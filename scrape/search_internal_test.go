package scrape

// Search provider tests (Phase G G.5). Package scrape (not scrape_test):
// the endpoint package-vars are the override seam — exporting them for
// tests would widen the production API. Fixtures live in
// testdata/search/ (plan deliverables); their "// recorded" header
// comment lines are stripped before serving.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/store"

	"github.com/zalando/go-keyring"
)

// The external scrape_test helpers are not visible from this internal
// package — the scrape-top e2e needs exactly two locals: a temp DB and a
// rich-enough static page.

func localSearchDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

const localSearchPage = `<html><head><title>hit page</title></head><body>` +
	`<p>Plenty of honest descriptive prose keeps the static renderer in charge without any browser escalation at all.</p>` +
	`<p>A second paragraph of harmless filler text pushes visible length safely past the two-hundred character threshold.</p>` +
	`</body></html>`

// fixtureBody reads a testdata/search fixture and strips the dated
// "//" header comment lines (JSON has no comments; provenance stays).
func fixtureBody(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "search", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		lines = append(lines, ln)
	}
	return []byte(strings.Join(lines, "\n"))
}

func wantHeader(r *http.Request, key, want string) error {
	if got := r.Header.Get(key); got != want {
		return fmt.Errorf("header %s = %q, want %q", key, got, want)
	}
	return nil
}

func hitsOf(t *testing.T, records []SearchRecord) []SearchHit {
	t.Helper()
	out := make([]SearchHit, len(records))
	for i, r := range records {
		out[i] = r.SearchHit
	}
	return out
}

func TestSearchBrave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/res/v1/web/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		q := r.URL.Query()
		if q.Get("q") != "golang scraping" || q.Get("count") != "5" {
			t.Errorf("query = %v, want q + count=5", q)
		}
		if err := wantHeader(r, "X-Subscription-Token", "brave-key"); err != nil {
			t.Error(err)
		}
		_, _ = w.Write(fixtureBody(t, "brave.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := braveSearchURL
	braveSearchURL = srv.URL + "/res/v1/web/search"
	defer func() { braveSearchURL = old }()

	records, err := Search(t.Context(), Deps{APIKeyFor: func(string) string { return "brave-key" }},
		"golang scraping", SearchOptions{Provider: "brave", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	got := hitsOf(t, records)
	want := []SearchHit{
		{Position: 1, Title: "Go Web Scraping Guide", URL: "https://example.com/guide", Snippet: "The complete guide to scraping with Go."},
		{Position: 2, Title: "Scraping at scale", URL: "https://example.org/scale", Snippet: "Techniques for large crawls."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hits = %+v, want %+v", got, want)
	}
}

// QA §3 (K4): Brave's web search answers 422 for count > 20 (its documented
// maximum), and the desktop offers 25/50 — clamp to 1..20.
func TestSearchBrave_CountClamp(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.Query().Get("count"))
		_, _ = w.Write(fixtureBody(t, "brave.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := braveSearchURL
	braveSearchURL = srv.URL + "/res/v1/web/search"
	defer func() { braveSearchURL = old }()

	for _, tc := range []struct {
		limit int
		want  string
	}{{50, "20"}, {20, "20"}, {5, "5"}, {0, "1"}} {
		if _, err := searchBrave(t.Context(), srv.Client(), "q", tc.limit); err != nil {
			t.Fatalf("limit %d: %v", tc.limit, err)
		}
		if c, _ := got.Load().(string); c != tc.want {
			t.Errorf("limit %d → count=%s, want %s", tc.limit, c, tc.want)
		}
	}
}

func TestSearchSerper(t *testing.T) {
	serperSearchURL = "https://serper.test/search"
	defer func() { serperSearchURL = "https://google.serper.dev/search" }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := wantHeader(r, "X-API-KEY", "serper-key"); err != nil {
			t.Error(err)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if body["q"] != "golang scraping" || body["num"] != float64(10) {
			t.Errorf("body = %v, want q + num", body)
		}
		_, _ = w.Write(fixtureBody(t, "serper.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := serperSearchURL
	serperSearchURL = srv.URL
	defer func() { serperSearchURL = old }()

	records, err := Search(t.Context(), Deps{APIKeyFor: func(string) string { return "serper-key" }},
		"golang scraping", SearchOptions{Provider: "serper"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Title != "Serper result one" || records[1].URL != "https://example.net/two" {
		t.Errorf("hits = %+v", records)
	}
	if records[0].Position != 1 || records[1].Position != 2 {
		t.Errorf("positions = %d/%d, want 1/2", records[0].Position, records[1].Position)
	}
}

func TestSearchSerpAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("engine") != "google" || q.Get("q") != "golang scraping" || q.Get("num") != "3" || q.Get("api_key") != "serpapi-key" {
			t.Errorf("query = %v", q)
		}
		_, _ = w.Write(fixtureBody(t, "serpapi.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := serpapiSearchURL
	serpapiSearchURL = srv.URL
	defer func() { serpapiSearchURL = old }()

	records, err := Search(t.Context(), Deps{APIKeyFor: func(string) string { return "serpapi-key" }},
		"golang scraping", SearchOptions{Provider: "serpapi", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Title != "SerpAPI top hit" || records[0].URL != "https://example.com/top" {
		t.Errorf("hits = %+v", records)
	}
}

func TestSearchSearXNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %s, want /search", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("format") != "json" || q.Get("q") != "golang scraping" {
			t.Errorf("query = %v", q)
		}
		_, _ = w.Write(fixtureBody(t, "searxng.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MAGPIE_SEARXNG_URL", srv.URL)
	t.Setenv("MAGPIE_BRAVE_API_KEY", "")
	t.Setenv("MAGPIE_SERPER_API_KEY", "")
	t.Setenv("MAGPIE_SERPAPI_API_KEY", "")
	t.Setenv("MAGPIE_EXA_API_KEY", "")
	t.Setenv("MAGPIE_API_KEY", "")

	// Zero-key path: no APIKeyFor configured at all.
	records, err := Search(t.Context(), Deps{}, "golang scraping", SearchOptions{Provider: "searxng"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Title != "SearXNG self-hosting" {
		t.Errorf("hits = %+v", records)
	}
}

func TestSearchSearXNGEmptyAndNoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixtureBody(t, "searxng_empty.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MAGPIE_SEARXNG_URL", srv.URL)
	records, err := Search(t.Context(), Deps{}, "nothing here", SearchOptions{Provider: "searxng"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("zero-result fixture gave %d records, want 0", len(records))
	}

	t.Setenv("MAGPIE_SEARXNG_URL", "")
	_, err = Search(t.Context(), Deps{}, "q", SearchOptions{Provider: "searxng"})
	if err == nil || !strings.Contains(err.Error(), "MAGPIE_SEARXNG_URL") {
		t.Errorf("err = %v, want the typed unset-URL error", err)
	}
}

func TestSearchExa(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := wantHeader(r, "X-Api-Key", "exa-key"); err != nil {
			t.Error(err)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if body["query"] != "golang scraping" || body["numResults"] != float64(2) {
			t.Errorf("body = %v", body)
		}
		_, _ = w.Write(fixtureBody(t, "exa.json")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := exaSearchURL
	exaSearchURL = srv.URL
	defer func() { exaSearchURL = old }()

	records, err := Search(t.Context(), Deps{APIKeyFor: func(string) string { return "exa-key" }},
		"golang scraping", SearchOptions{Provider: "exa", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Title != "Exa neural search" {
		t.Errorf("hits = %+v", records)
	}
}

func TestSearchDuckDuckGo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/html/" {
			t.Errorf("path = %s, want /html/", r.URL.Path)
		}
		if r.URL.Query().Get("q") == "" {
			t.Error("missing q")
		}
		_, _ = w.Write(fixtureBody(t, "duckduckgo.html")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := ddgSearchURL
	ddgSearchURL = srv.URL + "/html/"
	defer func() { ddgSearchURL = old }()

	// Zero-key: empty Deps, no key resolution at all.
	records, err := Search(t.Context(), Deps{}, "golang scraping", SearchOptions{Provider: "duckduckgo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("hits = %d, want 2", len(records))
	}
	if records[0].Title != "DDG First Hit" || records[0].URL != "https://example.com/ddg1" {
		t.Errorf("hit0 = %+v, want the uddg-decoded link", records[0])
	}
	if records[1].URL != "https://direct.example.org/page" {
		t.Errorf("hit1 URL = %s, want direct link passthrough", records[1].URL)
	}
	for _, r := range records { // QA W7: the fixture's first result is a y.js ad
		if strings.Contains(r.URL, "/y.js") {
			t.Errorf("ad listed as a hit: %+v", r)
		}
	}

	// Zero-result variant.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixtureBody(t, "duckduckgo_empty.html")) //nolint:errcheck // test server
	}))
	t.Cleanup(empty.Close)
	ddgSearchURL = empty.URL + "/html/"
	records, err = Search(t.Context(), Deps{}, "nothing", SearchOptions{Provider: "duckduckgo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("empty SERP gave %d records, want 0", len(records))
	}
}

func TestSearchMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>")) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	old := braveSearchURL
	braveSearchURL = srv.URL
	defer func() { braveSearchURL = old }()

	_, err := Search(t.Context(), Deps{APIKeyFor: func(string) string { return "k" }}, "q", SearchOptions{Provider: "brave"})
	if err == nil || !strings.Contains(err.Error(), "brave") {
		t.Errorf("err = %v, want the provider named", err)
	}
}

func TestSearchValidation(t *testing.T) {
	// Unknown provider names the valid set (exit 2 via OptionsError).
	_, err := Search(t.Context(), Deps{}, "q", SearchOptions{Provider: "altavista"})
	var oe *OptionsError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want *OptionsError", err)
	}
	for _, p := range SearchProviderNames() {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("err %q must name valid provider %q", err, p)
		}
	}
	// Negative scrape-top fails pre-I/O.
	_, err = Search(t.Context(), Deps{}, "q", SearchOptions{Provider: "duckduckgo", ScrapeTop: -1})
	if !errors.As(err, &oe) {
		t.Errorf("scrape-top -1: err = %v, want *OptionsError", err)
	}
	// Missing key on a keyed provider → ErrMissingKey (CLI exit 7 via keyHint).
	_, err = Search(t.Context(), Deps{APIKeyFor: func(string) string { return "" }}, "q", SearchOptions{Provider: "brave"})
	if !errors.Is(err, ErrMissingKey) {
		t.Errorf("err = %v, want ErrMissingKey", err)
	}
	// Default provider is the zero-key one.
	if DefaultSearchProvider != "duckduckgo" {
		t.Errorf("default = %q, want duckduckgo (works with no configuration)", DefaultSearchProvider)
	}
	// All six backends registered.
	if got := len(searchProviders); got != 6 {
		t.Errorf("registry = %d providers, want 6", got)
	}
}

// TestSearchScrapeTop: fixture SERP whose hit URLs point at local
// origins; scrape-top 2 embeds page records for the first two hits
// only, in position order; a failing scrape never drops the SERP record.
func TestSearchScrapeTop(t *testing.T) {
	var hits atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(localSearchPage)) //nolint:errcheck // test server
	}))
	t.Cleanup(origin.Close)
	var deadHits atomic.Int64
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deadHits.Add(1)
		// Rich 500 body: auto-render stays static (the tiny "gone" body
		// would trigger rod escalation and mask the status), and Classify
		// marks it IssueUnavailable — the batch item fails as data.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(localSearchPage)) //nolint:errcheck // test server
	}))
	t.Cleanup(dead.Close)

	hitURLs := []string{origin.URL + "/1", dead.URL + "/2", origin.URL + "/3", origin.URL + "/4", origin.URL + "/5"}
	var wantURLs []map[string]any
	for i, u := range hitURLs {
		wantURLs = append(wantURLs, map[string]any{"title": fmt.Sprintf("hit%d", i), "url": u, "content": "snippet"})
	}
	payload, err := json.Marshal(map[string]any{"results": wantURLs})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MAGPIE_SEARXNG_URL", srv.URL)

	d := Deps{DB: localSearchDB(t)}
	records, err := Search(t.Context(), d, "q", SearchOptions{Provider: "searxng", ScrapeTop: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 5 {
		t.Fatalf("records = %d, want 5 (SERP intact)", len(records))
	}
	for i, r := range records {
		if r.Position != i+1 {
			t.Errorf("record %d position = %d", i, r.Position)
		}
	}
	if records[0].Page == nil || !records[0].Page.OK {
		t.Error("record 0 must embed a successful page record")
	}
	if records[1].Page == nil || records[1].Page.OK {
		t.Error("record 1 must embed a failing page record (error is data)")
	}
	for i := 2; i < 5; i++ {
		if records[i].Page != nil {
			t.Errorf("record %d embeds a page record, want none (beyond scrape-top)", i)
		}
	}
	// With scrape-top 0: no page records at all.
	records, err = Search(t.Context(), d, "q", SearchOptions{Provider: "searxng"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Page != nil {
			t.Fatal("page record present without scrape-top")
		}
	}
}

func TestSearchProvidersFor(t *testing.T) {
	keys := func(m map[string]string) func(string) string { return func(p string) string { return m[p] } }
	for _, tc := range []struct {
		name   string
		searx  string
		keyFor func(string) string
		want   []string
	}{
		{"no keys, no env", "", keys(nil), []string{"duckduckgo"}},
		{"nil keyFor", "", nil, []string{"duckduckgo"}},
		{"searxng env", "http://127.0.0.1:8888", keys(nil), []string{"duckduckgo", "searxng"}},
		{"blank env", "  ", keys(nil), []string{"duckduckgo"}},
		{"brave key", "", keys(map[string]string{"brave": "k"}), []string{"brave", "duckduckgo"}},
		{"empty exa key", "", keys(map[string]string{"exa": ""}), []string{"duckduckgo"}},
	} {
		t.Setenv("MAGPIE_SEARXNG_URL", tc.searx)
		if got := SearchProvidersFor(tc.keyFor); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: SearchProvidersFor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// secretSink stands in for every search vendor (the URL seams below) and
// counts requests carrying secret in the query or any header — the wire,
// not the code path. own counts a vendor's own key, the vacuity leg.
func secretSink(t *testing.T, secret, own string) (leaks, owns *atomic.Int32) {
	t.Helper()
	leaks, owns = new(atomic.Int32), new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen := r.URL.RawQuery + fmt.Sprint(r.Header)
		if strings.Contains(seen, secret) {
			leaks.Add(1)
		}
		if strings.Contains(seen, own) {
			owns.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`)) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)
	for _, p := range []*string{&braveSearchURL, &serperSearchURL, &serpapiSearchURL, &exaSearchURL} {
		old := *p
		*p = srv.URL
		t.Cleanup(func() { *p = old })
	}
	return leaks, owns
}

// blankShellKeys: this shell exports real provider keys — blank every
// specific variable before anything resolves, or a real key reads through
// (or leaks to a loopback vendor).
func blankShellKeys(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	for _, v := range []string{"OPENAI", "ANTHROPIC", "OPENROUTER", "OPENCODE_GO", "OPENCODE_ZEN", "BRAVE", "SERPER", "SERPAPI", "EXA"} {
		t.Setenv("MAGPIE_"+v+"_API_KEY", "")
	}
	t.Setenv("MAGPIE_API_KEY", "")
}

// TestSearch_GenericKeyNeverSent (QA ST9 + S2b N1): the generic
// MAGPIE_API_KEY and the --api-key flag are LLM keys. No search vendor ever
// receives them — not as a header (brave, serper, exa), not in a query
// string (serpapi). Vacuity: the same resolver still hands the generic key
// to an LLM provider, and a vendor's own variable still reaches the vendor,
// so the zero leaks aren't a dead server.
func TestSearch_GenericKeyNeverSent(t *testing.T) {
	blankShellKeys(t)
	const secret, own = "generic-SECRET-key", "brave-own-key"
	leaks, owns := secretSink(t, secret, own)

	legs := map[string]func(t *testing.T) config.Config{
		"generic env": func(t *testing.T) config.Config {
			t.Setenv("MAGPIE_API_KEY", secret)
			return config.DefaultConfig()
		},
		"api-key flag": func(_ *testing.T) config.Config { // decision E2
			c := config.DefaultConfig()
			c.APIKeyFlag = secret
			return c
		},
	}
	for name, leg := range legs {
		t.Run(name, func(t *testing.T) {
			cfg := leg(t)
			for _, engine := range []string{"brave", "serper", "serpapi", "exa"} {
				_, err := Search(context.Background(), Deps{APIKeyFor: cfg.APIKey}, "widgets", SearchOptions{Provider: engine})
				if !errors.Is(err, ErrMissingKey) {
					t.Errorf("%s: err = %v, want ErrMissingKey (no key of its own)", engine, err)
				}
			}
			if got := cfg.APIKey("openai"); got != secret {
				t.Errorf("APIKey(openai) = %q, want the generic key: LLM providers keep the fallback", got)
			}
		})
	}
	if n := leaks.Load(); n != 0 {
		t.Fatalf("the generic key reached a search vendor %d time(s)", n)
	}

	// Vacuity: the sink is live and reads keys — a vendor's own variable arrives.
	t.Setenv("MAGPIE_BRAVE_API_KEY", own)
	if _, err := Search(context.Background(), Deps{APIKeyFor: config.DefaultConfig().APIKey}, "widgets", SearchOptions{Provider: "brave"}); errors.Is(err, ErrMissingKey) {
		t.Fatalf("brave with its own key: %v", err)
	}
	if owns.Load() == 0 {
		t.Fatal("brave's own key never reached the sink — the leak count above proves nothing")
	}
}

// TestSearch_KeyNeverInError (S2b N2): a transport error never prints the
// request URL — SerpAPI carries its key in the query string.
func TestSearch_KeyNeverInError(t *testing.T) {
	blankShellKeys(t)
	t.Setenv("MAGPIE_SERPAPI_API_KEY", "serpapi-SECRET-key")
	old := serpapiSearchURL
	serpapiSearchURL = "http://127.0.0.1:1/search"
	t.Cleanup(func() { serpapiSearchURL = old })
	_, err := Search(context.Background(), Deps{APIKeyFor: config.DefaultConfig().APIKey}, "q", SearchOptions{Provider: "serpapi"})
	if err == nil {
		t.Fatal("err = nil, want the refused connection") // vacuity: the key was sent toward a dead port
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the key: %v", err)
	}
}
