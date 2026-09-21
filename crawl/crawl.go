package crawl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/core"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"
)

// ErrRobotsBlocked marks a crawl refused by robots.txt (deny or unreachable).
var ErrRobotsBlocked = errors.New("robots.txt disallows crawl")

// ErrCostCeiling marks a crawl aborted before exceeding --max-cost.
var ErrCostCeiling = errors.New("cost ceiling exceeded")

// ErrSitemapOnlyEmpty marks a --sitemap-only run whose sitemap expansion
// produced zero in-scope URLs. Mapped to exit 3 (all pages failed) —
// never a silent 0-page success.
var ErrSitemapOnlyEmpty = errors.New("sitemap-only: no URLs found")

// ValidateSitemapOnly rejects the contradictory pair (--sitemap-only has
// no seed to fall back to, so --no-sitemap would make it a no-op). Called
// at both edges: CLI (exit 2 via fail) and MCP crawl_site (tool error).
func ValidateSitemapOnly(sitemapOnly, noSitemap bool) error {
	if sitemapOnly && noSitemap {
		return errors.New("crawl: --sitemap-only and --no-sitemap are contradictory (sitemap-only has no seed to fall back to)")
	}
	return nil
}

// Options configures one crawl run.
type Options struct {
	SeedURL      string
	Schema       *extract.Schema
	Corpus       bool // schema-less corpus mode: emit {url,title,depth,markdown} per page; jsonl only; zero LLM calls
	MaxPages     int
	MaxDepth     int
	SameHost     bool
	FetchWorkers int
	Rate         float64 // per-host rps; <=0 = 1
	Format       string  // jsonl|json|csv|sqlite
	Out          string
	RunID        string // fresh id (cmd-generated); ignored when Resume is set
	Resume       bool
	ResumeID     string // id to resume (set iff Resume)
	IgnoreRobots bool
	Provider     string
	Model        string
	MaxCost      float64
	Browser      string // TLS fingerprint: chrome|firefox|random ("" = stock)
	Lang         string // verbatim Accept-Language override (control chars rejected by scrape.ValidateOptions)
	// Proxy is a per-run egress override (pool-line grammar) for every
	// page fetch (static + rod escalation); beats the env pool. Validated
	// pre-I/O by the caller via fetch.ValidateRequestProxy.
	Proxy string
	DB    *store.DB
	// Scope bounds (compiled once in Run; bad globs fail pre-I/O).
	PathPrefix      string
	Include         []string
	Exclude         []string
	AllowSubdomains bool
	NoSitemap       bool // skip sitemap seed expansion
	// SitemapOnly makes the frontier exactly the (scope-filtered) sitemap
	// URLs: no seed enqueue, no link-following. Contradicts NoSitemap.
	SitemapOnly bool
	// AutoThrottle enables adaptive per-host pacing: Report backs off ×2
	// on 429/5xx, decays on success (opt-in — Scrapy ships it off too).
	AutoThrottle bool
	// Progress is called once per done/errored page from the sink goroutine
	// (nil = off). Total is intentionally omitted: the frontier total is
	// unknowable upfront.
	Progress func(done int)
	// OnRecord receives each extracted record at the sink (nil = off).
	// The MCP crawl_site handler captures records through it.
	OnRecord func(map[string]any)
	// Extractor serves per-page LLM (cold start + non-cacheable + heal).
	Extractor extract.Extractor
	// Propose is the LLM-proposal step inside synthesis (nil = free paths only).
	Propose selector.ProposeFunc
}

// Result summarizes a finished crawl.
type Result struct {
	RunID    string
	PagesOK  int
	PagesErr int
	Records  int
}

// domainState holds per-domain cache/heal runtime. mu serializes the
// extract workers sharing a domain (doc map + healer + flush counter).
type domainState struct {
	mu          sync.Mutex
	doc         selector.SelectorDoc
	loaded      bool
	healer      *selector.Healer
	cachedPages int // pages served from cache since last null-rate flush
}

