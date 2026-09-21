package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultCrawlSchema is used when crawl_site gets no schema: crawl.Run
// needs a non-nil *extract.Schema, and records flow to the writer
// unvalidated, so a permissive object schema is the honest default.
var defaultCrawlSchema = []byte(`{"type":"object","properties":{"title":{"type":"string"}}}`)

// --- scrape_url ---

// ScrapeIn is the scrape_url input.
type ScrapeIn struct {
	URL             string     `json:"url" jsonschema:"absolute http(s) URL to scrape (file:// works only when MAGPIE_ALLOW_FILE=1; private/loopback hosts are rejected)"`
	Schema          FlexMap    `json:"schema,omitempty" jsonschema:"JSON Schema object; omit for cleaned markdown only"`
	Render          string     `json:"render,omitempty" jsonschema:"auto, static, or browser"`
	UseCache        *FlexBool  `json:"use_cache,omitempty" jsonschema:"apply cached selectors when available"`
	PageFormat      string     `json:"page_format,omitempty" jsonschema:"page output format: markdown, llm, text, json, html, raw, or screenshot"`
	Include         StringList `json:"include,omitempty" jsonschema:"CSS selectors: scrape only matching subtrees"`
	Exclude         StringList `json:"exclude,omitempty" jsonschema:"CSS selectors: drop matching nodes"`
	OnlyMainContent *FlexBool  `json:"only_main_content,omitempty" jsonschema:"main-content only"`
	Profile         string     `json:"profile,omitempty" jsonschema:"header profile: default, chrome, firefox, safari, edge, ios, or chrome_android"`
	Browser         string     `json:"browser,omitempty" jsonschema:"TLS-impersonating browser fingerprint: chrome, firefox, safari, edge, ios, chrome_android, or random"`
	Cookies         string     `json:"cookies,omitempty" jsonschema:"raw Cookie header value"`
	Actions         StringList `json:"actions,omitempty" jsonschema:"browser action lines (rod), one per element: click <sel> | type <sel> <text> | scroll <n|top|bottom> | wait <ms> | wait-for <sel> | eval-js <expr>; forces browser rendering; screenshot is CLI-only"`
	Lang            string     `json:"lang,omitempty" jsonschema:"Accept-Language header value, e.g. fr-CA,fr;q=0.9 (no control characters)"`
	Headers         StringList `json:"headers,omitempty" jsonschema:"raw request headers, repeatable: Name: value (no control characters; wins over profile defaults)"`
	CaptureXHR      StringList `json:"capture_xhr,omitempty" jsonschema:"Go regexps: capture matching XHR/fetch response bodies (browser rendering only)"`
	CDP             string     `json:"cdp_url,omitempty" jsonschema:"remote browser CDP endpoint (ws://, wss://, http(s)://); overrides MAGPIE_CDP_URL"`
}

// ScrapeOut is the scrape_url output.
type ScrapeOut struct {
	URL       string             `json:"url" jsonschema:"requested URL"`
	FinalURL  string             `json:"final_url" jsonschema:"final URL after redirects"`
	Title     string             `json:"title" jsonschema:"page title"`
	Markdown  string             `json:"markdown,omitempty" jsonschema:"cleaned markdown (no schema)"`
	Content   string             `json:"content,omitempty" jsonschema:"rendered page content in page_format (no schema)"`
	Extracted map[string]any     `json:"extracted,omitempty" jsonschema:"extracted record (with schema)"`
	FromCache bool               `json:"from_cache" jsonschema:"served from selector cache with zero LLM calls"`
	Usage     map[string]any     `json:"usage,omitempty" jsonschema:"LLM usage when the extractor ran"`
	XHR       []fetch.XHRCapture `json:"xhr,omitempty" jsonschema:"captured XHR/fetch response bodies (with capture_xhr)"`
}

