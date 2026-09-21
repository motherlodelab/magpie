package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	pluginExec "github.com/motherlodelab/magpie/plugin/exec"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"

	"github.com/spf13/cobra"
)

func newCrawlCmd() *cobra.Command {
	var schema, format, out, resume, status string
	var maxPages, maxDepth, concurrency int
	var sameHost bool
	var rate float64
	var ignoreRobots bool
	var provider, model, exporterCmd string
	var pathPrefix string
	var include, exclude []string
	var allowSubdomains, noSitemap, sitemapOnly, autoThrottle bool
	var browser, lang string
	var corpus bool
	cmd := &cobra.Command{
		Use:   "crawl <url>",
		Short: "BFS crawl + extract a site",
		Args: func(cmd *cobra.Command, args []string) error {
			if status != "" {
				if len(args) != 0 {
					return fmt.Errorf("crawl: --status takes no URL argument")
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("accepts 1 arg(s), received %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if status != "" {
				return runCrawlStatus(status)
			}
			return runCrawl(cmd.Context(), args[0], crawlCLIOptions{
				Schema: schema, Format: format, Out: out, Resume: resume,
				MaxPages: maxPages, MaxDepth: maxDepth, Concurrency: concurrency,
				SameHost: sameHost, Rate: rate, IgnoreRobots: ignoreRobots,
				Provider: provider, Model: model, ExporterCmd: exporterCmd,
				PathPrefix: pathPrefix, Include: include, Exclude: exclude,
				AllowSubdomains: allowSubdomains, NoSitemap: noSitemap, Browser: browser,
				Lang: lang, Corpus: corpus, SitemapOnly: sitemapOnly, AutoThrottle: autoThrottle,
			})
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "JSON Schema file (yaml/json, required unless --corpus)")
	cmd.Flags().BoolVar(&corpus, "corpus", false, "schema-less corpus mode: one {url,title,depth,markdown} JSONL record per page with cleaned main-content text; zero LLM calls, no API key (jsonl only; mutually exclusive with --schema)")
	cmd.Flags().StringVar(&format, "format", "jsonl", "jsonl|json|csv|sqlite")
	cmd.Flags().StringVar(&out, "out", "", "output path (default stdout)")
	cmd.Flags().StringVar(&resume, "resume", "", "resume a checkpointed run_id")
	cmd.Flags().IntVar(&maxPages, "max-pages", 100, "max pages to claim/fetch (counts pages that fed synthesis too)")
	cmd.Flags().IntVar(&maxDepth, "max-depth", 3, "max link depth from seed")
	cmd.Flags().IntVar(&concurrency, "concurrency", 8, "fetch workers")
	cmd.Flags().BoolVar(&sameHost, "same-host", true, "follow only same-host links")
	cmd.Flags().StringVar(&pathPrefix, "path-prefix", "", "only follow links under this path prefix")
	cmd.Flags().StringSliceVar(&include, "include", nil, "comma-separated URL globs to include, e.g. '**/docs/**' (** crosses /, * stays in one segment)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "comma-separated URL globs to exclude (wins over --include)")
	cmd.Flags().BoolVar(&allowSubdomains, "allow-subdomains", false, "follow links into subdomains of the seed host")
	cmd.Flags().BoolVar(&noSitemap, "no-sitemap", false, "skip sitemap seed expansion")
	cmd.Flags().BoolVar(&sitemapOnly, "sitemap-only", false, "frontier = sitemap URLs only (no seed enqueue, no link-following); contradicts --no-sitemap")
	cmd.Flags().BoolVar(&autoThrottle, "auto-throttle", false, "adaptive per-host pacing: back off ×2 on 429/5xx, decay on success (static path only)")
	cmd.Flags().StringVar(&browser, "browser", "", "TLS-impersonating browser fingerprint: "+fetch.BrowserHelp)
	cmd.Flags().StringVar(&lang, "lang", "", "Accept-Language header value, e.g. fr-CA,fr;q=0.9 (no control characters)")
	cmd.Flags().StringVar(&status, "status", "", "print a run's status row + pending/inflight/done/errors counts instead of crawling")
	cmd.Flags().Float64Var(&rate, "rate", 1, "per-host requests/sec")
	cmd.Flags().BoolVar(&ignoreRobots, "ignore-robots", false, "fetch despite robots.txt (prints a warning)")
	cmd.Flags().StringVar(&provider, "provider", "", ProviderHelp)
	cmd.Flags().StringVar(&model, "model", "", "model name")
	// ponytail: argv = strings.Fields (no quoting) — paths with spaces need a wrapper script.
	cmd.Flags().StringVar(&exporterCmd, "exporter-cmd", "", "pipe each record as JSONL to this program's stdin (in addition to --out)")
	return cmd
}

type crawlCLIOptions struct {
	Schema       string
	Format       string
	Out          string
	Resume       string
	MaxPages     int
	MaxDepth     int
	Concurrency  int
	SameHost     bool
	Rate         float64
	IgnoreRobots bool
	Provider     string
	Model        string
	ExporterCmd  string
	Corpus       bool
	// Scope bounds; validated inside crawl.Run pre-I/O (ErrBadScope → exit 2).
	PathPrefix      string
	Include         []string
	Exclude         []string
	AllowSubdomains bool
	NoSitemap       bool
	SitemapOnly     bool
	AutoThrottle    bool
	Browser         string
	Lang            string
}

// runCrawlStatus is the CLI twin of the MCP crawl_site run_id poll: the
// run row plus frontier counts. Unknown ids fail handler-locally with
// exit 4 (no exitFor arm — status misses are command-specific usage).
func runCrawlStatus(runID string) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)
	info, err := db.GetRun(runID)
	if err != nil {
		return fail(4, "crawl: %v", err)
	}
	pending, inflight, done, errs, serr := db.CrawlStats(runID)
	if serr != nil {
		return serr
	}
	doc, merr := marshalOut(crawlStatusOut{
		RunID: info.RunID, Command: info.Command,
		StartedAt: info.StartedAt, FinishedAt: info.FinishedAt,
		Status: info.Status, PagesOK: info.PagesOK, PagesErr: info.PagesErr,
		Pending: pending, Inflight: inflight, Done: done, Errors: errs,
	}, "crawl")
	if merr != nil {
		return merr
	}
	fmt.Println(doc)
	return nil
}

type crawlStatusOut struct {
	RunID      string `json:"run_id"`
	Command    string `json:"command"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Status     string `json:"status"`
	PagesOK    int    `json:"pages_ok"`
	PagesErr   int    `json:"pages_err"`
	Pending    int    `json:"pending"`
	Inflight   int    `json:"inflight"`
	Done       int    `json:"done"`
	Errors     int    `json:"errors"`
}

func runCrawl(ctx context.Context, seedURL string, o crawlCLIOptions) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	if o.Schema != "" {
		cfg.Schema = o.Schema
	}
	if o.Format != "" {
		cfg.Format = o.Format
	}
	if o.Out != "" {
		cfg.Out = o.Out
	}
	if o.Corpus {
		if cfg.Schema != "" {
			return fail(2, "crawl: --corpus and --schema are mutually exclusive")
		}
		if err := crawl.ValidateCorpus(true, cfg.Format); err != nil {
			return fail(2, "%v", err)
		}
		cfg.Format = "jsonl"
	} else if cfg.Schema == "" {
		return fail(2, "crawl: --schema is required")
	}
	switch cfg.Format {
	case "jsonl", "json", "csv", "sqlite":
	default:
		return fail(2, "crawl: --format must be jsonl|json|csv|sqlite")
	}
	// Pure option contradiction: fails before credential resolution.
	if err := crawl.ValidateSitemapOnly(o.SitemapOnly, o.NoSitemap); err != nil {
		return fail(2, "%v", err)
	}
	provider := cfg.ExtractProvider
	if o.Provider != "" {
		provider = o.Provider
	}
	model := cfg.Model
	if o.Model != "" {
		model = o.Model
	}
	// Corpus mode is keyless: no schema load, no provider/API-key resolution,
	// no extractor wiring — Run gets Extractor/Propose nil and never calls them.
	var key string
	var sch *extract.Schema
	if !o.Corpus {
		key = cfg.APIKey(provider)
		if key == "" && needsAPIKey(provider) {
			return missingKeyErr(provider)
		}
		var lerr error
		sch, lerr = extract.LoadSchema(cfg.Schema)
		if lerr != nil {
			return lerr
		}
	}
	if o.ExporterCmd != "" {
		cfg.ExporterCmd = o.ExporterCmd
	}
	// Pre-I/O: an unknown fingerprint or bad lang must exit 2, never burn a fetch.
	if err := scrape.ValidateOptions(scrape.Options{Browser: o.Browser, Lang: o.Lang}); err != nil {
		return err
	}
	if o.IgnoreRobots {
		fmt.Fprintln(os.Stderr, "WARNING: --ignore-robots set; fetching despite robots.txt")
	}

	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)

	runID := o.Resume
	resuming := runID != ""
	if !resuming {
		runID = uuidNew()
	}
	var ex extract.Extractor
	var propose selector.ProposeFunc
	if !o.Corpus {
		ex, propose, err = wireExtractor(provider, key, model, sch, db, runID, cfg.MaxCost)
		if err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// Subprocess exporter tee: records flow to --out AND the child stdin.
	// The exporter is a copy — its failure warns (exit 4) but never blocks
	// the primary sink. Spawned after the NotifyContext swap: the closure
	// captures the ctx VARIABLE, so spawning must follow the last write
	// (go-statement ordering is the happens-before edge).
	var exportCh chan map[string]any
	var exportDone chan error
	var onRecord func(map[string]any)
	if cfg.ExporterCmd != "" {
		argv := strings.Fields(cfg.ExporterCmd)
		exp := &pluginExec.Exporter{Cmd: argv}
		exportCh = make(chan map[string]any, 1000)
		exportDone = make(chan error, 1)
		go func() { exportDone <- exp.Export(ctx, exportCh) }()
		onRecord = func(r map[string]any) { exportCh <- r }
	}

	res, err := crawl.Run(ctx, crawl.Options{
		SeedURL: seedURL, Schema: sch, MaxPages: o.MaxPages, MaxDepth: o.MaxDepth,
		SameHost: o.SameHost, FetchWorkers: o.Concurrency, Rate: o.Rate,
		PathPrefix: o.PathPrefix, Include: o.Include, Exclude: o.Exclude,
		AllowSubdomains: o.AllowSubdomains, NoSitemap: o.NoSitemap,
		SitemapOnly: o.SitemapOnly, AutoThrottle: o.AutoThrottle,
		Format: cfg.Format, Out: cfg.Out, RunID: runID,
		Resume: resuming, ResumeID: o.Resume, IgnoreRobots: o.IgnoreRobots,
		Browser: o.Browser, Lang: o.Lang, Corpus: o.Corpus,
		Provider: provider, Model: model, MaxCost: cfg.MaxCost,
		DB: db, Extractor: ex, Propose: propose, OnRecord: onRecord,
	})
	if exportCh != nil {
		close(exportCh)
		if exportErr := <-exportDone; exportErr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: exporter: %v\n", exportErr)
			if err == nil && res.Records > 0 && res.PagesErr == 0 {
				return fail(4, "crawl: exporter failed: %v", exportErr)
			}
		}
	}
	if err != nil {
		// Typed crawl errors map in exitFor: robots → 5, ceiling → 6,
		// scope/SSRF → 2.
		return err
	}
	fmt.Fprintf(os.Stderr, "crawl: run_id=%s pages_ok=%d pages_err=%d records=%d\n", res.RunID, res.PagesOK, res.PagesErr, res.Records)
	if res.Records == 0 {
		return fail(3, "crawl: no records extracted")
	}
	if res.PagesErr > 0 {
		return fail(4, "crawl: partial success (%d pages errored)", res.PagesErr)
	}
	return nil
}

// wireExtractor builds the per-run LLM extractor and the selector-proposal
// step (synthesis) — one cohesive unit: both exist only in extracted mode,
// and corpus mode passes nil/nil all the way to crawl.Run.
func wireExtractor(provider, key, model string, sch *extract.Schema, db *store.DB, runID string, maxCost float64) (extract.Extractor, selector.ProposeFunc, error) {
	ex, err := newExtractor(provider, key, model, sch, db, runID)
	if err != nil {
		return nil, nil, err
	}
	propose := func(pctx context.Context, fields []string, trimmed string) (map[string]string, error) {
		props := map[string]any{}
		for _, f := range fields {
			props[f] = map[string]any{"type": "string"}
		}
		raw, merr := json.Marshal(map[string]any{
			"type": "object", "properties": props,
			"required": fields, "additionalProperties": false,
		})
		if merr != nil {
			return nil, merr
		}
		asch, merr := extract.ParseSchema(raw)
		if merr != nil {
			return nil, merr
		}
		pex, err := newExtractor(provider, key, model, asch, db, runID)
		if err != nil {
			return nil, err
		}
		if cerr := checkCostCeiling(db, runID, provider, model, trimmed, maxCost); cerr != nil {
			return nil, cerr
		}
		res, xerr := pex.Extract(pctx, extract.ExtractInput{
			Markdown: "Propose one CSS selector per required field that extracts its value from the page below.\n\n" + trimmed,
			Schema:   asch, Purpose: "synth",
		})
		if xerr != nil {
			return nil, xerr
		}
		out := map[string]string{}
		for _, f := range fields {
			if v, ok := res.Record[f].(string); ok && v != "" {
				out[f] = v
			}
		}
		return out, nil
	}
	return ex, propose, nil
}
