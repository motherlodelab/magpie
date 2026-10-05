// Package mcp exposes the pipeline as an MCP server: scrape_url,
// crawl_site, extract_structured, get_cached_selectors plus the Phase C
// agent surface (batch, map, summarize, diff, brand, list_extractors,
// vertical_scrape), search, and deep research. Handlers share scrape.Run /
// crawl.Run / research.Run with the CLI; the transport (stdio vs
// Streamable HTTP) is chosen in cli/serve.go.
package mcp

import (
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deps wires the server to storage and provider constructors.
// ScrapeDeps carries the DB + ExtractorFor/APIKeyFor func fields so mcp
// never imports the CLI package.
type Deps struct {
	DB              *store.DB
	ScrapeDeps      scrape.Deps
	DefaultProvider string
	DefaultModel    string
	MaxCost         float64
}

// NewServer registers the thirteen tools on a fresh server. Every tool goes
// through widenedTool (schema inferred from In exactly as before, then
// widened for stringy clients) — never bare sdk.Tool literals, so a new
// tool cannot silently miss coercion.
func NewServer(d Deps) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "magpie", Version: "v1.0.0"}, nil)
	sdk.AddTool(server, widenedTool[ScrapeIn]("scrape_url", "Fetch, clean and extract one URL"), handleScrape(d))
	sdk.AddTool(server, widenedTool[CrawlIn]("crawl_site", "Crawl a site (synchronous) or poll a previous run by run_id"), handleCrawl(d))
	sdk.AddTool(server, widenedTool[ExtractIn]("extract_structured", "Extract structured data from HTML or markdown (no fetch)"), handleExtract(d))
	sdk.AddTool(server, widenedTool[SelectorsIn]("get_cached_selectors", "List cached selectors for a domain"), handleSelectors(d))
	sdk.AddTool(server, widenedTool[BatchIn]("batch", "Scrape up to 100 URLs with bounded concurrency (markdown only, zero LLM)"), handleBatch(d))
	sdk.AddTool(server, widenedTool[MapIn]("map", "List sitemap-derived URLs for a site"), handleMap(d))
	sdk.AddTool(server, widenedTool[SummarizeIn]("summarize", "Summarize one URL in at most N sentences"), handleSummarize(d))
	sdk.AddTool(server, widenedTool[DiffIn]("diff", "Word-level diff of a URL against a previous markdown snapshot"), handleDiff(d))
	sdk.AddTool(server, widenedTool[BrandIn]("brand", "Extract brand colors, fonts, logo and favicon (zero LLM)"), handleBrand(d))
	sdk.AddTool(server, widenedTool[ListExtractorsIn]("list_extractors", "List zero-LLM vertical extractors"), handleListExtractors(d))
	sdk.AddTool(server, widenedTool[VerticalScrapeIn]("vertical_scrape", "Extract one URL with a named vertical extractor (zero LLM)"), handleVertical(d))
	sdk.AddTool(server, widenedTool[SearchIn]("search", "Search the web via BYOK/no-key SERP providers, optionally scrape the top hits"), handleSearch(d))
	sdk.AddTool(server, widenedTool[ResearchIn]("research", "Deep research: plan, search, read, verify every quote against a stored snapshot, write a cited report (unmetered; capped by max_cost_usd)"), handleResearch(d))
	return server
}
