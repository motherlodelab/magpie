package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/scrape"

	"github.com/spf13/cobra"
)

func newSearchCmd() *cobra.Command {
	var provider, out string
	var limit, scrapeTop int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the web via a BYOK/no-key SERP provider, optionally scrape the top hits",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearch(cmd.Context(), args[0], provider, limit, scrapeTop, out)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "", "brave|serper|serpapi|searxng|exa|duckduckgo (default duckduckgo — zero-key)")
	cmd.Flags().IntVar(&limit, "limit", 10, "max hits")
	cmd.Flags().IntVar(&scrapeTop, "scrape-top", 0, "scrape the first N hits through the normal pipeline")
	cmd.Flags().StringVar(&out, "out", "", "output path (default stdout)")
	return cmd
}

func runSearch(ctx context.Context, query, provider string, limit, scrapeTop int, out string) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)

	records, err := scrape.Search(ctx, scrapeDeps(db, cfg), query, scrape.SearchOptions{
		Provider: provider, Limit: limit, ScrapeTop: scrapeTop,
	})
	if errors.Is(err, scrape.ErrMissingKey) { // QA ST9: --api-key and MAGPIE_API_KEY are LLM keys — name the engine's own
		return fmt.Errorf("%w: set MAGPIE_%s_API_KEY or `magpie config set-key %s`", err, strings.ToUpper(provider), provider)
	}
	if err != nil {
		return err // exitFor owns the code mapping (missing key → 7)
	}
	f := os.Stdout
	if out != "" {
		var err error
		if f, err = os.Create(out); err != nil {
			return fmt.Errorf("search: open out: %w", err)
		}
		defer func() { _ = f.Close() }() //nolint:errcheck // file close at command end; write errors already surfaced
	}
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("search: encode record: %w", err)
		}
	}
	return nil
}
