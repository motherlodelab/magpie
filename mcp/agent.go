package mcp

// Agent-surface tools (Phase C): batch, map, summarize, diff, brand,
// list_extractors, vertical_scrape. Handlers are thin shims over scrape/,
// crawl/, clean/, and vertical/ — same as tools.go. Item errors are data,
// never whole-tool aborts.

import (
	"context"
	"fmt"
	"os"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
	"github.com/motherlodelab/magpie/vertical"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- batch ---

// BatchIn is the batch input: up to 100 URLs, markdown-only, zero LLM.
type BatchIn struct {
	URLs            StringList `json:"urls" jsonschema:"up to 100 absolute http(s) or file URLs to scrape"`
	Concurrency     FlexInt    `json:"concurrency,omitempty" jsonschema:"max parallel scrapes (default 8)"`
	Render          string     `json:"render,omitempty" jsonschema:"auto, static, or browser"`
	Profile         string     `json:"profile,omitempty" jsonschema:"header profile: default, chrome, firefox, safari, edge, ios, or chrome_android"`
	Cookies         string     `json:"cookies,omitempty" jsonschema:"raw Cookie header value"`
	Include         StringList `json:"include,omitempty" jsonschema:"CSS selectors: scrape only matching subtrees"`
	Exclude         StringList `json:"exclude,omitempty" jsonschema:"CSS selectors: drop matching nodes"`
	OnlyMainContent *FlexBool  `json:"only_main_content,omitempty" jsonschema:"main-content only"`
}

// BatchItemOut is one per-URL record.
type BatchItemOut struct {
	URL      string `json:"url" jsonschema:"requested URL"`
	OK       bool   `json:"ok" jsonschema:"true when markdown was produced"`
	Title    string `json:"title,omitempty" jsonschema:"page title"`
	Markdown string `json:"markdown,omitempty" jsonschema:"cleaned markdown"`
	Error    string `json:"error,omitempty" jsonschema:"error string when ok is false"`
}

// BatchOut is the batch output: one record per input URL, input order.
type BatchOut struct {
	Results []BatchItemOut `json:"results" jsonschema:"per-URL records in input order"`
}

func handleBatch(d Deps) func(context.Context, *sdk.CallToolRequest, BatchIn) (*sdk.CallToolResult, BatchOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in BatchIn) (*sdk.CallToolResult, BatchOut, error) {
		conc := int(in.Concurrency)
		if conc == 0 {
			conc = scrape.DefaultBatchConcurrency
		}
		var onlyMain bool
		if in.OnlyMainContent != nil {
			onlyMain = bool(*in.OnlyMainContent)
		}
		items, err := scrape.Batch(ctx, d.ScrapeDeps, []string(in.URLs), scrape.BatchOptions{
			Concurrency: conc, Render: in.Render, Profile: in.Profile, Cookies: in.Cookies,
			Scope: clean.Scope{Include: []string(in.Include), Exclude: []string(in.Exclude), OnlyMainContent: onlyMain},
		})
		if err != nil {
			return nil, BatchOut{}, fmt.Errorf("mcp: batch: %w", err)
		}
		out := BatchOut{Results: make([]BatchItemOut, 0, len(items))}
		for _, it := range items {
			out.Results = append(out.Results, BatchItemOut{
				URL: it.URL, OK: it.OK, Title: it.Title, Markdown: it.Markdown, Error: it.Error,
			})
		}
		return nil, out, nil
	}
}

// --- map ---

// MapIn is the map input.
type MapIn struct {
	Site string `json:"site" jsonschema:"site origin to list sitemap URLs for"`
}

// MapOut is the map output.
type MapOut struct {
	Site      string   `json:"site" jsonschema:"mapped site"`
	URLs      []string `json:"urls" jsonschema:"sitemap-derived URLs"`
	Truncated bool     `json:"truncated" jsonschema:"true when sitemap caps cut the listing"`
}