func handleScrape(d Deps) func(context.Context, *sdk.CallToolRequest, ScrapeIn) (*sdk.CallToolResult, ScrapeOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ScrapeIn) (*sdk.CallToolResult, ScrapeOut, error) {
		// The screenshot action verb never crosses the MCP boundary: an
		// action line would hand an agent a server-side file-write to an
		// arbitrary path. page_format screenshot stays base64-through-
		// envelope. Pre-flight, before any browser launch.
		for i, line := range in.Actions {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if verb, _, _ := strings.Cut(trimmed, " "); verb == "screenshot" {
				return nil, ScrapeOut{}, fmt.Errorf("mcp: scrape_url: screenshot action is CLI-only (line %d); use page_format screenshot for base64", i+1)
			}
		}
		var sch *extract.Schema
		if len(in.Schema) > 0 {
			raw, err := json.Marshal(in.Schema)
			if err != nil {
				return nil, ScrapeOut{}, fmt.Errorf("mcp: scrape_url schema: %w", err)
			}
			sch, err = extract.ParseSchema(raw)
			if err != nil {
				return nil, ScrapeOut{}, fmt.Errorf("mcp: scrape_url schema: %w", err)
			}
		}
		useCache := true
		if in.UseCache != nil {
			useCache = bool(*in.UseCache)
		}
		var onlyMain bool
		if in.OnlyMainContent != nil {
			onlyMain = bool(*in.OnlyMainContent)
		}
		res, err := scrape.Run(ctx, d.ScrapeDeps, in.URL, scrape.Options{
			Schema: sch, Render: in.Render, Provider: d.DefaultProvider,
			Model: d.DefaultModel, MaxCost: d.MaxCost, UseCache: useCache,
			PageFormat: in.PageFormat,
			Scope:      clean.Scope{Include: []string(in.Include), Exclude: []string(in.Exclude), OnlyMainContent: onlyMain},
			Profile:    in.Profile, Cookies: in.Cookies, Browser: in.Browser,
			Actions: []string(in.Actions), Lang: in.Lang,
			Headers:    []string(in.Headers),
			CaptureXHR: []string(in.CaptureXHR), CDP: in.CDP,
		})
		if err != nil {
			return nil, ScrapeOut{}, fmt.Errorf("mcp: scrape_url: %w", err)
		}
		out := ScrapeOut{URL: res.URL, FinalURL: res.FinalURL, Title: res.Title, FromCache: res.FromCache, XHR: res.XHR}
		if sch == nil {
			out.Markdown = res.Markdown
			if in.PageFormat != "" {
				out.Content = res.Rendered
			}
		} else {
			out.Extracted = res.Record
			if !res.FromCache {
				out.Usage = map[string]any{
					"provider": res.Provider, "model": res.Model,
					"prompt_tokens":     res.Usage.PromptTokens,
					"completion_tokens": res.Usage.CompletionTokens,
					"usd_estimate":      res.Usage.USDEstimate,
				}
			}
		}
		return nil, out, nil
	}
}

// --- crawl_site ---

// CrawlIn is the crawl_site input. Set only RunID to poll a previous run
// (status path: zero extractor calls).
type CrawlIn struct {
	URL             string     `json:"url,omitempty" jsonschema:"seed URL for a fresh crawl"`
	MaxPages        FlexInt    `json:"max_pages,omitempty" jsonschema:"max pages to claim and fetch"`
	MaxDepth        FlexInt    `json:"max_depth,omitempty" jsonschema:"max link depth from seed"`
	SameHost        *FlexBool  `json:"same_host,omitempty" jsonschema:"follow only same-host links (default true)"`
	PathPrefix      string     `json:"path_prefix,omitempty" jsonschema:"only follow links under this path prefix"`
	Include         StringList `json:"include,omitempty" jsonschema:"URL globs to include, e.g. **/docs/** (** crosses /, * stays in one segment)"`
	Exclude         StringList `json:"exclude,omitempty" jsonschema:"URL globs to exclude (wins over include)"`
	AllowSubdomains *FlexBool  `json:"allow_subdomains,omitempty" jsonschema:"follow links into subdomains of the seed host"`
	NoSitemap       *FlexBool  `json:"no_sitemap,omitempty" jsonschema:"skip sitemap seed expansion"`
	SitemapOnly     *FlexBool  `json:"sitemap_only,omitempty" jsonschema:"frontier = sitemap URLs only (no seed enqueue, no link-following); contradicts no_sitemap"`
	AutoThrottle    *FlexBool  `json:"auto_throttle,omitempty" jsonschema:"adaptive per-host pacing: back off on 429/5xx, decay on success"`
	Browser         string     `json:"browser,omitempty" jsonschema:"TLS-impersonating browser fingerprint: chrome, firefox, safari, edge, ios, chrome_android, or random"`
	Schema          FlexMap    `json:"schema,omitempty" jsonschema:"JSON Schema object for extraction"`
	RunID           string     `json:"run_id,omitempty" jsonschema:"poll a previous run instead of crawling"`
}

