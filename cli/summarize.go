package cli

import (
	"context"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"

	"github.com/spf13/cobra"
)

func newSummarizeCmd() *cobra.Command {
	var maxSentences int
	var provider, model, out string
	cmd := &cobra.Command{
		Use:   "summarize <url>",
		Short: "Summarize one URL in at most N sentences",
		Long: `Fetch a URL and summarize it in plain text. The summary is
hard-truncated to --max-sentences even when the model rambles.
--provider auto tries keyed providers in documented order.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSummarize(cmd.Context(), args[0], summarizeOptions{
				MaxSentences: maxSentences, Provider: provider, Model: model, Out: out,
			})
		},
	}
	cmd.Flags().IntVar(&maxSentences, "max-sentences", 3, "max sentences in the summary (1-20)")
	cmd.Flags().StringVar(&provider, "provider", "", ProviderHelp+"|auto")
	cmd.Flags().StringVar(&model, "model", "", "model name")
	cmd.Flags().StringVar(&out, "out", "", "output path (default stdout)")
	return cmd
}

type summarizeOptions struct {
	MaxSentences int
	Provider     string
	Model        string
	Out          string
}

func runSummarize(ctx context.Context, rawURL string, o summarizeOptions) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	provider := cfg.ExtractProvider
	if o.Provider != "" {
		provider = o.Provider
	}
	model := cfg.Model
	if o.Model != "" {
		model = o.Model
	}

	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)

	res, err := scrape.Summarize(ctx, scrapeDeps(db, cfg), rawURL, scrape.SummarizeOptions{
		MaxSentences: o.MaxSentences, Provider: provider, Model: model, MaxCost: cfg.MaxCost,
	})
	if err != nil {
		return keyHint(err)
	}
	doc, merr := marshalOut(map[string]any{
		"url": res.URL, "final_url": res.FinalURL, "title": res.Title,
		"summary": res.Summary, "provider": res.Provider, "model": res.Model,
		"usage": extract.UsageMap(res.Provider, res.Model, res.Usage),
	}, "summarize")
	if merr != nil {
		return merr
	}
	return writeOut(o.Out, doc)
}