func handleMap(d Deps) func(context.Context, *sdk.CallToolRequest, MapIn) (*sdk.CallToolResult, MapOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in MapIn) (*sdk.CallToolResult, MapOut, error) {
		urls, truncated, err := crawl.ListSitemapURLs(ctx, d.ScrapeDeps.Fetcher, in.Site)
		if err != nil {
			return nil, MapOut{}, fmt.Errorf("mcp: map: %w", err)
		}
		if urls == nil {
			urls = []string{}
		}
		return nil, MapOut{Site: in.Site, URLs: urls, Truncated: truncated}, nil
	}
}

// --- summarize ---

// SummarizeIn is the summarize input.
type SummarizeIn struct {
	URL          string  `json:"url" jsonschema:"absolute http(s) or file URL to summarize"`
	MaxSentences FlexInt `json:"max_sentences,omitempty" jsonschema:"max sentences in the summary (default 3, clamp 1-20)"`
	Provider     string  `json:"provider,omitempty" jsonschema:"LLM provider (default server provider; auto tries keyed providers in order)"`
	Model        string  `json:"model,omitempty" jsonschema:"model name (default server model)"`
}

// SummarizeOut is the summarize output.
type SummarizeOut struct {
	URL     string         `json:"url" jsonschema:"requested URL"`
	Title   string         `json:"title" jsonschema:"page title"`
	Summary string         `json:"summary" jsonschema:"summary text, at most max_sentences sentences"`
	Usage   map[string]any `json:"usage,omitempty" jsonschema:"LLM usage"`
}

func handleSummarize(d Deps) func(context.Context, *sdk.CallToolRequest, SummarizeIn) (*sdk.CallToolResult, SummarizeOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in SummarizeIn) (*sdk.CallToolResult, SummarizeOut, error) {
		provider := in.Provider
		if provider == "" {
			provider = d.DefaultProvider
		}
		model := in.Model
		if model == "" {
			model = d.DefaultModel
		}
		res, err := scrape.Summarize(ctx, d.ScrapeDeps, in.URL, scrape.SummarizeOptions{
			MaxSentences: int(in.MaxSentences), Provider: provider, Model: model, MaxCost: d.MaxCost,
		})
		if err != nil {
			return nil, SummarizeOut{}, fmt.Errorf("mcp: summarize: %w", err)
		}
		return nil, SummarizeOut{URL: res.URL, Title: res.Title, Summary: res.Summary, Usage: extract.UsageMap(res.Provider, res.Model, res.Usage)}, nil
	}
}

// --- diff ---

// DiffIn is the diff input: current page vs an inline previous snapshot.
type DiffIn struct {
	URL              string `json:"url" jsonschema:"absolute http(s) or file URL to diff"`
	PreviousSnapshot string `json:"previous_snapshot" jsonschema:"previous markdown snapshot to diff against"`
}

// DiffOut is the diff output: empty diff means identical.
type DiffOut struct {
	URL     string `json:"url" jsonschema:"requested URL"`
	Diff    string `json:"diff" jsonschema:"word-level -old +new stream (empty when identical)"`
	Changed bool   `json:"changed" jsonschema:"false when the page matches the snapshot"`
}

func handleDiff(d Deps) func(context.Context, *sdk.CallToolRequest, DiffIn) (*sdk.CallToolResult, DiffOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in DiffIn) (*sdk.CallToolResult, DiffOut, error) {
		res, err := scrape.Run(ctx, d.ScrapeDeps, in.URL, scrape.Options{})
		if err != nil {
			return nil, DiffOut{}, fmt.Errorf("mcp: diff: %w", err)
		}
		diff, err := scrape.DiffWords(in.PreviousSnapshot, res.Markdown)
		if err != nil {
			return nil, DiffOut{}, fmt.Errorf("mcp: diff: %w", err)
		}
		return nil, DiffOut{URL: res.URL, Diff: diff, Changed: diff != ""}, nil
	}
}

