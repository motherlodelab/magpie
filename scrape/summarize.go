package scrape

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/store"
)

// MaxSummarizeInputWords caps the markdown fed to the prompt (cost bound).
const MaxSummarizeInputWords = 4000

// SummarizeOptions configures one summarization. MaxSentences clamps to
// 1–20 (≤0 means the default 3). Provider "auto" tries AutoProviderOrder.
type SummarizeOptions struct {
	MaxSentences int
	Provider     string
	Model        string
	MaxCost      float64
}

// SummaryOut is one summarization: page identity plus hard-truncated text
// and the prompt call's usage (agents see the spend, not just the text).
type SummaryOut struct {
	URL      string             `json:"url"`
	FinalURL string             `json:"final_url"`
	Title    string             `json:"title"`
	Summary  string             `json:"summary"`
	Provider string             `json:"provider"`
	Model    string             `json:"model"`
	Usage    extract.TokenUsage `json:"usage"`
}

// Summarize scrapes rawURL markdown-only (zero schema, zero validator),
// prompts for at most N sentences, and hard-truncates the model text to N
// with stdlib splitting — a rambling model still returns ≤N.
// The prompt fan-out (explicit fail-fast, auto fallback) lives in Prompt;
// Summarize owns the scrape, the run, and the truncation.
func Summarize(ctx context.Context, d Deps, rawURL string, o SummarizeOptions) (SummaryOut, error) {
	res, err := Run(ctx, d, rawURL, Options{})
	if err != nil {
		return SummaryOut{}, err
	}
	out, err := SummarizeText(ctx, d, res.Markdown, o)
	if err != nil {
		return SummaryOut{}, err
	}
	out.URL, out.FinalURL, out.Title = res.URL, res.FinalURL, res.Title
	return out, nil
}

// SummarizeText is Summarize minus the fetch: it condenses caller-supplied
// markdown to at most N sentences and owns its own run row (command
// "summarize"). It never fetches — Summarize is Run + SummarizeText plus
// the page-identity fill. Callers that already hold the page (the desktop
// workspace) pass the markdown straight in and keep their URL/Title.
func SummarizeText(ctx context.Context, d Deps, markdown string, o SummarizeOptions) (SummaryOut, error) {
	n := o.MaxSentences
	if n <= 0 {
		n = 3
	}
	if n > 20 {
		n = 20
	}
	input := capWords(markdown, MaxSummarizeInputWords)
	system := fmt.Sprintf("Summarize the page in at most %d sentences. Reply with plain text only, no JSON, no markdown formatting.", n)

	runID := store.NewRunID()
	if err := d.DB.BeginRun(runID, "summarize"); err != nil {
		return SummaryOut{}, err
	}
	finish := func(ok int, status string) {
		if ferr := d.DB.FinishRun(runID, ok, 0, status); ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: finish run: %v\n", ferr)
		}
	}
	pr, err := Prompt(ctx, d, runID, PromptOptions{
		Provider: o.Provider, Model: o.Model, MaxCost: o.MaxCost,
		System: system, User: input, Purpose: "summarize",
	})
	if err != nil {
		finish(0, "error")
		return SummaryOut{}, err
	}
	finish(1, "finished")
	return SummaryOut{
		Summary: truncateSentences(pr.Text, n), Provider: pr.Provider, Model: o.Model,
		Usage: pr.Usage,
	}, nil
}

// capWords clips text to the first n words (cost bound on the way in).
func capWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return s
	}
	return strings.Join(words[:n], " ")
}

// truncateSentences hard-cuts text after the nth sentence terminator,
// returning the original prefix (truncation, never rewrite). Fewer than
// n sentences returns the whole trimmed text.
func truncateSentences(s string, n int) string {
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' || s[i] == '!' || s[i] == '?' {
			count++
			if count == n {
				return strings.TrimSpace(s[:i+1])
			}
		}
	}
	return strings.TrimSpace(s)
}
