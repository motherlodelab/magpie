// Package scrape is the shared single-URL flow: fetch → clean →
// extract + selector-cache apply. Both the CLI `scrape` command and the
// MCP `scrape_url` tool call Run; output formatting stays in the CLI.
package scrape

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"
	"github.com/motherlodelab/magpie/vertical"
)

// ErrMissingKey marks a schema extraction without credentials (CLI maps to exit 7).
var ErrMissingKey = errors.New("missing API key")

// Deps injects CLI-owned constructors so scrape never imports the CLI
// package (which would cycle once serve lives there).
// Deps must be safe for concurrent use: Batch fans one Deps out over N
// goroutines sharing DB and the constructors. Nil-schema (markdown-only)
// runs never call ExtractorFor; *store.DB handles concurrent readers.
type Deps struct {
	DB           *store.DB
	ExtractorFor func(provider, key, model string, sch *extract.Schema, runID string) (extract.Extractor, error)
	APIKeyFor    func(provider string) string
	// Fetcher serves raw-URL fetches and vertical sub-fetches; nil =
	// NewStaticFetcher() (production default; tests inject a fake).
	Fetcher vertical.Fetcher
}

// Options configures one scrape. A nil Schema means markdown-only (no LLM).
type Options struct {
	Schema     *extract.Schema
	Render     string // auto|static|browser ("" = auto)
	Provider   string
	Model      string
	MaxCost    float64
	UseCache   bool
	PageFormat string // markdown|llm|text|json|html|raw|screenshot ("" = markdown)
	Scope      clean.Scope
	Profile    string
	Browser    string // TLS fingerprint: chrome|firefox|safari|edge|ios|chrome_android|random ("" = stock)
	Cookies    string
	// Viewport is "WxH" (e.g. 1280x800) for page-format screenshot.
	Viewport string
	// Actions are browser action lines (fetch.ParseActions grammar); they
	// force the browser path and reject render=static. Validated here
	// pre-I/O (parse errors surface as OptionsError with verb + line).
	Actions []string
	// Lang is the verbatim Accept-Language header value (control chars
	// rejected — header-injection boundary).
	Lang string
	// Headers are raw "Name: value" request headers for the static
	// path, merged after the profile bundle (user wins). Shape and
	// control characters are validated here pre-I/O (OptionsError).
	Headers []string
	// Webhook URL for watch change notifications (operator-chosen
	// endpoint; the POST client is scoped AllowPrivate).
	Webhook string
	// Vertical selects a zero-LLM typed extractor: "" (default) = off,
	// "auto" = strict auto-dispatch, or an extractor name for explicit
	// selection. Explicit selection with a schema ignores the schema.
	Vertical string
	// CaptureXHR lists Go regexps for XHR/fetch response capture (rod
	// only; render=static rejected at this boundary). Validated here.
	CaptureXHR []string
	// CDP is a remote browser endpoint (ws://, wss://, http(s)://);
	// MAGPIE_CDP_URL is the env fallback (flag wins). Scheme validated
	// here pre-I/O; empty = launch locally as always.
	CDP string
	// Proxy is a per-run egress override (pool-line grammar) that beats
	// the env pool for this run's fetches: main page (static + browser),
	// screenshot captures, and extractor sub-fetches (verticalFetcher
	// injects it — extractors build their own FetchRequest and can't see
	// run options). Validated here pre-I/O (OptionsError, ErrProxyConfig
	// family).
	Proxy string
}

// Result is one scraped page.
type Result struct {
	RunID          string
	URL            string
	FinalURL       string
	Title          string
	Markdown       string          `json:",omitempty"`
	StructuredData json.RawMessage `json:",omitempty"`
	Record         map[string]any  `json:",omitempty"`
	FromCache      bool
	Usage          extract.TokenUsage
	Provider       string
	Model          string
	Rendered       string
	// Vertical names the extractor that produced Record ("" when unused).
	Vertical string `json:",omitempty"`
	// ScreenshotPNG carries raw PNG bytes for page-format screenshot;
	// never JSON-serialized (Rendered carries the base64 form).
	ScreenshotPNG []byte `json:"-"`
	// XHR carries captured XHR/fetch bodies (additive-omitempty; nil
	// unless --capture-xhr matched something).
	XHR []fetch.XHRCapture `json:",omitempty"`
}

