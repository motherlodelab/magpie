package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"

	"github.com/spf13/cobra"
)

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Inspect and manage cached selectors"}
	inspect := &cobra.Command{Use: "inspect", Short: "Show cached selectors + null rates", RunE: runCacheInspect}
	inspect.Flags().String("domain", "", "filter by domain")
	inspect.Flags().String("schema-hash", "", "filter by schema hash")
	clear := &cobra.Command{Use: "clear", Short: "Evict cached selectors", RunE: runCacheClear}
	clear.Flags().String("domain", "", "evict only this domain (default all)")
	clear.Flags().String("schema-hash", "", "evict only this schema hash")
	heal := &cobra.Command{Use: "heal", Short: "Force re-synthesis for a domain", RunE: runCacheHeal}
	heal.Flags().String("domain", "", "domain to heal (required)")
	heal.Flags().String("schema", "", "schema file (required)")
	heal.Flags().String("seed-url", "", "BFS seed URL (required)")
	heal.Flags().String("provider", "", ProviderHelp)
	heal.Flags().String("model", "", "model name")
	// MarkFlagRequired fails only for unregistered flags (programmer bug here).
	_ = heal.MarkFlagRequired("domain")   //nolint:errcheck // flags registered just above
	_ = heal.MarkFlagRequired("schema")   //nolint:errcheck // flags registered just above
	_ = heal.MarkFlagRequired("seed-url") //nolint:errcheck // flags registered just above
	cmd.AddCommand(inspect, clear, heal)
	return cmd
}

func openCacheDB() (*store.DB, error) {
	cfg, err := resolveConfig()
	if err != nil {
		return nil, err
	}
	return store.Open(cfg.CacheDB) //nolint:wrapcheck // config error already contextual
}

func runCacheInspect(cmd *cobra.Command, _ []string) error {
	domain, err := cmd.Flags().GetString("domain")
	if err != nil {
		return err
	}
	hash, err := cmd.Flags().GetString("schema-hash")
	if err != nil {
		return err
	}
	db, err := openCacheDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() //nolint:errcheck // read-only; close unactionable
	entries, err := db.ListSelectors(domain)
	if err != nil {
		return err
	}
	shown := 0
	for _, e := range entries {
		if hash != "" && e.SchemaHash != hash {
			continue
		}
		var doc selector.SelectorDoc
		if jerr := json.Unmarshal([]byte(e.FieldsJSON), &doc); jerr != nil {
			fmt.Fprintf(os.Stderr, "warning: bad doc for %s: %v\n", e.Domain, jerr)
			continue
		}
		fmt.Printf("domain=%s schema_hash=%.12s samples=%d synthesized=%s\n", e.Domain, e.SchemaHash, e.SamplesUsed, e.SynthesizedAt)
		for name, sel := range doc.Fields {
			fmt.Printf("  field=%s type=%s expr=%s null_rate=%.2f\n", name, sel.Type, sel.Expr, sel.NullRate)
		}
		shown++
	}
	if shown == 0 {
		fmt.Fprintln(os.Stderr, "cache: no matching selectors")
	}
	return nil
}

func runCacheClear(cmd *cobra.Command, _ []string) error {
	domain, err := cmd.Flags().GetString("domain")
	if err != nil {
		return err
	}
	hash, err := cmd.Flags().GetString("schema-hash")
	if err != nil {
		return err
	}
	db, err := openCacheDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() //nolint:errcheck // close unactionable
	if hash != "" && domain == "" {
		return fail(2, "cache clear: --schema-hash needs --domain")
	}
	n, err := db.DeleteSelectors(domain, hash)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "cache: evicted %d entr%s\n", n, plural(n))
	return nil
}