// crawlContext carries the run-wide collaborators shared by Run's phases
// (setup, seeding, pipeline stages, heal, finish) so helpers take one
// receiver instead of a parameter list. Pure refactor of the former
// ~500-line Run — no behavior change.
type crawlContext struct {
	db         *store.DB
	opts       Options
	runID      string
	maxPages   int
	maxDepth   int
	format     string
	scope      Scope
	schemaHash string
	required   []string
	filter     *Filter
	checker    *Checker
	limiters   *HostLimiters
	gate       *core.BrowserGate
	frontier   *Frontier
	static     *fetch.StaticFetcher

	outstanding atomic.Int64

	// Per-domain cache/heal state, shared by the extract workers and the
	// final null-rate flush.
	domains   map[string]*domainState
	domainsMu sync.Mutex
}

// Run executes a crawl: seed/resume → pump + pipeline → writer → FinishRun.
func Run(ctx context.Context, opts Options) (Result, error) {
	cc, err := newCrawlContext(opts)
	if err != nil {
		return Result{}, err
	}
	if err := cc.begin(); err != nil {
		return Result{}, err
	}
	if err := cc.seed(ctx); err != nil {
		return Result{}, err
	}
	return cc.runPipeline(ctx)
}

// ValidateCorpus rejects an impossible corpus/format pair. Shared by the
// CLI edge (exit 2) and newCrawlContext (Options error) so the rule can't
// drift between them.
func ValidateCorpus(corpus bool, format string) error {
	if corpus && format != "" && format != "jsonl" {
		return fmt.Errorf("crawl: corpus mode requires --format jsonl")
	}
	return nil
}

// newCrawlContext validates options and derives the run-wide defaults:
// maxPages/maxDepth/format, compiled scope, schema hash, and run ID.
func newCrawlContext(opts Options) (*crawlContext, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("crawl: nil DB")
	}
	if opts.Schema == nil && !opts.Corpus {
		return nil, fmt.Errorf("crawl: nil schema")
	}
	if opts.Extractor == nil && !opts.Corpus {
		return nil, fmt.Errorf("crawl: nil extractor")
	}
	if err := ValidateCorpus(opts.Corpus, opts.Format); err != nil {
		return nil, err
	}
	cc := &crawlContext{db: opts.DB, opts: opts}
	cc.maxPages = opts.MaxPages
	if cc.maxPages <= 0 {
		cc.maxPages = 100
	}
	// maxDepth <= 0 = unset → default 3; explicit seed-only passes -1.
	cc.maxDepth = opts.MaxDepth
	if cc.maxDepth <= 0 {
		cc.maxDepth = 3
	}
	if opts.MaxDepth < 0 {
		cc.maxDepth = 0
	}
	cc.format = opts.Format
	if cc.format == "" {
		cc.format = "jsonl"
	}
	// Corpus mode has no schema: the selector-cache reads that would consult
	// the hash are skipped, so the hash is never used. SchemaHash would panic
	// on nil (sch.Raw), so both derivations stay schema-guarded.
	if opts.Schema != nil {
		cc.schemaHash = selector.SchemaHash(opts.Schema)
		cc.required = requiredFields(opts.Schema)
	}
	// Compile the frontier scope once; a bad glob fails before any I/O.
	scope, err := CompileScope(opts.SameHost, opts.AllowSubdomains, opts.PathPrefix, opts.Include, opts.Exclude)
	if err != nil {
		return nil, err
	}
	cc.scope = scope
	cc.runID = opts.RunID
	if cc.runID == "" {
		cc.runID = newRunID()
	}
	if opts.Resume && opts.ResumeID != "" {
		cc.runID = opts.ResumeID
	}
	// Static fetcher for the pipeline (and sitemap expansion in seed);
	// created in setup so Run reads as pure phase calls.
	if cc.static, err = fetch.NewStaticFetcher(); err != nil {
		return nil, err
	}
	return cc, nil
}

