package crawl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// proxy is the validated per-run egress (opts.Proxy), wired into
	// the checker at begin(); nil when unset.
	proxy *url.URL

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
	// Per-run egress validated once, pre-I/O (typed ErrProxyConfig) —
	// page fetches read opts.Proxy on their FetchRequests, the robots
	// checker gets the parsed URL at begin().
	var proxy *url.URL
	if opts.Proxy != "" {
		u, err := fetch.ValidateRequestProxy(opts.Proxy)
		if err != nil {
			return nil, err
		}
		proxy = u
	}
	cc := &crawlContext{db: opts.DB, opts: opts, proxy: proxy}
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

	c.checker = NewChecker()
	// Validated in newCrawlContext (pre-I/O); robots fetches ride the
	// per-run egress so a proxy-only site's politeness check uses the
	// same route as its page fetches. One crawl, one egress story.
	c.checker.UseProxy(c.proxy)
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
