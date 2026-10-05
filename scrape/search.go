package scrape

// Web search (Phase G): six BYOK/no-key SERP backends behind one data
// registry (like fetch.HeaderProfiles — no plugin system), each ~30
// lines + a recorded fixture in testdata/search/. The HTTP client rides
// fetch.GuardedTransport so SSRF + the proxy pool are inherited for free
// (crawl/robots.go precedent). Endpoints are package vars — the test
// override seam; never a production config surface.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/motherlodelab/magpie/fetch"
)

// SearchHit is one SERP result.
type SearchHit struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet,omitempty"`
}

// SearchRecord is one search output record: the hit plus, when
// --scrape-top reached it, the batch page record.
type SearchRecord struct {
	SearchHit
	Page *BatchItem `json:"page,omitempty"`
}

// SearchProvider queries one backend: limit ≤ 0 means the backend
// default; the provider's API key (already resolved) rides the context.
// Implementations return hits in provider order; Search renumbers
// positions 1-based.
type SearchProvider func(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error)

// Endpoints (package vars so tests swap in httptest URLs).
var (
	braveSearchURL   = "https://api.search.brave.com/res/v1/web/search"
	serperSearchURL  = "https://google.serper.dev/search"
	serpapiSearchURL = "https://serpapi.com/search"
	exaSearchURL     = "https://api.exa.ai/search"
	ddgSearchURL     = "https://html.duckduckgo.com/html/"
)

// searchProviders is the registry.
var searchProviders = map[string]SearchProvider{
	"brave":      searchBrave,
	"serper":     searchSerper,
	"serpapi":    searchSerpAPI,
	"searxng":    searchSearXNG,
	"exa":        searchExa,
	"duckduckgo": searchDuckDuckGo,
}

// searchProvidersNeedingKeys: everything except the zero-key pair.
var searchProvidersNeedingKeys = map[string]bool{
	"brave": true, "serper": true, "serpapi": true, "exa": true,
}

