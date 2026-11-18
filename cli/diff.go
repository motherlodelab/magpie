package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/recdiff"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"

	"github.com/spf13/cobra"
)

func newDiffCmd() *cobra.Command {
	var against, key, items, ignore, format string
	var records bool
	cmd := &cobra.Command{
		Use:   "diff <url> --against <file> | diff --records <prev> <cur>",
		Short: "Word diff of a URL against a snapshot, or record-set diff of two record files",
		Long: `Two modes.

URL mode: fetch a URL and diff its current markdown against a snapshot file
(--against <file>, - = stdin). Prints -old +new word pairs; identical input
prints nothing and exits 0. Zero LLM.

Records mode (--records): diff two record files (JSONL or a JSON array),
matching records by key and reporting rows added, changed and gone with
field-level before/after values. No changes prints nothing and exits 0.
Zero LLM.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if records {
				if len(args) != 2 {
					return fail(2, "diff: --records needs exactly two files: prev cur")
				}
				return runRecordDiff(args[0], args[1], recdiff.Options{
					Key:    key,
					Items:  items,
					Ignore: splitComma(ignore),
				}, format, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			if len(args) != 1 {
				return fail(2, "diff: exactly one URL is required")
			}
			return runDiff(cmd.Context(), args[0], diffOptions{Against: against})
		},
	}
	cmd.Flags().StringVar(&against, "against", "", "snapshot file to diff against (- = stdin)")
	cmd.Flags().BoolVar(&records, "records", false, "record-set mode: diff two record files given as prev cur")
	cmd.Flags().StringVar(&key, "key", "", "dotted path identifying a record (default url)")
	cmd.Flags().StringVar(&items, "items", "", "dotted path to an array to explode; each element keyed by --key inside it")
	cmd.Flags().StringVar(&ignore, "ignore", "", "comma-separated dotted paths never compared")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|json")
	return cmd
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type diffOptions struct {
	Against string
}

func runDiff(ctx context.Context, rawURL string, o diffOptions) error {
	if o.Against == "" {
		return fail(2, "diff: --against is required")
	}
	var snap []byte
	var err error
	if o.Against == "-" {
		snap, err = io.ReadAll(io.LimitReader(os.Stdin, 50<<20))
		if err != nil {
			return fmt.Errorf("diff: read stdin: %w", err)
		}
	} else {
		snap, err = os.ReadFile(o.Against)
		if err != nil {
			return fmt.Errorf("diff: read %s: %w", o.Against, err)
		}
	}
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.CacheDB)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }() //nolint:errcheck // end of command; close error unactionable

	res, err := scrape.Run(ctx, scrape.Deps{
		DB: db,
		ExtractorFor: func(p, key, m string, s *extract.Schema, runID string) (extract.Extractor, error) {
			return newExtractor(p, key, m, s, db, runID)
		},
		APIKeyFor: cfg.APIKey,
	}, rawURL, scrape.Options{})
	if err != nil {
		return keyHint(err)
	}
	diff, err := scrape.DiffWords(string(snap), res.Markdown)
	if err != nil {
		return err
	}
	if diff != "" {
		fmt.Print(diff)
	}
	return nil
}

func runRecordDiff(prevPath, curPath string, o recdiff.Options, format string, out, errW io.Writer) error {
	if prevPath == "-" || curPath == "-" {
		return fail(2, "diff: records mode takes two file paths, not stdin")
	}
	if format != "text" && format != "json" {
		return fail(2, "diff: --format must be text or json")
	}
	load := func(path string) (recdiff.Set, error) {
		f, err := os.Open(path)
		if err != nil {
			return recdiff.Set{}, fmt.Errorf("diff: read %s: %w", path, err)
		}
		defer func() { _ = f.Close() }() //nolint:errcheck // read-only; close error unactionable
		return recdiff.Load(f, o)
	}
	prev, err := load(prevPath)
	if err != nil {
		return err
	}
	cur, err := load(curPath)
	if err != nil {
		return err
	}
	res := recdiff.Diff(prev, cur, o)
	if format == "json" {
		b, err := json.Marshal(res)
		if err != nil {
			return fmt.Errorf("diff: marshal: %w", err)
		}
		fmt.Fprintln(out, string(b))
		return nil
	}
	if res.NoKey > 0 {
		fmt.Fprintf(errW, "! %d records had no key\n", res.NoKey)
	}
	if res.DupKeys > 0 {
		fmt.Fprintf(errW, "! %d duplicate keys (last occurrence wins)\n", res.DupKeys)
	}
	for _, r := range res.Rows {
		switch r.Change {
		case "added":
			fmt.Fprintf(out, "+ %s\n", r.Key)
		case "gone":
			fmt.Fprintf(out, "- %s\n", r.Key)
		case "changed":
			for _, fc := range r.Fields {
				if fc.Long {
					fmt.Fprintf(out, "~ %s %s: (long field, hash differs)\n", r.Key, fc.Path)
					continue
				}
				fmt.Fprintf(out, "~ %s %s: %s → %s\n", r.Key, fc.Path, fc.Old, fc.New)
			}
		}
	}
	return nil
}