// OptionsError marks a pre-I/O options validation failure (CLI exit 2).
// Callers match it with errors.As; the message text is API surface
// (crosses the MCP boundary to agents) — change only with a declared
// message change.
type OptionsError struct{ msg string }

func (e *OptionsError) Error() string { return e.msg }

// ValidateOptions checks Render, PageFormat, Browser, and Vertical before
// any I/O. One switch site, shared by Run (which calls it first), the CLI
// commands, and batch — adding an option edits this function, not four
// validators. The trust boundary stays in this package: callers map the
// typed error (exit 2) but never re-implement the checks. CLI commands
// may call it with a partial Options (browser-only pre-flight in
// crawl/batch) — every check must stay optional-field-only, or those
// subset calls break.
func ValidateOptions(o Options) error {
	render := defaultRender(o.Render)
	switch render {
	case "auto", "static", "browser":
	default:
		return &OptionsError{fmt.Sprintf("scrape: render %q must be auto|static|browser", render)}
	}
	switch o.PageFormat {
	case "", "markdown", "llm", "text", "json", "html", "raw", "screenshot":
	default:
		return &OptionsError{fmt.Sprintf("scrape: page format %q must be markdown|llm|text|json|html|raw|screenshot", o.PageFormat)}
	}
	if o.PageFormat == "screenshot" && render == "static" {
		return &OptionsError{"scrape: page format \"screenshot\" requires browser rendering (render auto|browser, not static)"}
	}
	if len(o.CaptureXHR) > 0 {
		if render == "static" {
			return &OptionsError{"scrape: capture-xhr requires browser rendering (render auto|browser, not static)"}
		}
		if _, err := fetch.ValidateXHRPatterns(o.CaptureXHR); err != nil {
			return &OptionsError{fmt.Sprintf("scrape: %v", err)}
		}
	}
	if cdp := resolveCDP(o); cdp != "" {
		u, err := url.Parse(cdp)
		switch {
		case err != nil || u.Host == "":
			return &OptionsError{fmt.Sprintf("scrape: cdp-url must be an absolute ws://, wss://, or http(s):// endpoint (got %s)", fetch.RedactCDP(cdp))}
		case u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "http" && u.Scheme != "https":
			return &OptionsError{fmt.Sprintf("scrape: cdp-url scheme %q must be ws, wss, http, or https (%s)", u.Scheme, fetch.RedactCDP(cdp))}
		}
	}
	if o.Viewport != "" {
		if w, h, err := parseViewport(o.Viewport); err != nil || w <= 0 || h <= 0 {
			return &OptionsError{fmt.Sprintf("scrape: viewport %q must be WxH (e.g. 1280x800)", o.Viewport)}
		}
	}
	if !fetch.ValidBrowser(o.Browser) {
		return &OptionsError{fmt.Sprintf("scrape: browser %q must be %s", o.Browser, fetch.BrowserHelp)}
	}
	if o.Vertical != "" && o.Vertical != "auto" {
		if _, ok := vertical.Lookup(o.Vertical); !ok {
			return &OptionsError{fmt.Sprintf("scrape: vertical %q unknown (see `magpie vertical --list`)", o.Vertical)}
		}
	}
	if len(o.Actions) > 0 {
		if render == "static" {
			return &OptionsError{"scrape: actions require browser rendering (render auto|browser, not static)"}
		}
		if _, err := fetch.ParseActions(o.Actions); err != nil {
			return &OptionsError{fmt.Sprintf("scrape: %v", err)}
		}
	}
	if strings.ContainsFunc(o.Lang, unicode.IsControl) {
		return &OptionsError{"scrape: lang must not contain control characters (it becomes a raw Accept-Language header value)"}
	}
	for i, h := range o.Headers {
		if strings.ContainsFunc(h, unicode.IsControl) {
			return &OptionsError{fmt.Sprintf("scrape: header %d must not contain control characters (it becomes a raw request header line)", i+1)}
		}
		name, _, ok := strings.Cut(h, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return &OptionsError{fmt.Sprintf("scrape: header %d must be \"Name: value\"", i+1)}
		}
	}
	if o.Proxy != "" {
		if _, err := fetch.ValidateRequestProxy(o.Proxy); err != nil {
			return &OptionsError{fmt.Sprintf("scrape: %v", err)}
		}
	}
	return nil
}