// --- brand ---

// BrandIn is the brand input.
type BrandIn struct {
	URL string `json:"url" jsonschema:"absolute http(s) or file URL to extract brand from"`
}

// BrandOut is the brand output: zero LLM.
type BrandOut struct {
	URL     string   `json:"url" jsonschema:"requested URL"`
	Title   string   `json:"title" jsonschema:"page title"`
	Colors  []string `json:"colors" jsonschema:"hex colors in first-seen order"`
	Fonts   []string `json:"fonts" jsonschema:"font families in first-seen order"`
	Logo    string   `json:"logo,omitempty" jsonschema:"logo image src, inline-SVG hook, or og:image fallback"`
	Favicon string   `json:"favicon,omitempty" jsonschema:"absolute favicon URL"`
}

func handleBrand(d Deps) func(context.Context, *sdk.CallToolRequest, BrandIn) (*sdk.CallToolResult, BrandOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in BrandIn) (*sdk.CallToolResult, BrandOut, error) {
		res, err := scrape.BrandPage(ctx, d.ScrapeDeps, in.URL)
		if err != nil {
			return nil, BrandOut{}, fmt.Errorf("mcp: brand: %w", err)
		}
		return nil, BrandOut{
			URL: res.URL, Title: res.Title,
			Colors: res.Colors, Fonts: res.Fonts, Logo: res.Logo, Favicon: res.Favicon,
		}, nil
	}
}

// --- list_extractors ---

// ListExtractorsIn is the list_extractors input (no fields).
type ListExtractorsIn struct{}

// ExtractorInfoOut describes one zero-LLM extractor.
type ExtractorInfoOut struct {
	Name     string   `json:"name" jsonschema:"extractor name (for vertical_scrape)"`
	Label    string   `json:"label" jsonschema:"human label"`
	Desc     string   `json:"desc" jsonschema:"what it extracts"`
	Patterns []string `json:"patterns" jsonschema:"example URL shapes"`
}

// ListExtractorsOut is the list_extractors output.
type ListExtractorsOut struct {
	Extractors []ExtractorInfoOut `json:"extractors" jsonschema:"all registered extractors"`
}

func handleListExtractors(_ Deps) func(context.Context, *sdk.CallToolRequest, ListExtractorsIn) (*sdk.CallToolResult, ListExtractorsOut, error) {
	return func(_ context.Context, _ *sdk.CallToolRequest, _ ListExtractorsIn) (*sdk.CallToolResult, ListExtractorsOut, error) {
		infos := vertical.List()
		out := ListExtractorsOut{Extractors: make([]ExtractorInfoOut, 0, len(infos))}
		for _, info := range infos {
			out.Extractors = append(out.Extractors, ExtractorInfoOut{
				Name: info.Name, Label: info.Label, Desc: info.Desc, Patterns: info.Patterns,
			})
		}
		return nil, out, nil
	}
}

// --- vertical_scrape ---

// VerticalScrapeIn is the vertical_scrape input.
type VerticalScrapeIn struct {
	URL      string `json:"url" jsonschema:"absolute http(s) or file URL to extract"`
	Vertical string `json:"vertical" jsonschema:"extractor name (see list_extractors)"`
}

// VerticalScrapeOut is the vertical_scrape output: zero LLM.
type VerticalScrapeOut struct {
	URL      string         `json:"url" jsonschema:"requested URL"`
	Vertical string         `json:"vertical" jsonschema:"extractor that handled the URL"`
	Record   map[string]any `json:"record" jsonschema:"typed extraction record"`
}