// CrawlOut is the crawl_site output (fresh run and status poll share it).
type CrawlOut struct {
	RunID        string         `json:"run_id" jsonschema:"run id (pass back as run_id to poll)"`
	PagesCrawled int            `json:"pages_crawled" jsonschema:"pages done plus errored"`
	Records      int            `json:"records" jsonschema:"records extracted (pages_ok)"`
	Errors       int            `json:"errors,omitempty" jsonschema:"pages errored"`
	Status       string         `json:"status" jsonschema:"run status"`
	Usage        map[string]any `json:"usage,omitempty" jsonschema:"accumulated LLM usage"`
}

// flexBool dereferences an optional bool input with a default
// (same_host nil→true; the scope bools nil→false).
func flexBool(b *FlexBool, def bool) bool {
	if b == nil {
		return def
	}
	return bool(*b)
}

func handleCrawl(d Deps) func(context.Context, *sdk.CallToolRequest, CrawlIn) (*sdk.CallToolResult, CrawlOut, error) {
	return func(ctx context.Context, req *sdk.CallToolRequest, in CrawlIn) (*sdk.CallToolResult, CrawlOut, error) {
		// Status-only path: stored status, zero extractor involvement.
		if in.RunID != "" {
			info, err := d.DB.GetRun(in.RunID)
			if err != nil {
				return nil, CrawlOut{}, fmt.Errorf("mcp: crawl_site: %w", err)
			}
			_, _, done, errs, serr := d.DB.CrawlStats(in.RunID)
			if serr != nil {
				return nil, CrawlOut{}, fmt.Errorf("mcp: crawl_site: %w", serr)
			}
			return nil, CrawlOut{
				RunID: in.RunID, PagesCrawled: done + errs, Records: done,
				Errors: errs, Status: info.Status, Usage: runUsage(info),
			}, nil
		}

		sch, err := crawlSchema(map[string]any(in.Schema))
		if err != nil {
			return nil, CrawlOut{}, err
		}
		sameHost := flexBool(in.SameHost, true)
		allowSubdomains, noSitemap := flexBool(in.AllowSubdomains, false), flexBool(in.NoSitemap, false)
		sitemapOnly, autoThrottle := flexBool(in.SitemapOnly, false), flexBool(in.AutoThrottle, false)
		// Contradiction check rides the crawl-side predicate so CLI and MCP
		// can never drift (the crawl package owns its option semantics).
		if err := crawl.ValidateSitemapOnly(sitemapOnly, noSitemap); err != nil {
			return nil, CrawlOut{}, fmt.Errorf("mcp: crawl_site: %w", err)
		}
		key := ""
		if d.ScrapeDeps.APIKeyFor != nil {
			key = d.ScrapeDeps.APIKeyFor(d.DefaultProvider)
		}
		runID := newRunID()
		ex, err := d.ScrapeDeps.ExtractorFor(d.DefaultProvider, key, d.DefaultModel, sch, runID)
		if err != nil {
			return nil, CrawlOut{}, fmt.Errorf("mcp: crawl_site: %w", err)
		}

		// Progress: one NotifyProgress per finished page when the client
		// sent a progress token. Total is omitted — the frontier total is
		// unknowable upfront.
		var progress func(done int)
		if token := req.Params.GetProgressToken(); token != nil {
			progress = func(done int) {
				_ = req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{ //nolint:errcheck // progress is best-effort; a failed notify must not fail the crawl
					ProgressToken: token, Progress: float64(done),
					Message: fmt.Sprintf("%d pages", done),
				})
			}
		}
		res, err := crawl.Run(ctx, crawl.Options{
			SeedURL: in.URL, Schema: sch, MaxPages: int(in.MaxPages), MaxDepth: int(in.MaxDepth),
			SameHost: sameHost, PathPrefix: in.PathPrefix,
			Include: []string(in.Include), Exclude: []string(in.Exclude),
			AllowSubdomains: allowSubdomains, NoSitemap: noSitemap,
			SitemapOnly: sitemapOnly, AutoThrottle: autoThrottle,
			Format: "jsonl", Out: os.DevNull, RunID: runID,
			Browser:  in.Browser,
			Provider: d.DefaultProvider, Model: d.DefaultModel, MaxCost: d.MaxCost,
			DB: d.DB, Extractor: ex, Progress: progress,
		})
		if err != nil {
			return nil, CrawlOut{}, fmt.Errorf("mcp: crawl_site: %w", err)
		}
		info, err := d.DB.GetRun(res.RunID)
		if err != nil {
			return nil, CrawlOut{}, fmt.Errorf("mcp: crawl_site: %w", err)
		}
		return nil, CrawlOut{
			RunID: res.RunID, PagesCrawled: res.PagesOK + res.PagesErr,
			Records: res.Records, Errors: res.PagesErr,
			Status: info.Status, Usage: runUsage(info),
		}, nil
	}
}

