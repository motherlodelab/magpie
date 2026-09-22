---
name: magpie
description: Drive the magpie web scraper (CLI + MCP) — fetch, clean, and extract structured data from any page with zero LLM by default
---

# magpie — web scraping for agents

magpie is a local-first Go CLI that turns web pages into clean markdown or
typed JSON: fetch (requests, with browser escalation only when a page needs
it) → clean (trafilatura boilerplate removal) → extract (JSON Schema,
zero-LLM selectors, or BYOK LLM). One static binary, no account, no credits.

Setup: `magpie init --client <claude-code|claude-desktop|cursor>` writes the
MCP stanza into that client's config (merge, idempotent), or copy this stanza
into the config by hand:

```json
{ "mcpServers": { "magpie": { "command": "/abs/path/to/magpie", "args": ["serve"] } } }
```

## Prefer zero-LLM first

Extraction costs no tokens unless you ask for it. In order of preference:

1. `vertical` / `vertical_scrape` — typed extraction from a known vertical
   (job postings, products, events, articles, …), zero LLM.
2. `extract` / `extract_structured` with a JSON Schema — CSS-selector and
   JSON-LD hints do the work; LLM only repairs if configured.
3. Plain markdown via `scrape` / `scrape_url` — usually enough to answer.
4. `summarize` (LLM) only when a summary itself is the deliverable.

## CLI verbs

| Verb | Action |
| :-- | :-- |
| `magpie scrape <url>` | Fetch → clean → extract a single URL |
| `magpie crawl <url>` | BFS crawl + extract a site |
| `magpie extract` | Extract structured data from stdin/file (no fetch) |
| `magpie summarize <url>` | Summarize one URL in at most N sentences |
| `magpie search <query>` | Web search via BYOK/no-key SERP provider, optionally scrape the top hits |
| `magpie map <site>` | List sitemap-derived URLs for a site |
| `magpie diff <url> --against <file>` | Word-level diff of a URL vs a previous markdown snapshot |
| `magpie brand <url>` | Brand colors, fonts, logo, favicon (zero LLM) |
| `magpie vertical [--list] [<url> --name]` | Zero-LLM typed extraction |

Run `magpie <verb> --help` for flags (output format, selectors, schema,
browser escalation, proxy, cookies, …).

## MCP tools (via `magpie serve`)

| Tool | Action |
| :-- | :-- |
| `scrape_url` | Fetch → clean → extract one URL (schema optional; `actions` DSL lines and `lang` accepted) |
| `crawl_site` | Crawl a site, or poll a previous run via `run_id` (synchronous — keep `MaxPages` bounded) |
| `extract_structured` | Extract from HTML/markdown, no fetch (schema) or plain text (prompt) |
| `get_cached_selectors` | List cached selectors for a domain |
| `batch` | Scrape ≤100 URLs with bounded concurrency (markdown only, zero LLM) |
| `map` | List sitemap-derived URLs for a site |
| `summarize` | Summarize one URL in ≤N sentences |
| `diff` | Word-level diff of a URL vs a previous snapshot |
| `brand` | Brand colors, fonts, logo, favicon (zero LLM) |
| `list_extractors` | List zero-LLM vertical extractors |
| `vertical_scrape` | Extract one URL with a named vertical (zero LLM) |
| `search` | Search the web via BYOK/no-key SERP providers, optionally scrape the top hits |

## extract_structured schema example

`schema` is a JSON Schema (draft 2020-12); optional per-field `x-magpie`
hints add CSS selectors, regex, coercion:

```json
{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "price"],
  "properties": {
    "name":  { "type": "string", "x-magpie": { "css_hint": "h1", "trim": true } },
    "price": { "type": "number", "x-magpie": { "css_hint": "span.price", "coerce": "eur_decimal" } }
  }
}
```

## Politeness

Per-host crawl rate is capped at 1 request/second. Keep `MaxPages` bounded —
`crawl_site` runs synchronously, so an unbounded crawl blocks your tool call
until it finishes. Respect robots.txt outcomes (`ErrRobotsBlocked`) instead
of retrying around them.
