package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"

	"github.com/spf13/cobra"
)

func newScrapeCmd() *cobra.Command {
	var schema, render, provider, model, out, format string
	var noCache bool
	var pageFormat, headerProfile, cookies, browser, viewport string
	var include, exclude []string
	var onlyMainContent bool
	var verticalName string
	var actionLines []string
	var actionsFile, lang string
	var captureXHR []string
	var cdpURL string
	var headers []string
	cmd := &cobra.Command{
		Use:   "scrape <url>",
		Short: "Fetch → clean → extract a single URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Action file lines run first, then inline --action lines in
			// flag order (proxy-file precedent for the file format).
			var actions []string
			if actionsFile != "" {
				lines, err := readActionLines(actionsFile)
				if err != nil {
					return err
				}
				actions = append(actions, lines...)
			}
			actions = append(actions, actionLines...)
			return runScrape(cmd.Context(), args[0], scrapeOptions{
				Schema: schema, Render: render, Provider: provider, Model: model,
				Out: out, Format: format, NoCache: noCache,
				PageFormat: pageFormat, Include: include, Exclude: exclude,
				OnlyMainContent: onlyMainContent, HeaderProfile: headerProfile, Cookies: cookies,
				Browser: browser, Vertical: verticalName, Viewport: viewport,
				Actions: actions, Lang: lang,
				Headers:    headers,
				CaptureXHR: captureXHR, CDP: cdpURL,
			})
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "JSON Schema file (yaml/json)")
	cmd.Flags().StringVar(&render, "render", "", "auto|static|browser")
	cmd.Flags().StringVar(&provider, "provider", "", ProviderHelp)
	cmd.Flags().StringVar(&model, "model", "", "model name")
	cmd.Flags().StringVar(&out, "out", "", "output path (default stdout)")
	cmd.Flags().StringVar(&format, "format", "", "json|jsonl (csv|sqlite not supported in Phase 1)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "bypass selector cache")
	cmd.Flags().StringVar(&pageFormat, "page-format", "", "page output format: markdown|llm|text|json|html|raw|screenshot")
	cmd.Flags().StringSliceVar(&include, "include", nil, "comma-separated CSS selectors: scrape only matching subtrees")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "comma-separated CSS selectors: drop matching nodes")
	cmd.Flags().BoolVar(&onlyMainContent, "only-main-content", false, "main-content only (trafilatura already does this)")
	cmd.Flags().StringVar(&headerProfile, "header-profile", "", "request header bundle: default|chrome|firefox|safari|edge|ios|chrome_android")
	cmd.Flags().StringVar(&cookies, "cookies", "", "raw Cookie header value, e.g. \"a=b; c=d\"")
	cmd.Flags().StringVar(&browser, "browser", "", "TLS-impersonating browser fingerprint: "+fetch.BrowserHelp)
	cmd.Flags().StringVar(&viewport, "viewport", "", "screenshot viewport WxH, e.g. 1280x800 (page-format screenshot only)")
	cmd.Flags().StringVar(&verticalName, "vertical", "", "zero-LLM typed extractor: auto or a name (default off; `magpie vertical --list` in Phase C)")
	cmd.Flags().StringSliceVar(&actionLines, "action", nil, "browser action line, repeatable: click <sel> | type <sel> <text…> | scroll <n|top|bottom> | wait <ms> | wait-for <sel> | screenshot <path> | eval-js <expr…>")
	cmd.Flags().StringVar(&actionsFile, "actions", "", "action file: one action per line, # comments, rest-of-line args need no quoting")
	cmd.Flags().StringVar(&lang, "lang", "", "Accept-Language header value, e.g. fr-CA,fr;q=0.9 (no control characters)")
	cmd.Flags().StringSliceVar(&headers, "header", nil, "raw request header, repeatable: \"Name: value\" (no control characters; wins over profile defaults)")
	cmd.Flags().StringSliceVar(&captureXHR, "capture-xhr", nil, "Go regexp: capture matching XHR/fetch response bodies (repeatable; browser rendering only)")
	cmd.Flags().StringVar(&cdpURL, "cdp-url", "", "remote browser CDP endpoint (ws://, wss://, http(s)://); overrides MAGPIE_CDP_URL — never launches a local browser")
	return cmd
}

// readActionLines reads a proxy-file-style action file: one action per
// line; blanks and # comments are skipped by the parser but kept here so
// inline line numbers stay aligned with the file.
func readActionLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fail(2, "read actions file: %v", err)
	}
	return strings.Split(string(raw), "\n"), nil
}