func crawlSchema(m map[string]any) (*extract.Schema, error) {
	raw := defaultCrawlSchema
	if len(m) > 0 {
		var err error
		raw, err = json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("mcp: crawl_site schema: %w", err)
		}
	}
	sch, err := extract.ParseSchema(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp: crawl_site schema: %w", err)
	}
	return sch, nil
}

func runUsage(info store.RunInfo) map[string]any {
	return map[string]any{
		"prompt_tokens":     info.PromptTokens,
		"completion_tokens": info.CompletionTokens,
		"usd_estimate":      info.USDEstimate,
		"fetch_pages":       info.FetchPages,
		"fetch_bytes":       info.FetchBytes,
		"fetch_ms":          info.FetchMs,
	}
}

// --- extract_structured ---

// ExtractIn is the extract_structured input (no fetch). Schema and
// Prompt are mutually exclusive: schema validates structured output,
// prompt returns plain text with no validator.
type ExtractIn struct {
	Content     string  `json:"content" jsonschema:"HTML or markdown source to extract from"`
	ContentType string  `json:"content_type,omitempty" jsonschema:"html or markdown (default html)"`
	Schema      FlexMap `json:"schema,omitempty" jsonschema:"JSON Schema object (required unless prompt is set)"`
	Prompt      string  `json:"prompt,omitempty" jsonschema:"free-text instruction; returns plain text with no schema"`
}

// ExtractOut is the extract_structured output.
type ExtractOut struct {
	Extracted map[string]any `json:"extracted,omitempty" jsonschema:"extracted record (with schema)"`
	Content   string         `json:"content,omitempty" jsonschema:"plain-text result (with prompt)"`
	Usage     map[string]any `json:"usage,omitempty" jsonschema:"LLM usage"`
}