// defaultRender is the single home for the Render empty→"auto" default,
// shared by ValidateOptions and Run's dispatch.
func defaultRender(r string) string {
	if r == "" {
		return "auto"
	}
	return r
}

// resolveCDP returns the CDP endpoint: the explicit option, else the
// MAGPIE_CDP_URL env fallback (flag > env, the MAGPIE_PROXY pattern).
func resolveCDP(o Options) string {
	if o.CDP != "" {
		return o.CDP
	}
	return os.Getenv("MAGPIE_CDP_URL")
}

// Run fetches, cleans, and optionally extracts one URL.
func Run(ctx context.Context, d Deps, rawURL string, o Options) (Result, error) {
	if d.DB == nil {
		return Result{}, fmt.Errorf("scrape: nil DB")
	}
	o.CDP = resolveCDP(o) // resolve before validation so the scheme check sees the env fallback
	if err := ValidateOptions(o); err != nil {
		return Result{}, err
	}
	// ValidateOptions guaranteed the name; resolve the extractor for dispatch.
	var explicit *vertical.Extractor
	if o.Vertical != "" && o.Vertical != "auto" {
		ex, _ := vertical.Lookup(o.Vertical)
		explicit = &ex
	}

	runID := store.NewRunID()
	if err := d.DB.BeginRun(runID, "scrape"); err != nil {
		return Result{}, err
	}
	finish := func(ok, er int, status string) {
		if err := d.DB.FinishRun(runID, ok, er, status); err != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", err)
		}
	}
	// One deferred finish owns the run row: return paths below only flip
	// the counters. (0,1,"error") is the default; success paths set
	// (1,0,"finished"); missing-key and cost-ceiling aborts zero the error
	// count (er=0) — config errors, not page errors.
	ok, er, status := 0, 1, "error"
	defer func() { finish(ok, er, status) }()
	verticalDone := func(res Result, verr error) (Result, error) {
		if verr == nil {
			ok, er, status = 1, 0, "finished"
		}
		return res, verr
	}

	// Screenshot is a browser-only capability: no static fetch, no clean,
	// no LLM. Fresh browser per capture — same pattern as fetchBrowser.
	// With actions, the capture joins that browser session (click-then-
	// capture: the screenshot becomes the final action step).
	if o.PageFormat == "screenshot" {
		png, serr := screenshotPage(ctx, rawURL, o)
		if serr != nil {
			return Result{}, serr
		}
		ok, er, status = 1, 0, "finished"
		return Result{RunID: runID, URL: rawURL, FinalURL: rawURL,
			Rendered: base64.StdEncoding.EncodeToString(png), ScreenshotPNG: png}, nil
	}

	var vf = d.Fetcher
	if vf == nil {
		static, serr := fetch.NewStaticFetcher()
		if serr != nil {
			return Result{}, serr
		}
		vf = static
	}
	fetchStart := time.Now()
	render := defaultRender(o.Render) // same default ValidateOptions validated

	// Vertical dispatch is decided from the URL alone, BEFORE any fetch:
	// extractors fetch their own targets (watch pages, JSON APIs), so the
	// main page fetch is result context for them, never a gate. A blocked
	// or JS-shell main page must not kill a vertical run (youtube's
	// consent shell used to die in Clean before dispatch ever ran).
	var dispatch *vertical.Extractor
	if explicit != nil {
		// ^ --list ships in Phase C; the message names it anyway so the string never changes.
		if u, err := url.Parse(rawURL); err != nil || !explicit.Match(u) {
			return Result{}, fmt.Errorf("scrape: vertical %q: %w for %s", o.Vertical, vertical.ErrURLMismatch, rawURL)
		}
		dispatch = explicit
	} else if o.Vertical == "auto" {
		if ex, ok := vertical.MatchURL(rawURL); ok {
			dispatch = &ex
		}
	}

	// The wrapper rides every vertical fetch; the browser func only fires
	// on a typed challenge from an extractor's sub-fetch.
	vfetch := verticalFetcher{static: vf, o: o}
	vbase := Result{RunID: runID, URL: rawURL}

	page, err := fetchURL(ctx, vf, rawURL, render, o)
	if err != nil {
		if dispatch == nil {
			return Result{}, err
		}
		return verticalDone(runVertical(ctx, vfetch, rawURL, *dispatch, vbase))
	}
	// Fetch telemetry rides the run row next to LLM usage; warn-only,
	// never fails the page. The proxy entry that served (redacted
	// host:port) lands in run_history.proxy.
	if lerr := d.DB.LogFetch(runID, int64(len(page.HTML)), time.Since(fetchStart).Milliseconds()); lerr != nil {
		fmt.Fprintf(os.Stderr, "warning: log fetch: %v\n", lerr)
	}
	if page.Proxy != "" {
		if perr := d.DB.SetRunProxy(runID, page.Proxy); perr != nil {
			fmt.Fprintf(os.Stderr, "warning: record proxy: %v\n", perr)
		}
	}

	cleaned, err := clean.Clean(ctx, clean.RawPage{
		HTML: page.HTML, URL: page.URL, FinalURL: page.FinalURL,
		Scope: o.Scope, StatusCode: page.StatusCode,
		ContentType: page.Headers.Get("Content-Type"),
	})
	if err != nil || cleaned.Quality != clean.IssueNone {
		if dispatch != nil {
			return verticalDone(runVertical(ctx, vfetch, rawURL, *dispatch, vbase))
		}
		if err != nil {
			return Result{}, err
		}
		return Result{}, &clean.QualityError{Issue: cleaned.Quality, URL: rawURL}
	}
	base := Result{RunID: runID, URL: page.URL, FinalURL: cleaned.FinalURL, Title: cleaned.Title,
		Markdown: cleaned.Markdown, StructuredData: cleaned.StructuredData, XHR: page.XHR}
	if o.PageFormat == "raw" {
		// The decoded response body, untouched (quality gate above still
		// classified it — challenge raw is a typed error, never bytes).
		base.Rendered = string(page.HTML)
	} else {
		rendered, rerr := clean.Render(cleaned, o.PageFormat)
		if rerr != nil {
			return Result{}, rerr
		}
		base.Rendered = rendered
	}

	if dispatch != nil {
		return verticalDone(runVertical(ctx, vfetch, rawURL, *dispatch, base))
	}

	// No schema → markdown only, no LLM.
	if o.Schema == nil {
		ok, er, status = 1, 0, "finished"
		return base, nil
	}

	provider := o.Provider
	model := o.Model
	key := ""
	if d.APIKeyFor != nil {
		key = d.APIKeyFor(provider)
	}
	if key == "" && extract.NeedsAPIKey(provider) {
		er = 0 // nothing was attempted — config error, not a page error
		return Result{}, fmt.Errorf("scrape: provider %s: %w", provider, ErrMissingKey)
	}
	ex, err := d.ExtractorFor(provider, key, model, o.Schema, runID)
	if err != nil {
		return Result{}, err
	}

	// Selector cache: hit + all required fields non-null → 0 LLM calls.
	if o.UseCache {
		if doc, found, gerr := d.DB.GetSelectors(domainOfURL(cleaned.FinalURL), selector.SchemaHash(o.Schema)); gerr == nil && found {
			if rec, nulls := selectorApply(doc, o.Schema, page.HTML, cleaned.StructuredData); len(nulls) == 0 {
				ok, er, status = 1, 0, "finished"
				base.Record = rec
				base.FromCache = true
				return base, nil
			}
		}
	}

	promptText := "Extract structured data.\n" + string(cleaned.StructuredData) + "\n" + cleaned.Markdown
	if err := CheckCostCeiling(d.DB, runID, provider, model, promptText, o.MaxCost); err != nil {
		er = 0 // abort before any spend — not a page error
		return Result{}, fmt.Errorf("scrape: %w", err)
	}

	res, err := ex.Extract(ctx, extract.ExtractInput{
		Markdown: cleaned.Markdown, StructuredData: cleaned.StructuredData, Schema: o.Schema,
	})
	if err != nil {
		return Result{}, err
	}
	ok, er, status = 1, 0, "finished"
	base.Record = res.Record
	base.Usage = res.Usage
	base.Provider = res.Provider
	base.Model = res.Model
	return base, nil
}