type scrapeOptions struct {
	Schema   string
	Render   string
	Provider string
	Model    string
	Out      string
	Format   string
	NoCache  bool
	// PageFormat is flag-only: it must never enter config.Config.Format,
	// which crawl validates as jsonl|json|csv|sqlite.
	PageFormat      string
	Include         []string
	Exclude         []string
	OnlyMainContent bool
	HeaderProfile   string
	Cookies         string
	Browser         string
	Viewport        string
	// Vertical is flag-only (auto|name, default off): validated pre-I/O.
	Vertical string
	// Actions are browser action lines (file first, then inline) and Lang
	// the Accept-Language override; both validated pre-I/O by scrape.
	Actions []string
	Lang    string
	// CaptureXHR patterns and CDP endpoint; both validated pre-I/O by
	// scrape.ValidateOptions (render=static + capture-xhr rejected).
	CaptureXHR []string
	CDP        string
	// Headers are raw "Name: value" request headers (--header,
	// repeatable); validated pre-I/O by scrape.ValidateOptions.
	Headers []string
}

func runScrape(ctx context.Context, rawURL string, o scrapeOptions) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	applyScrapeFlags(&cfg, o)

	if cfg.Format == "csv" || cfg.Format == "sqlite" {
		return fail(2, "format %q not supported in Phase 1 (use json|jsonl)", cfg.Format)
	}
	if cfg.Format == "" {
		cfg.Format = "json"
	}
	if cfg.Format != "json" && cfg.Format != "jsonl" {
		return fail(2, "format %q must be json|jsonl", cfg.Format)
	}
	// One validation site (scrape owns the switches): render, page format,
	// browser fingerprint, vertical name, actions, lang — all pre-I/O,
	// exit 2 via exitFor.
	if err := scrape.ValidateOptions(scrape.Options{
		Render: cfg.Render, PageFormat: o.PageFormat,
		Browser: o.Browser, Vertical: o.Vertical,
		Actions: o.Actions, Lang: o.Lang,
		Headers:    o.Headers,
		CaptureXHR: o.CaptureXHR, CDP: o.CDP,
	}); err != nil {
		return err
	}

	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)

	provider := cfg.ExtractProvider
	if o.Provider != "" {
		provider = o.Provider
	}
	model := cfg.Model
	if o.Model != "" {
		model = o.Model
	}

	var sch *extract.Schema
	if cfg.Schema != "" {
		sch, err = extract.LoadSchema(cfg.Schema)
		if err != nil {
			return err
		}
	}

	res, err := scrape.Run(ctx, scrapeDeps(db, cfg), rawURL, scrape.Options{
		Schema: sch, Render: cfg.Render, Provider: provider, Model: model,
		MaxCost: cfg.MaxCost, UseCache: !cfg.NoCache,
		PageFormat: o.PageFormat,
		Scope:      clean.Scope{Include: o.Include, Exclude: o.Exclude, OnlyMainContent: o.OnlyMainContent},
		Profile:    o.HeaderProfile, Cookies: o.Cookies, Browser: o.Browser,
		Vertical: o.Vertical, Viewport: o.Viewport,
		Actions: o.Actions, Lang: o.Lang,
		Headers:    o.Headers,
		CaptureXHR: o.CaptureXHR, CDP: o.CDP,
	})
	if err != nil {
		return keyHint(err) // exitFor owns the code mapping
	}
	// Screenshot owns its whole output contract in one place: --out
	// writes the raw PNG; without it the base64 rides in a JSON envelope
	// (same shape the MCP tool returns) — never silently dropped.
	if o.PageFormat == "screenshot" {
		if cfg.Out != "" {
			if err := os.WriteFile(cfg.Out, res.ScreenshotPNG, 0o644); err != nil {
				return fmt.Errorf("write out: %w", err)
			}
			return nil
		}
		sdoc, merr := screenshotDoc(res)
		if merr != nil {
			return merr
		}
		return writeOut(cfg.Out, sdoc)
	}
	if res.Vertical != "" {
		vdoc, merr := verticalDoc(res)
		if merr != nil {
			return merr
		}
		return writeOut(cfg.Out, vdoc)
	}
	if sch == nil {
		// Content formats print the rendered page directly. html/raw must
		// stay in this branch: markdownDoc's envelope has no field for
		// them, so falling through would silently drop the requested
		// content (the screenshot-drop bug, again).
		switch o.PageFormat {
		case "json":
			// XHR captures ride the json envelope when present; output
			// stays byte-identical when --capture-xhr is absent (Phase-G
			// lesson: one cohesive block per output contract).
			if len(res.XHR) > 0 {
				doc, merr := xhrJSONEnvelope(res.Rendered, res.XHR)
				if merr != nil {
					return merr
				}
				return writeOut(cfg.Out, doc)
			}
			return writeOut(cfg.Out, res.Rendered)
		case "llm", "text", "html", "raw":
			return writeOut(cfg.Out, res.Rendered)
		}
		mdoc, merr := markdownDoc(res)
		if merr != nil {
			return merr
		}
		return writeOut(cfg.Out, mdoc)
	}
	edoc, merr := extractedDoc(res)
	if merr != nil {
		return merr
	}
	return writeOut(cfg.Out, edoc)
}