func handleVertical(d Deps) func(context.Context, *sdk.CallToolRequest, VerticalScrapeIn) (*sdk.CallToolResult, VerticalScrapeOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in VerticalScrapeIn) (*sdk.CallToolResult, VerticalScrapeOut, error) {
		if in.Vertical == "" {
			// Required, not auto: an empty name is a caller bug, and failing
			// here names the fix (list_extractors). Run would reject "" as
			// an unknown name anyway — this message is the actionable one.
			return nil, VerticalScrapeOut{}, fmt.Errorf("mcp: vertical_scrape: vertical is required (see list_extractors)")
		}
		// Explicit vertical rides the shared Run dispatch: unknown names
		// fail pre-I/O, mismatches return ErrURLMismatch wording.
		res, err := scrape.Run(ctx, d.ScrapeDeps, in.URL, scrape.Options{Vertical: in.Vertical})
		if err != nil {
			return nil, VerticalScrapeOut{}, fmt.Errorf("mcp: vertical_scrape: %w", err)
		}
		return nil, VerticalScrapeOut{URL: res.URL, Vertical: res.Vertical, Record: res.Record}, nil
	}
}

// extractPrompt serves the extract_structured prompt path: schema-less
// text with no validator. The prompt fan-out lives in scrape.Prompt;
// this handler owns the content prep and the run.
func extractPrompt(ctx context.Context, d Deps, in ExtractIn) (*sdk.CallToolResult, ExtractOut, error) {
	markdown, _, err := prepContent(ctx, in)
	if err != nil {
		return nil, ExtractOut{}, err
	}

	runID := store.NewRunID()
	if err := d.DB.BeginRun(runID, "mcp-extract"); err != nil {
		return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: %w", err)
	}
	finish := func(ok int, status string) {
		if err := d.DB.FinishRun(runID, ok, 0, status); err != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", err)
		}
	}
	pr, err := scrape.Prompt(ctx, d.ScrapeDeps, runID, scrape.PromptOptions{
		Provider: d.DefaultProvider, Model: d.DefaultModel, MaxCost: d.MaxCost,
		System: "Reply with plain text only, no JSON.",
		User:   in.Prompt + "\n\nContent:\n" + markdown, Purpose: "extract",
	})
	if err != nil {
		finish(0, "error")
		return nil, ExtractOut{}, fmt.Errorf("mcp: extract_structured: %w", err)
	}
	finish(1, "finished")
	return nil, ExtractOut{Content: pr.Text, Usage: extract.UsageMap(pr.Provider, d.DefaultModel, pr.Usage)}, nil
}

// --- search ---

// SearchIn is the search input: a SERP query via a BYOK/no-key backend,
// optionally scraping the top hits through the normal pipeline.
type SearchIn struct {
	Query     string  `json:"query" jsonschema:"search query"`
	Provider  string  `json:"provider,omitempty" jsonschema:"brave, serper, serpapi, searxng, exa, or duckduckgo (default duckduckgo — zero-key)"`
	Limit     FlexInt `json:"limit,omitempty" jsonschema:"max hits (default 10)"`
	ScrapeTop FlexInt `json:"scrape_top,omitempty" jsonschema:"scrape the first N hits through the normal pipeline"`
}

// SearchOut is the search output: hits in position order, each with an
// optional page record when scrape_top reached it.
type SearchOut struct {
	Query string                `json:"query" jsonschema:"the query"`
	Hits  []scrape.SearchRecord `json:"hits" jsonschema:"hits in position order; page carries the scraped record when scrape_top ran"`
}

func handleSearch(d Deps) func(context.Context, *sdk.CallToolRequest, SearchIn) (*sdk.CallToolResult, SearchOut, error) {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in SearchIn) (*sdk.CallToolResult, SearchOut, error) {
		records, err := scrape.Search(ctx, d.ScrapeDeps, in.Query, scrape.SearchOptions{
			Provider: in.Provider, Limit: int(in.Limit), ScrapeTop: int(in.ScrapeTop),
		})
		if err != nil {
			return nil, SearchOut{}, fmt.Errorf("mcp: search: %w", err)
		}
		if records == nil {
			records = []scrape.SearchRecord{}
		}
		return nil, SearchOut{Query: in.Query, Hits: records}, nil
	}
}