// SearchProviderNames returns the valid provider names, sorted.
func SearchProviderNames() []string {
	names := make([]string, 0, len(searchProviders))
	for n := range searchProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SearchProvidersFor returns the search backends usable with keyFor,
// sorted: duckduckgo always, searxng when MAGPIE_SEARXNG_URL is set, each
// keyed backend with a non-empty key (a nil keyFor means no keys). The one
// home for "which backends can run" (research fans a search out over them).
func SearchProvidersFor(keyFor func(string) string) []string {
	var out []string
	for _, p := range SearchProviderNames() {
		switch {
		case p == "searxng":
			if strings.TrimSpace(os.Getenv("MAGPIE_SEARXNG_URL")) == "" {
				continue
			}
		case searchProvidersNeedingKeys[p]:
			if keyFor == nil || keyFor(p) == "" {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// DefaultSearchProvider is the zero-key fallback so `magpie search`
// works with no configuration at all.
const DefaultSearchProvider = "duckduckgo"

// SearchOptions configures one search.
type SearchOptions struct {
	Provider  string // "" = DefaultSearchProvider
	Limit     int    // max hits (<= 0 → 10)
	ScrapeTop int    // scrape the first N hits (0 = off)
}

// searchKeysKey carries resolved provider keys through the frozen
// SearchProvider signature (the registry is static data; keys are
// per-call).
type searchKeysKey struct{}

func searchKey(ctx context.Context, provider string) string {
	m, _ := ctx.Value(searchKeysKey{}).(map[string]string)
	return m[provider]
}

// Search runs one query and optionally scrapes the top hits through the
// normal pipeline. Records come back in position order.
func Search(ctx context.Context, d Deps, query string, o SearchOptions) ([]SearchRecord, error) {
	provider := o.Provider
	if provider == "" {
		provider = DefaultSearchProvider
	}
	fn, ok := searchProviders[provider]
	if !ok {
		return nil, &OptionsError{fmt.Sprintf("scrape: search provider %q unknown (want %s)", provider, strings.Join(SearchProviderNames(), "|"))}
	}
	if o.ScrapeTop < 0 {
		return nil, &OptionsError{fmt.Sprintf("scrape: scrape-top %d must not be negative", o.ScrapeTop)}
	}
	limit := o.Limit
	if limit <= 0 {
		limit = 10
	}
	key := ""
	if d.APIKeyFor != nil {
		key = d.APIKeyFor(provider)
	}
	ctx = context.WithValue(ctx, searchKeysKey{}, map[string]string{provider: key})
	if searchProvidersNeedingKeys[provider] && key == "" {
		return nil, fmt.Errorf("scrape: search provider %s: %w", provider, ErrMissingKey)
	}
	// ponytail: the transport opts into AllowPrivate because every peer
	// of this client is operator-chosen (fixed provider endpoints or
	// MAGPIE_SEARXNG_URL) — a local searxng is the canonical zero-key
	// deployment. Hit URLs from the SERP never dial through this client;
	// scrape-top goes through scrape.Run's strict fetcher.
	client := &http.Client{Transport: fetch.GuardedTransportWithOptions(fetch.SSRFOptions{AllowPrivate: true}), Timeout: 15 * time.Second}
	hits, err := fn(ctx, client, query, limit)
	if err != nil {
		return nil, fmt.Errorf("scrape: search %s: %w", provider, err)
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	records := make([]SearchRecord, len(hits))
	for i, h := range hits {
		h.Position = i + 1
		records[i] = SearchRecord{SearchHit: h}
	}
	if o.ScrapeTop > 0 && len(records) > 0 {
		n := min(o.ScrapeTop, len(records))
		urls := make([]string, n)
		for i := 0; i < n; i++ {
			urls[i] = records[i].URL
		}
		items, err := Batch(ctx, d, urls, BatchOptions{Concurrency: DefaultBatchConcurrency})
		if err != nil {
			return nil, fmt.Errorf("scrape: search scrape-top: %w", err)
		}
		for i := range items {
			records[i].Page = &items[i]
		}
	}
	return records, nil
}

// searchJSON is the shared helper: do the request, decode the body,
// fail with the provider named (never raw response bodies).
func searchJSON(ctx context.Context, req *http.Request, c *http.Client, out any) error {
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body fully read below
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("bad JSON: %w", err)
	}
	return nil
}

// queryKey is one URL query parameter.
type queryKey struct{ k, v string }

// searchRequest builds a request against an endpoint with query params,
// failing loudly on malformed endpoints/URLs (never silently).
func searchRequest(ctx context.Context, method, endpoint string, params ...queryKey) (*http.Request, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint %q: %w", endpoint, err)
	}
	q := u.Query()
	for _, p := range params {
		q.Set(p.k, p.v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	return req, nil
}

// searchPostRequest builds a JSON-body POST request.
func searchPostRequest(ctx context.Context, endpoint string, body map[string]any) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	return req, nil
}

func searchBrave(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error) {
	req, err := searchRequest(ctx, http.MethodGet, braveSearchURL, queryKey{"q", query}, queryKey{"count", fmt.Sprintf("%d", max(limit, 1))})
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", searchKey(ctx, "brave"))
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := searchJSON(ctx, req, c, &out); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		hits = append(hits, SearchHit{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return hits, nil
}

func searchSerper(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error) {
	req, err := searchPostRequest(ctx, serperSearchURL, map[string]any{"q": query, "num": max(limit, 1)})
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", searchKey(ctx, "serper"))
	var out struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := searchJSON(ctx, req, c, &out); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(out.Organic))
	for _, r := range out.Organic {
		hits = append(hits, SearchHit{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return hits, nil
}

func searchSerpAPI(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error) {
	req, err := searchRequest(ctx, http.MethodGet, serpapiSearchURL, queryKey{"engine", "google"}, queryKey{"q", query},
		queryKey{"num", fmt.Sprintf("%d", max(limit, 1))}, queryKey{"api_key", searchKey(ctx, "serpapi")})
	if err != nil {
		return nil, err
	}
	var out struct {
		OrganicResults []struct {
			Title string `json:"title"`
			Link  string `json:"link"`
			Snip  string `json:"snippet"`
		} `json:"organic_results"`
	}
	if err := searchJSON(ctx, req, c, &out); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(out.OrganicResults))
	for _, r := range out.OrganicResults {
		hits = append(hits, SearchHit{Title: r.Title, URL: r.Link, Snippet: r.Snip})
	}
	return hits, nil
}

func searchSearXNG(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("MAGPIE_SEARXNG_URL")), "/")
	if base == "" {
		return nil, fmt.Errorf("MAGPIE_SEARXNG_URL is not set (searxng needs a self-hosted instance; it is the zero-key path)")
	}
	qs := []queryKey{{"q", query}, {"format", "json"}}
	req, err := searchRequest(ctx, http.MethodGet, base+"/search", qs...)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := searchJSON(ctx, req, c, &out); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(out.Results))
	for _, r := range out.Results {
		hits = append(hits, SearchHit{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return hits, nil
}

func searchExa(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error) {
	req, err := searchPostRequest(ctx, exaSearchURL, map[string]any{"query": query, "numResults": max(limit, 1)})
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", searchKey(ctx, "exa"))
	var out struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := searchJSON(ctx, req, c, &out); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(out.Results))
	for _, r := range out.Results {
		hits = append(hits, SearchHit{Title: r.Title, URL: r.URL, Snippet: r.Text})
	}
	return hits, nil
}

// searchDuckDuckGo is the zero-key fallback: the HTML endpoint parsed
// for result__a anchors (uddg redirect links decoded to real URLs).
func searchDuckDuckGo(ctx context.Context, c *http.Client, query string, limit int) ([]SearchHit, error) {
	req, err := searchRequest(ctx, http.MethodGet, ddgSearchURL, queryKey{"q", query})
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36")
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // read below
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	doc.Find("div.result").Each(func(_ int, s *goquery.Selection) {
		if len(hits) >= max(limit, 1) {
			return
		}
		a := s.Find("a.result__a").First()
		if a.Length() == 0 {
			return
		}
		href, ok := a.Attr("href")
		if !ok {
			return
		}
		link := ddgDecodeHref(href)
		if link == "" {
			return
		}
		snippet := strings.TrimSpace(s.Find(".result__snippet").First().Text())
		hits = append(hits, SearchHit{Title: strings.TrimSpace(a.Text()), URL: link, Snippet: snippet})
	})
	return hits, nil
}

// ddgDecodeHref unwraps //duckduckgo.com/l/?uddg=<encoded> redirects.
func ddgDecodeHref(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if u.Host != "" && u.Host != "duckduckgo.com" {
		return href
	}
	if got := u.Query().Get("uddg"); got != "" {
		return got
	}
	return href
}
