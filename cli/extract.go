package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"

	"github.com/spf13/cobra"
)

func newExtractCmd() *cobra.Command {
	var schema, contentType, provider, model, out, prompt string
	cmd := &cobra.Command{
		Use:   "extract",
		Short: "Extract structured data from stdin/file (no fetch)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExtract(cmd.Context(), extractOptions{
				Schema: schema, ContentType: contentType, Provider: provider,
				Model: model, Out: out, File: firstArg(args), Prompt: prompt,
			})
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "JSON Schema file (yaml/json)")
	cmd.Flags().StringVar(&contentType, "content-type", "html", "html|markdown")
	cmd.Flags().StringVar(&provider, "provider", "", ProviderHelp+"|auto")
	cmd.Flags().StringVar(&model, "model", "", "model name")
	cmd.Flags().StringVar(&out, "out", "", "output path (default stdout)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "free-text instruction; returns plain text with no schema (mutually exclusive with --schema)")
	return cmd
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

type extractOptions struct {
	Schema      string
	ContentType string
	Provider    string
	Model       string
	Out         string
	File        string
	Prompt      string
}

func runExtract(ctx context.Context, o extractOptions) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	if o.Schema != "" {
		cfg.Schema = o.Schema
	}
	if o.Provider != "" {
		cfg.ExtractProvider = o.Provider
	}
	if o.Model != "" {
		cfg.Model = o.Model
	}
	if o.Prompt != "" && cfg.Schema != "" {
		return fail(2, "extract: --prompt and --schema are mutually exclusive")
	}
	if o.Prompt == "" && cfg.Schema == "" {
		return fail(2, "extract: --schema is required")
	}
	if o.ContentType != "html" && o.ContentType != "markdown" {
		return fail(2, "extract: --content-type must be html|markdown")
	}

	var input []byte
	if o.File != "" {
		input, err = os.ReadFile(o.File)
		if err != nil {
			return fmt.Errorf("extract: read %s: %w", o.File, err)
		}
	} else {
		input, err = io.ReadAll(io.LimitReader(os.Stdin, 50<<20))
		if err != nil {
			return fmt.Errorf("extract: read stdin: %w", err)
		}
	}

	var cleaned clean.CleanedPage
	if o.ContentType == "html" {
		cleaned, err = clean.Clean(ctx, clean.RawPage{HTML: input})
		if err != nil {
			return err
		}
	} else {
		cleaned = clean.CleanedPage{Markdown: string(input)}
	}

	var sch *extract.Schema
	if o.Prompt == "" {
		sch, err = extract.LoadSchema(cfg.Schema)
		if err != nil {
			return err
		}
	}
	provider := cfg.ExtractProvider
	model := cfg.Model
	if model == "" {
		model = "claude-sonnet-5"
	}
	key := cfg.APIKey(provider)
	if provider != "auto" && key == "" && extract.NeedsAPIKey(provider) {
		return missingKeyErr(provider)
	}

	db, err := openCmdDB(cfg)
	if err != nil {
		return err
	}
	defer closeDB(db)
	runID := store.NewRunID()
	if err := db.BeginRun(runID, "extract"); err != nil {
		return err
	}
	finish := func(ok, er int, status string) {
		if ferr := db.FinishRun(runID, ok, er, status); ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
		}
	}

	if o.Prompt != "" {
		return runExtractPrompt(ctx, db, runID, cfg, o, provider, model, cleaned)
	}

	ex, err := newExtractor(provider, key, model, sch, db, runID)
	if err != nil {
		return err
	}

	if err := scrape.CheckCostCeiling(db, runID, provider, model, cleaned.Markdown, cfg.MaxCost); err != nil {
		finish(0, 0, "error")
		return err
	}

	res, err := ex.Extract(ctx, extract.ExtractInput{
		Markdown: cleaned.Markdown, StructuredData: cleaned.StructuredData, Schema: sch,
	})
	if err != nil {
		finish(0, 1, "error")
		return err
	}
	finish(1, 0, "finished")
	doc, merr := marshalOut(map[string]any{"extracted": res.Record}, "extract")
	if merr != nil {
		return merr
	}
	return writeOut(o.Out, doc)
}

// runExtractPrompt serves --prompt: schema-less text with no validator.
// The prompt fan-out (explicit fail-fast, auto fallback) lives in
// scrape.Prompt; this wrapper owns the CLI run and the key pre-checks
// that preserve exit codes (missing key -> 7, keyless auto -> 2).
func runExtractPrompt(ctx context.Context, db *store.DB, runID string, cfg config.Config, o extractOptions, provider, model string, cleaned clean.CleanedPage) error {
	finish := func(ok, er int, status string) {
		if ferr := db.FinishRun(runID, ok, er, status); ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
		}
	}
	if provider == "auto" && len(scrape.AutoCandidates(cfg.APIKey)) == 0 {
		finish(0, 0, "error")
		return fail(2, "extract: --provider auto: no provider has a key (tried %s)", strings.Join(scrape.AutoProviderOrder, ", "))
	}
	pr, err := scrape.Prompt(ctx, scrapeDeps(db, cfg), runID, scrape.PromptOptions{
		Provider: provider, Model: model, MaxCost: cfg.MaxCost,
		System: "Reply with plain text only, no JSON.",
		User:   o.Prompt + "\n\nPage markdown:\n" + cleaned.Markdown, Purpose: "extract",
	})
	if err != nil {
		finish(0, 1, "error")
		return err
	}
	finish(1, 0, "finished")
	return writeOut(o.Out, pr.Text)
}