// begin opens the run row (or resumes it) and rebuilds resume state:
// inflight reset, visited-hash reload, outstanding count. Former Run
// middle section, verbatim.
func (c *crawlContext) begin() error {
	c.filter = NewFilter()
	if !c.opts.Resume {
		if err := c.db.BeginRun(c.runID, "crawl"); err != nil {
			return err
		}
	} else {
		if err := c.db.ResumeRun(c.runID); err != nil {
			return err
		}
	}

	// ponytail: the robots Checker rides the env pool, not opts.Proxy —
	// threading the per-run override needs an exported fetch ctx helper.
	// Known gap: a proxy-only site mislabels its egress failure as a
	// robots refusal (checker.Allowed err → ErrRobotsBlocked). Documented
	// in CORE-HANDOFF §1; revisit if a caller actually sets per-run
	// proxy on crawls (desktop L-phase or a CLI --proxy flag).
	c.checker = NewChecker()
	c.limiters = NewHostLimiters(c.opts.Rate, 3)
	if c.opts.AutoThrottle {
		c.limiters.SetAuto()
	}

	if c.opts.Resume {
		if _, err := c.db.ResetInflight(c.runID); err != nil {
			return err
		}
		hashes, err := c.db.LoadHashes(c.runID)
		if err != nil {
			return err
		}
		for _, h := range hashes {
			c.filter.Add(h)
		}
		if p, _, _, _, err := c.db.CrawlStats(c.runID); err == nil {
			c.outstanding.Add(int64(p))
		}
	}
	c.frontier = NewFrontier(c.db, c.filter, c.runID)
	return nil
}

// seed enqueues the seed (robots-checked first) plus sitemap expansion on
// fresh runs; resume runs start from the persisted frontier instead.
// Former Run seed block, verbatim.
func (c *crawlContext) seed(ctx context.Context) error {
	if c.opts.Resume {
		return nil
	}
	seedHost, herr := hostOf(c.opts.SeedURL)
	if herr != nil {
		seedHost = c.opts.SeedURL
	}
	if isHTTP(c.opts.SeedURL) && !c.opts.IgnoreRobots {
		allowed, err := c.checker.Allowed(ctx, c.opts.SeedURL)
		if err != nil || !allowed {
			if ferr := c.db.FinishRun(c.runID, 0, 0, "robots_blocked"); ferr != nil {
				fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
			}
			if err != nil && errors.Is(err, ErrRobotsUnreachable) {
				return fmt.Errorf("crawl: seed %s: %w", seedHost, ErrRobotsBlocked)
			}
			return fmt.Errorf("crawl: seed %s: %w", seedHost, ErrRobotsBlocked)
		}
	}
	seeds := []string{c.opts.SeedURL}
	if isHTTP(c.opts.SeedURL) && !c.opts.NoSitemap {
		// Expand the seed through the sitemap (page URLs, capped at
		// maxPages) instead of enqueuing raw sitemap-XML URLs. In normal
		// mode expansion NEVER fails the crawl: warn and proceed seed-only.
		// In sitemap-only mode the expansion IS the frontier, so an
		// expansion error is fatal (there is no seed-only fallback — the
		// seed URL is explicitly excluded from the contract).
		expanded, _, serr := ListSitemapURLs(ctx, c.static, c.opts.SeedURL)
		if serr != nil {
			if c.opts.SitemapOnly {
				return fmt.Errorf("crawl: sitemap-only: %w", serr)
			}
			fmt.Fprintf(os.Stderr, "crawl: sitemap expansion: %v; continuing seed-only\n", serr)
		} else {
			if len(expanded) > c.maxPages {
				expanded = expanded[:c.maxPages]
			}
			filtered := scopeFilterExpansion(c.scope, c.opts.SeedURL, expanded)
			if c.opts.SitemapOnly {
				seeds = filtered // seed itself excluded unless the sitemap lists it
				if len(seeds) == 0 {
					return fmt.Errorf("crawl: %w", ErrSitemapOnlyEmpty)
				}
			} else {
				seeds = append(seeds, filtered...)
			}
		}
	}
	n, err := c.frontier.Add(seeds, 0)
	if err != nil {
		return err
	}
	c.outstanding.Add(int64(n))
	return nil
}

