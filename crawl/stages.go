package crawl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/core"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/selector"
)

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