func handleExtract(d Deps) func(context.Context, *sdk.CallToolRequest, ExtractIn) (*sdk.CallToolResult, ExtractOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ExtractIn) (*sdk.CallToolResult, ExtractOut, error) {
		if in.Prompt != "" && len(in.Schema) > 0 {
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: prompt and schema are mutually exclusive")
		}
		if in.Prompt != "" {
			return extractPrompt(ctx, d, in)
		}
		if len(in.Schema) == 0 {
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: schema is required")
		}
		raw, err := json.Marshal(in.Schema)
		if err != nil {
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured schema: %w", err)
		}
		sch, err := extract.ParseSchema(raw)
		if err != nil {
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured schema: %w", err)
		}
		ct := in.ContentType
		if ct == "" {
			ct = "html"
		}
		var markdown string
		var sidecar json.RawMessage
		switch ct {
		case "html":
			cleaned, err := clean.Clean(ctx, clean.RawPage{HTML: []byte(in.Content)})
			if err != nil {
				return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: %w", err)
			}
			markdown, sidecar = cleaned.Markdown, cleaned.StructuredData
		case "markdown":
			markdown = in.Content
		default:
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: content_type %q must be html|markdown", ct)
		}

		runID := newRunID()
		if err := d.DB.BeginRun(runID, "mcp-extract"); err != nil {
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: %w", err)
		}
		finish := func(ok int, status string) {
			if err := d.DB.FinishRun(runID, ok, 0, status); err != nil {
				fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", err)
			}
		}
		key := ""
		if d.ScrapeDeps.APIKeyFor != nil {
			key = d.ScrapeDeps.APIKeyFor(d.DefaultProvider)
		}
		ex, err := d.ScrapeDeps.ExtractorFor(d.DefaultProvider, key, d.DefaultModel, sch, runID)
		if err != nil {
			finish(0, "error")
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: %w", err)
		}
		res, err := ex.Extract(ctx, extract.ExtractInput{
			Markdown: markdown, StructuredData: sidecar, Schema: sch,
		})
		if err != nil {
			finish(0, "error")
			return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: %w", err)
		}
		finish(1, "finished")
		return nil, ExtractOut{Extracted: res.Record, Usage: map[string]any{
			"provider": res.Provider, "model": res.Model,
			"prompt_tokens":     res.Usage.PromptTokens,
			"completion_tokens": res.Usage.CompletionTokens,
			"usd_estimate":      res.Usage.USDEstimate,
		}}, nil
	}
}

// --- get_cached_selectors ---

// SelectorsIn is the get_cached_selectors input.
type SelectorsIn struct {
	Domain     string `json:"domain" jsonschema:"domain to list cached selectors for"`
	SchemaHash string `json:"schema_hash,omitempty" jsonschema:"only this schema hash"`
}

// CachedSelector is one cached doc.
type CachedSelector struct {
	SchemaHash    string             `json:"schema_hash" jsonschema:"schema hash"`
	Fields        map[string]any     `json:"fields" jsonschema:"selector fields"`
	SynthesizedAt string             `json:"synthesized_at" jsonschema:"synthesis time"`
	NullRates     map[string]float64 `json:"null_rates,omitempty" jsonschema:"per-field null rates"`
}

// SelectorsOut is the get_cached_selectors output.
type SelectorsOut struct {
	Domain    string           `json:"domain" jsonschema:"queried domain"`
	Selectors []CachedSelector `json:"selectors" jsonschema:"cached selector docs"`
}

func handleSelectors(d Deps) func(context.Context, *sdk.CallToolRequest, SelectorsIn) (*sdk.CallToolResult, SelectorsOut, error) {
	return func(_ context.Context, _ *sdk.CallToolRequest, in SelectorsIn) (*sdk.CallToolResult, SelectorsOut, error) {
		entries, err := d.DB.ListSelectors(in.Domain)
		if err != nil {
			return nil, SelectorsOut{}, fmt.Errorf("mcp: get_cached_selectors: %w", err)
		}
		out := SelectorsOut{Domain: in.Domain}
		for _, e := range entries {
			if in.SchemaHash != "" && e.SchemaHash != in.SchemaHash {
				continue
			}
			var doc selector.SelectorDoc
			if err := json.Unmarshal([]byte(e.FieldsJSON), &doc); err != nil {
				continue
			}
			fields := map[string]any{}
			nulls := map[string]float64{}
			for name, sel := range doc.Fields {
				fields[name] = map[string]any{"type": sel.Type, "expr": sel.Expr}
				nulls[name] = sel.NullRate
			}
			out.Selectors = append(out.Selectors, CachedSelector{
				SchemaHash: e.SchemaHash, Fields: fields,
				SynthesizedAt: e.SynthesizedAt, NullRates: nulls,
			})
		}
		if out.Selectors == nil {
			out.Selectors = []CachedSelector{}
		}
		return nil, out, nil
	}
}

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%x-%d", b, os.Getpid())
	}
	// ponytail: timestamp+pid fallback on RNG failure.
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}