// runPipeline wires the fetch/clean/extract stages, pumps the frontier
// into core.Run, and delegates result handling to finish. Former Run
// stage wiring, verbatim.
func (c *crawlContext) runPipeline(ctx context.Context) (Result, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	source := make(chan core.FetchTask, 1000)
	var pumpErr atomic.Value
	go func() {
		defer close(source)
		claimed := 0
		for {
			if runCtx.Err() != nil {
				return
			}
			if claimed >= c.maxPages {
				return
			}
			batch := c.maxPages - claimed
			if batch > 32 {
				batch = 32
			}
			rows, err := c.db.Claim(c.runID, batch)
			if err != nil {
				pumpErr.Store(err)
				return
			}
			if len(rows) == 0 {
				if c.outstanding.Load() == 0 {
					return
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			for _, r := range rows {
				select {
				case source <- core.FetchTask{URL: r.URL, URLHash: r.URLHash, Depth: r.Depth}:
					claimed++
				case <-runCtx.Done():
					return
				}
			}
		}
	}()

	c.gate = core.NewBrowserGate(2)
	c.domains = map[string]*domainState{}

	w, err := newWriter(c.opts.Out, c.format, c.opts.Schema, c.db, c.runID, c.opts.Corpus)
	if err != nil {
		return Result{}, err
	}
	var sinkErr error
	doneCount := 0
	sink := func(r core.PageResult) {
		if sinkErr != nil {
			return
		}
		if r.Err != nil {
			if merr := c.db.MarkError(c.runID, hashTask(r.Task), r.Err.Error()); merr != nil {
				fmt.Fprintf(os.Stderr, "warning: mark error: %v\n", merr)
			}
		} else {
			if werr := w.write(r); werr != nil {
				sinkErr = werr
				cancel()
				if merr := c.db.MarkError(c.runID, hashTask(r.Task), werr.Error()); merr != nil {
					fmt.Fprintf(os.Stderr, "warning: mark error: %v\n", merr)
				}
			} else if merr := c.db.MarkDone(c.runID, hashTask(r.Task)); merr != nil {
				fmt.Fprintf(os.Stderr, "warning: mark done: %v\n", merr)
			} else if c.opts.OnRecord != nil {
				c.opts.OnRecord(w.record(r))
			}
		}
		c.outstanding.Add(-1)
		doneCount++
		if c.opts.Progress != nil {
			c.opts.Progress(doneCount)
		}
	}

	cfg := core.DefaultPipelineConfig()
	if c.opts.FetchWorkers > 0 {
		cfg.FetchWorkers = c.opts.FetchWorkers
	}
	runErr := core.Run(runCtx, cfg, source, c.fetchPage, c.cleanPage, c.extractPage, sink)
	perr, _ := pumpErr.Load().(error) // nil when the pump never failed
	return c.finish(ctx, w, sinkErr, perr, runErr)
}

// finish flushes per-domain null rates, closes the writer, and records
// the terminal run status (error / interrupted / finished). Former Run
// result-handling tail, verbatim.
func (c *crawlContext) finish(ctx context.Context, w *writer, sinkErr error, pumpErr error, runErr error) (Result, error) {
	// Final null-rate flush per domain.
	c.domainsMu.Lock()
	for domain, st := range c.domains {
		st.mu.Lock()
		if st.loaded && len(st.doc.Fields) > 0 {
			c.flushNullRates(st, domain)
		}
		st.mu.Unlock()
	}
	c.domainsMu.Unlock()

	if werr := w.close(); werr != nil && sinkErr == nil {
		sinkErr = werr
	}
	finishWarn := func(status string) {
		if ferr := c.db.FinishRun(c.runID, 0, 0, status); ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
		}
	}
	if sinkErr != nil {
		finishWarn("error")
		return Result{RunID: c.runID}, sinkErr
	}
	if pumpErr != nil {
		finishWarn("error")
		return Result{RunID: c.runID}, pumpErr
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		finishWarn("error")
		return Result{RunID: c.runID}, runErr
	}

	_, _, done, errs, serr := c.db.CrawlStats(c.runID)
	if serr != nil {
		finishWarn("error")
		return Result{RunID: c.runID}, serr
	}
	status := "finished"
	if ctx.Err() != nil || (runErr != nil && errors.Is(runErr, context.Canceled)) {
		status = "interrupted"
	}
	if ferr := c.db.FinishRun(c.runID, done, errs, status); ferr != nil {
		return Result{RunID: c.runID}, ferr
	}
	return Result{RunID: c.runID, PagesOK: done, PagesErr: errs, Records: w.count()}, nil
}

func (c *crawlContext) fetchPage(ctx context.Context, task core.FetchTask) (core.FetchedPage, error) {
	// hostOf's error only ever means "no usable host" (host == ""):
	// unparsable URL or missing host — such tasks skip pacing and get
	// classified below like any other fetch.
	host, _ := hostOf(task.URL) //nolint:errcheck // "" covers both error and empty-host
	if isHTTP(task.URL) && !c.opts.IgnoreRobots {
		allowed, err := c.checker.Allowed(ctx, task.URL)
		if err != nil || !allowed {
			return core.FetchedPage{Task: task, Err: fmt.Errorf("crawl: robots disallow %s", task.URL)}, nil
		}
		if d := c.checker.CrawlDelay(ctx, task.URL); d > 0 && host != "" {
			c.limiters.SetFloor(host, d)
		}
	}
	if host != "" {
		if err := c.limiters.Wait(ctx, host); err != nil {
			return core.FetchedPage{}, err
		}
	}
	var last *fetch.FetchResponse
	fetchStart := time.Now()
	resp, err := FetchWithRetry(ctx, func() (*fetch.FetchResponse, error) {
		r, ferr := c.static.Fetch(ctx, fetch.FetchRequest{URL: task.URL, Browser: c.opts.Browser, Lang: c.opts.Lang, Proxy: c.opts.Proxy})
		// Reset on transport failure: qualityErr must classify the
		// terminal outcome, never a stale response from an earlier try.
		last = r
		// AutoThrottle reports PER ATTEMPT, inside the retry closure:
		// backoff.go consumes intermediate 429/503s, so post-hoc wiring
		// would see only final 200s and the backoff would be dead code.
		// The rod-escalation branch below never Reports — AutoThrottle
		// adapts on the static path only (rare escalation path).
		if c.opts.AutoThrottle && r != nil && host != "" {
			ra, _ := retryAfterOf(r.Headers)
			c.limiters.Report(host, r.StatusCode, ra)
		}
		return r, ferr
	})
	if err != nil {
		// Retry taxonomy (backoff.go) already ran: classify the outcome.
		if qerr := qualityErr(ctx, task, last); qerr != nil {
			return core.FetchedPage{Task: task, Err: qerr}, nil
		}
		return core.FetchedPage{Task: task, Err: err}, nil
	}
	if resp.StatusCode/100 != 2 {
		if qerr := qualityErr(ctx, task, resp); qerr != nil {
			return core.FetchedPage{Task: task, Err: qerr}, nil
		}
		return core.FetchedPage{Task: task, Err: classify(resp, nil)}, nil
	}
	if score, embedded := fetch.ScoreJSRequired(resp.HTML, resp.Headers); !embedded && fetch.NeedsBrowser(score) && isHTTP(task.URL) {
		if err := c.gate.Acquire(ctx); err != nil {
			return core.FetchedPage{}, err
		}
		bresp, berr := func() (*fetch.FetchResponse, error) {
			defer c.gate.Release()
			rod := fetch.NewRodFetcher()
			rod.Proxy = c.opts.Proxy
			defer func() { _ = rod.Close() }() //nolint:errcheck // teardown unactionable
			return rod.Fetch(ctx, fetch.FetchRequest{URL: task.URL, Lang: c.opts.Lang})
		}()
		if berr != nil {
			return core.FetchedPage{Task: task, Err: berr}, nil
		}
		resp = bresp
	}
	// Fetch telemetry rides the run row next to LLM usage. Warn-only:
	// a logging failure must never fail the page.
	if lerr := c.db.LogFetch(c.runID, int64(len(resp.HTML)), time.Since(fetchStart).Milliseconds()); lerr != nil {
		fmt.Fprintf(os.Stderr, "warning: log fetch: %v\n", lerr)
	}
	return core.FetchedPage{Task: task, Resp: resp}, nil
}

func (c *crawlContext) cleanPage(ctx context.Context, page core.FetchedPage) (core.Cleaned, error) {
	if page.Err != nil || page.Resp == nil {
		return core.Cleaned{Task: page.Task, Err: page.Err}, nil
	}
	html := page.Resp.HTML
	finalURL := page.Resp.FinalURL
	if finalURL == "" {
		finalURL = page.Task.URL
	}
	sidecar := clean.HarvestSidecar(html)
	// Link extraction ALWAYS (nav links live in boilerplate) — except
	// sitemap-only runs, whose frontier contract is exactly the sitemap.
	if !c.opts.SitemapOnly {
		if links, err := c.frontier.ExtractLinks(html, finalURL, page.Task.Depth, c.maxDepth, c.scope); err == nil && links > 0 {
			c.outstanding.Add(int64(links))
		}
	}
	// Trafilatura only when the page will need an LLM.
	// ponytail: one SQLite point read per page to decide (ceiling =
	// negligible WAL read on the single conn).
	needLLM := true
	if !c.opts.Corpus {
		if doc, ok, err := c.db.GetSelectors(domainOf(finalURL), c.schemaHash); err == nil && ok {
			needLLM = hasNonCacheable(doc, c.opts.Schema)
		}
	}
	if !needLLM {
		return core.Cleaned{Task: page.Task, Resp: page.Resp,
			Page:    clean.CleanedPage{StructuredData: sidecar, FinalURL: finalURL},
			Sidecar: sidecar}, nil
	}
	cleaned, err := clean.Clean(ctx, clean.RawPage{
		HTML: html, URL: page.Task.URL, FinalURL: finalURL,
		StatusCode: page.Resp.StatusCode, ContentType: page.Resp.Headers.Get("Content-Type"),
	})
	if err != nil {
		return core.Cleaned{Task: page.Task, Err: err}, nil
	}
	return core.Cleaned{Task: page.Task, Resp: page.Resp, Page: cleaned, Sidecar: cleaned.StructuredData}, nil
}

// checkCeiling aborts before LLM spend when running+projected > max.
func (c *crawlContext) checkCeiling(promptText string) error {
	if c.opts.MaxCost <= 0 {
		return nil
	}
	running, err := c.db.RunCost(c.runID)
	if err != nil {
		return err // fail closed
	}
	proj := extract.ProjectedCost(c.opts.Model, promptText)
	if proj == 0 {
		if toks := extract.EstimatePromptTokens(promptText); toks > 0 {
			proj = float64(toks) / 1e6 * 2.00
		}
	}
	if running+proj > c.opts.MaxCost {
		return ErrCostCeiling
	}
	return nil
}

// extractOne runs one cost-checked LLM extraction against the schema.
func (c *crawlContext) extractOne(ctx context.Context, markdown string, sidecar json.RawMessage, purpose, extra string) (extract.ExtractResult, error) {
	if err := c.checkCeiling(markdown + string(sidecar)); err != nil {
		return extract.ExtractResult{}, err
	}
	return c.opts.Extractor.Extract(ctx, extract.ExtractInput{
		Markdown: markdown, StructuredData: sidecar,
		Schema: c.opts.Schema, Purpose: purpose, PromptExtra: extra,
	})
}

func (c *crawlContext) extractPage(ctx context.Context, cl core.Cleaned) (core.PageResult, error) {
	if cl.Err != nil {
		return core.PageResult{Task: cl.Task, Err: cl.Err}, nil
	}
	// Corpus mode: the cleaned markdown IS the record — no selectors, no
	// heal, no domain state.
	if c.opts.Corpus {
		return core.PageResult{Task: cl.Task, Err: cl.Err, Title: cl.Page.Title, Text: cl.Page.Markdown}, nil
	}
	finalURL := cl.Page.FinalURL
	if finalURL == "" && cl.Resp != nil {
		finalURL = cl.Resp.FinalURL
	}
	if finalURL == "" {
		finalURL = cl.Task.URL
	}
	domain := domainOf(finalURL)
	c.domainsMu.Lock()
	st, ok := c.domains[domain]
	if !ok {
		st = &domainState{healer: selector.NewHealer(50, 0.30, 3)}
		c.domains[domain] = st
	}
	c.domainsMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()

	html := ""
	if cl.Resp != nil {
		html = string(cl.Resp.HTML)
	}
	// Load-once per domain per run.
	if !st.loaded {
		if raw, found, err := c.db.GetSelectors(domain, c.schemaHash); err == nil && found {
			var doc selector.SelectorDoc
			if jerr := json.Unmarshal([]byte(raw), &doc); jerr == nil {
				st.doc = doc
			}
		}
		st.loaded = true
	}
	hasDoc := len(st.doc.Fields) > 0

	if hasDoc {
		// Markdown for fill/heal LLM calls (may have skipped trafilatura).
		md := cl.Page.Markdown
		if md == "" && html != "" {
			if cleaned, cerr := clean.Clean(ctx, clean.RawPage{HTML: []byte(html), URL: cl.Task.URL, FinalURL: finalURL}); cerr == nil {
				md = cleaned.Markdown
			}
		}
		applier := selector.NewApplier(st.doc, c.opts.Schema)
		rec, nulls := applier.Apply(html, cl.Sidecar)
		nullSet := map[string]bool{}
		for _, f := range nulls {
			nullSet[f] = true
		}
		var triggered []string
		for _, f := range nulls {
			if trig := st.healer.Observe(f, true); len(trig) > 0 {
				triggered = append(triggered, trig...)
			}
		}
		for f := range st.doc.Fields {
			if !nullSet[f] {
				if trig := st.healer.Observe(f, false); len(trig) > 0 {
					triggered = append(triggered, trig...)
				}
			}
		}
		// Fill non-cacheable fields via per-page LLM.
		if len(nulls) > 0 {
			res, err := c.extractOne(ctx, md, cl.Sidecar, "extract", "")
			if err != nil {
				if errors.Is(err, ErrCostCeiling) {
					return core.PageResult{}, err
				}
				// Degrade: emit cached fields only.
				return core.PageResult{Task: cl.Task, Record: rec}, nil
			}
			for _, f := range nulls {
				if v, present := res.Record[f]; present {
					rec[f] = v
				}
			}
			if validRequired(res.Record, c.required) {
				st.healer.Retain(selector.SynthSample{URL: finalURL, HTML: html, Sidecar: cl.Sidecar, Truth: res.Record})
			}
		}
		if len(triggered) > 0 {
			c.healField(ctx, st, domain, finalURL, html, md, cl.Sidecar, triggered)
		}
		st.cachedPages++
		if st.cachedPages%25 == 0 {
			c.flushNullRates(st, domain)
		}
		return core.PageResult{Task: cl.Task, Record: rec}, nil
	}

	// Cold domain: direct LLM (Purpose synth) + retain + synthesize at N.
	res, err := c.extractOne(ctx, cl.Page.Markdown, cl.Sidecar, "synth", "")
	if err != nil {
		if errors.Is(err, ErrCostCeiling) {
			return core.PageResult{}, err
		}
		return core.PageResult{Task: cl.Task, Err: err}, nil
	}
	if validRequired(res.Record, c.required) {
		st.healer.Retain(selector.SynthSample{URL: finalURL, HTML: html, Sidecar: cl.Sidecar, Truth: res.Record})
		if len(st.healer.Samples()) >= 3 {
			doc, serr := selector.Synthesize(ctx, st.healer.Samples(), c.opts.Schema, c.opts.Propose, nil)
			if serr == nil {
				doc.Domain = domain
				if raw, merr := json.Marshal(doc); merr == nil {
					if perr := c.db.PutSelectors(domain, c.schemaHash, string(raw), doc.SamplesUsed); perr != nil {
						fmt.Fprintf(os.Stderr, "warning: cache selectors: %v\n", perr)
					} else {
						st.doc = doc
					}
				}
			}
		}
	}
	return core.PageResult{Task: cl.Task, Record: res.Record}, nil
}

// scopeFilterExpansion keeps only in-scope sitemap-expanded URLs (the
// seed itself is always kept by the caller — the operator chose it).
// Sitemaps list the whole site, so without this filter --path-prefix
// would burn the --max-pages budget on out-of-scope URLs.
func scopeFilterExpansion(scope Scope, seedURL string, expanded []string) []string {
	seedU, err := url.Parse(seedURL)
	if err != nil {
		return nil
	}
	var out []string
	for _, raw := range expanded {
		c, cerr := Canonicalize(raw)
		if cerr != nil {
			continue
		}
		cu, perr := url.Parse(c)
		if perr != nil {
			continue
		}
		if scope.Allows(seedU, cu) {
			out = append(out, c)
		}
	}
	return out
}

// healField re-synthesizes triggered fields only (full template when ≥50%
// broken), merging into the live doc so healthy fields serve throughout.
// healField re-synthesizes triggered fields only (full template when ≥50%
// broken), merging into the live doc so healthy fields serve throughout.
func (c *crawlContext) healField(ctx context.Context, st *domainState, domain, pageURL, html, md string, sidecar json.RawMessage, triggered []string) {
	// Fresh ground truth on the current (new-template) page.
	if res, err := c.extractOne(ctx, md, sidecar, "synth", "heal sample: "+pageURL); err == nil {
		if validRequired(res.Record, requiredFields(c.opts.Schema)) {
			st.healer.Retain(selector.SynthSample{URL: pageURL, HTML: html, Sidecar: sidecar, Truth: res.Record})
		}
	}
	samples := st.healer.Samples()
	if len(samples) == 0 {
		return
	}
	only := triggered
	if selector.FullResynth(len(st.doc.Fields), len(triggered)) {
		only = nil // ≥50% broken → full redesign
	}
	doc, err := selector.Synthesize(ctx, samples, c.opts.Schema, c.opts.Propose, only)
	if err != nil {
		return
	}
	if only == nil {
		for f, sel := range doc.Fields {
			st.doc.Fields[f] = sel
		}
	} else {
		for _, f := range only {
			if sel, ok := doc.Fields[f]; ok {
				st.doc.Fields[f] = sel
			} else {
				delete(st.doc.Fields, f) // still failing → per-page LLM
			}
		}
	}
	if raw, err := json.Marshal(st.doc); err == nil {
		if perr := c.db.PutSelectors(domain, c.schemaHash, string(raw), len(samples)); perr != nil {
			fmt.Fprintf(os.Stderr, "warning: cache selectors: %v\n", perr)
		}
	}
}

// flushNullRates persists current null rates into fields_json.
// ponytail: a crash loses ≤25 pages of stats (ceiling = slightly stale
// trigger after --resume — self-corrects within 25 pages).
func (c *crawlContext) flushNullRates(st *domainState, domain string) {
	for f, sel := range st.doc.Fields {
		sel.NullRate = st.healer.NullRate(f)
		st.doc.Fields[f] = sel
	}
	if raw, err := json.Marshal(st.doc); err == nil {
		if perr := c.db.PutSelectors(domain, c.schemaHash, string(raw), st.doc.SamplesUsed); perr != nil {
			fmt.Fprintf(os.Stderr, "warning: cache selectors: %v\n", perr)
		}
	}
}

func hasNonCacheable(docJSON string, sch *extract.Schema) bool {
	var doc selector.SelectorDoc
	if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
		return true
	}
	for _, f := range selector.SchemaFields(sch) {
		if _, ok := doc.Fields[f]; !ok {
			return true
		}
	}
	return false
}