func fetchURL(ctx context.Context, vf vertical.Fetcher, rawURL, render string, o Options) (*fetch.FetchResponse, error) {
	// Actions force the browser path: their whole point is DOM interaction
	// a static fetch cannot honor (ValidateOptions rejected static).
	if render == "browser" || len(o.Actions) > 0 || len(o.CaptureXHR) > 0 {
		return fetchBrowserChecked(ctx, rawURL, o)
	}
	// file:// never auto-escalates to the browser: a local file that failed
	// or parsed tiny stays that way — Chrome cannot make it appear, and the
	// attempt costs a launch (plus a 150MB chromium download on a cold box).
	isFile := false
	if u, perr := url.Parse(rawURL); perr == nil {
		isFile = u.Scheme == "file"
	}
	// A4 pass-through: every status reaches Clean+Classify so blocked pages
	// get typed quality errors instead of "fetch: HTTP %d".
	resp, err := vf.Fetch(ctx, fetch.FetchRequest{URL: rawURL, Profile: o.Profile, Cookies: o.Cookies, Browser: o.Browser, Lang: o.Lang, Headers: o.Headers, Proxy: o.Proxy})
	if err != nil {
		if isFile {
			return nil, err
		}
		// G.2: a typed challenge gets exactly one rod escalation attempt
		// under render=auto (a real browser often clears it); static
		// callers asked for no browser and get the typed error directly.
		var ce *fetch.ChallengeError
		if render != "static" && errors.As(err, &ce) {
			bresp, berr := fetchBrowserChecked(ctx, rawURL, o)
			if berr == nil {
				return bresp, nil
			}
			// Header-authoritative static detection wins over the
			// browser-side vendor, and launch noise never masks it.
			return nil, ce
		}
		return nil, err
	}
	if render == "static" {
		return resp, nil
	}
	score, embedded := fetch.ScoreJSRequired(resp.HTML, resp.Headers)
	if isFile || embedded || !fetch.NeedsBrowser(score) {
		return resp, nil
	}
	return fetchBrowser(ctx, rawURL, o)
}

func domainOfURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "file"
	}
	return strings.ToLower(u.Host)
}

func selectorApply(docJSON string, sch *extract.Schema, html []byte, sidecar json.RawMessage) (map[string]any, []string) {
	var doc selector.SelectorDoc
	if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
		return nil, []string{"*"}
	}
	return selector.NewApplier(doc, sch).Apply(string(html), sidecar)
}

// CheckCostCeiling reuses crawl.ErrCostCeiling — no second sentinel.
// Exported so the CLI (crawl/extract/heal) and the desktop app share this
// one guard and the fail-closed semantics can't drift between surfaces.
// The message carries no package prefix — callers add context when wrapping.
func CheckCostCeiling(db *store.DB, runID, provider, model, promptText string, maxCost float64) error {
	if maxCost <= 0 {
		return nil
	}
	if extract.IsFlatRateProvider(provider) {
		return nil
	}
	running, err := db.RunCost(runID)
	if err != nil {
		return err
	}
	proj := extract.ProjectedCostWithFallback(model, promptText)
	if running+proj > maxCost {
		return fmt.Errorf("running %.6f + projected %.6f > max %.6f: %w", running, proj, maxCost, crawl.ErrCostCeiling)
	}
	return nil
}