func applyScrapeFlags(cfg *config.Config, o scrapeOptions) {
	f := config.Flags{}
	if o.Provider != "" {
		f.Provider, f.ProviderChanged = o.Provider, true
	}
	if o.Model != "" {
		f.Model, f.ModelChanged = o.Model, true
	}
	if o.Render != "" {
		f.Render, f.RenderChanged = o.Render, true
	}
	if o.Format != "" {
		f.Format, f.FormatChanged = o.Format, true
	}
	if o.Out != "" {
		f.Out, f.OutChanged = o.Out, true
	}
	if o.Schema != "" {
		f.Schema, f.SchemaChanged = o.Schema, true
	}
	if o.NoCache {
		f.NoCache, f.NoCacheChanged = true, true
	}
	cfg.ApplyFlags(f)
}

func markdownDoc(r scrape.Result) (string, error) {
	return marshalOut(markdownOut{
		URL: r.URL, FinalURL: r.FinalURL, Title: r.Title,
		Markdown: r.Markdown, StructuredData: orEmpty(r.StructuredData),
		XHR: r.XHR,
	}, "scrape")
}

// xhrJSONEnvelope injects captured XHR bodies into the page-format json
// envelope (same keys + xhr; only called when captures exist).
func xhrJSONEnvelope(rendered string, xhr []fetch.XHRCapture) (string, error) {
	// UseNumber keeps the page JSON's number literals verbatim on the
	// re-marshal — plain Unmarshal makes every number a float64 and
	// silently rounds IDs above 2^53 (the data this flag exists to fetch).
	dec := json.NewDecoder(strings.NewReader(rendered))
	dec.UseNumber()
	var env map[string]any
	if err := dec.Decode(&env); err != nil {
		return "", fmt.Errorf("xhr envelope: %w", err)
	}
	env["xhr"] = xhr
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func extractedDoc(r scrape.Result) (string, error) {
	out := extractedOut{
		URL: r.URL, FinalURL: r.FinalURL, Title: r.Title,
		Extracted: r.Record, FromCache: r.FromCache,
	}
	if !r.FromCache {
		out.Usage = usageOut{
			Provider: r.Provider, Model: r.Model,
			PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens,
			USDEstimate: r.Usage.USDEstimate,
		}
	}
	return marshalOut(out, "scrape")
}

// verticalDoc renders a zero-LLM vertical hit: the typed record plus the
// extractor name, no LLM usage block (nothing was billed).
func verticalDoc(r scrape.Result) (string, error) {
	return marshalOut(struct {
		URL      string         `json:"url"`
		FinalURL string         `json:"final_url"`
		Title    string         `json:"title"`
		Vertical string         `json:"vertical"`
		Record   map[string]any `json:"record"`
	}{r.URL, r.FinalURL, r.Title, r.Vertical, r.Record}, "scrape")
}

// screenshotDoc renders the screenshot output when --out is absent: the
// base64 PNG in content, mirroring the MCP envelope (raw bytes only
// ever travel via --out).
func screenshotDoc(r scrape.Result) (string, error) {
	return marshalOut(struct {
		URL      string `json:"url"`
		FinalURL string `json:"final_url"`
		Format   string `json:"page_format"`
		Content  string `json:"content"`
	}{r.URL, r.FinalURL, "screenshot", r.Rendered}, "scrape")
}

func orEmpty(r json.RawMessage) json.RawMessage {
	if len(r) == 0 {
		return json.RawMessage("null")
	}
	return r
}