func requiredFields(sch *extract.Schema) []string {
	obj, ok := sch.Raw.(map[string]any)
	if !ok {
		return nil
	}
	req, ok := obj["required"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, r := range req {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func validRequired(rec map[string]any, required []string) bool {
	for _, f := range required {
		if v, ok := rec[f]; !ok || v == nil {
			return false
		}
	}
	return true
}

// qualityErr routes a non-2xx response through Clean+Classify, returning a
// typed ErrQuality error for blocked pages or nil when content is fine.
// The page lands on the existing page-error path (counted, not cached).
func qualityErr(ctx context.Context, task core.FetchTask, resp *fetch.FetchResponse) error {
	if resp == nil || resp.StatusCode/100 == 2 {
		return nil
	}
	finalURL := resp.FinalURL
	if finalURL == "" {
		finalURL = task.URL
	}
	cleaned, cerr := clean.Clean(ctx, clean.RawPage{
		HTML: resp.HTML, URL: task.URL, FinalURL: finalURL, StatusCode: resp.StatusCode,
		ContentType: resp.Headers.Get("Content-Type"),
	})
	if cerr != nil || cleaned.Quality == clean.IssueNone {
		return nil
	}
	return &clean.QualityError{Issue: cleaned.Quality, URL: task.URL}
}

func domainOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "invalid"
	}
	if u.Host == "" {
		return "file"
	}
	return strings.ToLower(u.Host)
}

func hostOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", err
	}
	return strings.ToLower(u.Host), nil
}

func isHTTP(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func hashTask(t core.FetchTask) string {
	if t.URLHash != "" {
		return t.URLHash
	}
	return t.URL
}

func newRunID() string {
	return strings.ReplaceAll(fmt.Sprintf("%d", time.Now().UnixNano()), "-", "") + "-crawl"
}