func plural(n int64) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func runCacheHeal(cmd *cobra.Command, _ []string) error {
	domain, err := cmd.Flags().GetString("domain")
	if err != nil {
		return err
	}
	schemaPath, err := cmd.Flags().GetString("schema")
	if err != nil {
		return err
	}
	seedURL, err := cmd.Flags().GetString("seed-url")
	if err != nil {
		return err
	}
	providerFlag, err := cmd.Flags().GetString("provider")
	if err != nil {
		return err
	}
	modelFlag, err := cmd.Flags().GetString("model")
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	sch, err := extract.LoadSchema(schemaPath)
	if err != nil {
		return err
	}
	provider := cfg.ExtractProvider
	if providerFlag != "" {
		provider = providerFlag
	}
	model := cfg.Model
	if modelFlag != "" {
		model = modelFlag
	}
	key := cfg.APIKey(provider)
	if key == "" && extract.NeedsAPIKey(provider) {
		return missingKeyErr(provider)
	}
	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)
	runID := store.NewRunID()
	if err := db.BeginRun(runID, "cache-heal"); err != nil {
		return err
	}
	ex, err := newExtractor(provider, key, model, sch, db, runID)
	if err != nil {
		return err
	}

	// BFS ≤3 pages from the seed (static fetch only).
	static, err := fetch.NewStaticFetcher()
	if err != nil {
		return err
	}
	seen := map[string]bool{seedURL: true}
	queue := []string{seedURL}
	var samples []selector.SynthSample
	for len(queue) > 0 && len(samples) < 3 {
		rawURL := queue[0]
		queue = queue[1:]
		resp, ferr := static.Fetch(ctx, fetch.FetchRequest{URL: rawURL})
		if ferr != nil || resp.StatusCode/100 != 2 {
			continue
		}
		finalURL := resp.FinalURL
		if finalURL == "" {
			finalURL = rawURL
		}
		cleaned, cerr := clean.Clean(ctx, clean.RawPage{HTML: resp.HTML, URL: rawURL, FinalURL: finalURL})
		if cerr != nil {
			continue
		}
		if cerr := scrape.CheckCostCeiling(db, runID, provider, model, cleaned.Markdown, cfg.MaxCost); cerr != nil {
			return cerr
		}
		res, xerr := ex.Extract(ctx, extract.ExtractInput{
			Markdown: cleaned.Markdown, StructuredData: cleaned.StructuredData,
			Schema: sch, Purpose: "synth",
		})
		if xerr != nil {
			continue
		}
		if !validRequiredLocal(res.Record, sch) {
			continue
		}
		samples = append(samples, selector.SynthSample{URL: finalURL, HTML: string(resp.HTML), Sidecar: cleaned.StructuredData, Truth: res.Record})
		for _, link := range harvestLinks(resp.HTML, finalURL, domain) {
			if !seen[link] && len(queue)+len(samples) < 6 {
				seen[link] = true
				queue = append(queue, link)
			}
		}
	}
	finishHeal := func(ok, er int, status string) {
		if ferr := db.FinishRun(runID, ok, er, status); ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
		}
	}
	if len(samples) < 2 {
		finishHeal(0, 0, "error")
		return fail(3, "cache heal: only %d usable sample(s) from %s; need at least 2", len(samples), seedURL)
	}
	doc, err := selector.Synthesize(ctx, samples, sch, nil, nil)
	if err != nil {
		finishHeal(0, 0, "error")
		return err
	}
	doc.Domain = domain
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	hash := selector.SchemaHash(sch)
	if err := db.PutSelectors(domain, hash, string(raw), doc.SamplesUsed); err != nil {
		return err
	}
	finishHeal(len(samples), 0, "finished")
	fmt.Fprintf(os.Stderr, "cache: healed %s (%d fields from %d samples)\n", domain, len(doc.Fields), len(samples))
	return nil
}

func validRequiredLocal(rec map[string]any, sch *extract.Schema) bool {
	obj, ok := sch.Raw.(map[string]any)
	if !ok {
		return true
	}
	req, ok := obj["required"].([]any)
	if !ok {
		return true
	}
	for _, r := range req {
		if s, ok := r.(string); ok {
			if v, present := rec[s]; !present || v == nil {
				return false
			}
		}
	}
	return true
}

func harvestLinks(html []byte, pageURL, domain string) []string {
	u, err := url.Parse(pageURL)
	if err != nil || u.Host == "" {
		return nil
	}
	var out []string
	for _, href := range linkHrefs(html) {
		ref, err := url.Parse(href)
		if err != nil {
			continue
		}
		abs := u.ResolveReference(ref)
		if abs.Host == u.Host && abs.Host == domain {
			out = append(out, abs.String())
		}
	}
	return out
}

func linkHrefs(html []byte) []string {
	var out []string
	for _, part := range strings.Split(string(html), "href=\"") {
		if j := strings.Index(part, "\""); j > 0 {
			if h := part[:j]; !strings.Contains(h, " ") && !strings.Contains(h, ">") {
				out = append(out, h)
			}
		}
	}
	return out
}
