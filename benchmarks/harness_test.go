// Package benchmarks is the offline quality harness: it runs the real clean
// pipeline over the committed testdata fixtures and asserts metric floors that
// survive -update regeneration: a rewritten .md golden cannot absorb a
// regression past these floors. Test-only: go build ./... never sees this
// package. Methodology: methodology.md; published numbers: README.md + results/.
package benchmarks

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/clean"
)

var writeResults = flag.String("write-results", "", "write run JSON to this path; relative paths resolve from the repo root (e.g. benchmarks/results/2026-09-24.json)")

// fixture is one corpus row. Bands and llmShrinks are frozen measurements
// (probe 2026-09-24 on master), not aspirations: they pin extraction quality
// with ±25-30% slack around the measured value.
type fixture struct {
	name       string      // file is ../testdata/<dir>/<name>.html (test binary cwd = package dir)
	dir        string      // "clean" or "quality"
	status     int         // RawPage.StatusCode — Classify is status-dependent
	band       [2]int      // frozen md word floor/ceiling; {0,0} ⇒ assert <50 cap only
	facts      []string    // curated visible strings; must appear in md AND llm text
	wantIssue  clean.Issue // pipeline reality; Classify's synthetic-input taxonomy stays in clean/quality_test.go
	llmShrinks bool        // len(llm) < len(md); true only for article — product's llm output GROWS (metadata header + structured data outweigh a 70-word body)
}

var corpus = []fixture{
	{name: "article", dir: "clean", status: 200, band: [2]int{90, 150},
		facts:     []string{"The Widget Price Guide", "Widget Basic", "Widget Max", "Two-year warranty", "Ships worldwide"},
		wantIssue: clean.IssueNone, llmShrinks: true},
	{name: "product", dir: "clean", status: 200, band: [2]int{52, 88},
		facts:     []string{"Widget Pro", "flagship widget", "reinforced steel body", "five stars", "free shipping across the EU"},
		wantIssue: clean.IssueNone},
	{name: "spa-shell", dir: "clean", status: 200, band: [2]int{4, 12},
		facts:     []string{"Please enable JavaScript to use this app."},
		wantIssue: clean.IssueNone},
	{name: "challenge-akamai", dir: "quality", status: 403, wantIssue: clean.IssueAccessDenied},
	{name: "login-wall", dir: "quality", status: 401, wantIssue: clean.IssueLoginRequired},
	{name: "empty-shell", dir: "quality", status: 200, wantIssue: clean.IssueNone},
	{name: "rich-with-marker", dir: "quality", status: 403, band: [2]int{560, 1000},
		facts: []string{
			"Writing About Bot Protection Without Getting Blocked", "Just a moment", "challenge token",
			"edge caching, and retry budgets", "naive substring detectors fire on your own documentation",
		},
		wantIssue: clean.IssueNone},
}

// result is one row of the results JSON written by -write-results.
type result struct {
	Fixture    string `json:"fixture"`
	RawWords   int    `json:"raw_words"`
	MDWords    int    `json:"md_words"`
	LLMTokens  int    `json:"llm_tokens"` // len/4 chars-as-tokens proxy, repo convention (capTokens)
	MDChars    int    `json:"md_chars"`
	LLMChars   int    `json:"llm_chars"`
	FactsKept  int    `json:"facts_kept"`
	FactsTotal int    `json:"facts_total"`
	// Clean+ToLLMText combined, warm — named pipeline_ns because it is not
	// clean-only (see runRow).
	PipelineNS int64 `json:"pipeline_ns"`
}

type resultsFile struct {
	GeneratedAt string `json:"generated_at"`
	GoVersion   string `json:"go_version"`
	Commit      string `json:"commit"`
	Aggregates  struct {
		FidelityPct           float64 `json:"fidelity_pct"`
		MeanReductionVsRawPct float64 `json:"mean_reduction_vs_raw_pct"`
	} `json:"aggregates"`
	PerFixture []result `json:"per_fixture"`
}

func loadHTML(t testing.TB, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", dir, name+".html"))
	if err != nil {
		t.Fatalf("load fixture %s/%s: %v", dir, name, err)
	}
	return b
}

// factRe matches a curated fact case-insensitively. Multi-word facts (space or
// hyphen) match as literal quoted text; single tokens get \b boundaries so
// "API" cannot match "apiece".
func factRe(fact string) *regexp.Regexp {
	q := regexp.QuoteMeta(fact)
	if !strings.ContainsAny(fact, " -") {
		q = `\b` + q + `\b`
	}
	return regexp.MustCompile(`(?i)` + q)
}

// runRow drives the real pipeline once: Clean + ToLLMText, timed together
// (the 1s ceiling covers the combined cost, so does the recorded duration).
func runRow(t testing.TB, html []byte, f fixture) (clean.CleanedPage, string, time.Duration) {
	t.Helper()
	raw := clean.RawPage{HTML: html, URL: "https://example.com/" + f.name, StatusCode: f.status}
	start := time.Now()
	p, err := clean.Clean(context.Background(), raw)
	if err != nil {
		t.Fatalf("Clean %s: %v", f.name, err)
	}
	llm := clean.ToLLMText(p)
	return p, llm, time.Since(start)
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

// rawWordCount strips tags naively for the raw-word denominator of the
// reduction aggregate; informational only.
func rawWordCount(html []byte) int {
	return clean.WordCount(string(tagRe.ReplaceAll(html, []byte(" "))))
}

func TestFactMatching(t *testing.T) {
	if factRe("API").MatchString("the apiece marker") {
		t.Error(`single-token fact "API" must not match "apiece" (word boundary)`)
	}
	if !factRe("Widget Pro").MatchString(" Meet the WIDGET PRO suite") {
		t.Error("multi-word fact must match case-insensitively")
	}
	if factRe("a.b (v2)").MatchString("a X b (v2)") { // QuoteMeta: '.' and parens literal
		t.Error("regex metachars in facts must be quoted")
	}
}

func TestCorpus_CleanQuality(t *testing.T) {
	// Warm the pipeline once: the first Clean pays one-time trafilatura/regex
	// init (~0.7-1.1s under go test ./... load) — not per-page cost. Without
	// this the 1s ceiling below flakes on whichever row runs coldest.
	warm := clean.RawPage{HTML: loadHTML(t, corpus[0].dir, corpus[0].name), URL: "https://example.com/warm", StatusCode: 200}
	if _, err := clean.Clean(context.Background(), warm); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	rows := make([]result, 0, len(corpus))
	for _, f := range corpus {
		t.Run(f.name, func(t *testing.T) {
			html := loadHTML(t, f.dir, f.name)
			p, llm, elapsed := runRow(t, html, f)
			if p.Quality != f.wantIssue {
				t.Errorf("Quality = %q, want %q", p.Quality, f.wantIssue)
			}
			words := clean.WordCount(p.Markdown)
			t.Logf("%s: words=%d band=%v md=%dB llm=%dB quality=%q %s", f.name, words, f.band, len(p.Markdown), len(llm), p.Quality, elapsed)
			if f.band != [2]int{} {
				if words < f.band[0] || words > f.band[1] {
					t.Errorf("words=%d outside frozen band %v", words, f.band)
				}
			} else if words >= 50 {
				t.Errorf("blocked/thin row kept %d words, want <50", words)
			}
			kept := 0
			for _, fact := range f.facts {
				inMD, inLLM := factRe(fact).MatchString(p.Markdown), factRe(fact).MatchString(llm)
				if !inMD {
					t.Errorf("fact %q missing from markdown", fact)
				}
				if !inLLM {
					t.Errorf("fact %q missing from llm text", fact)
				}
				if inMD && inLLM {
					kept++
				}
			}
			if f.llmShrinks && len(llm) >= len(p.Markdown) {
				t.Errorf("llm=%dB >= md=%dB, want shrink", len(llm), len(p.Markdown))
			}
			// ponytail: the 1s ceiling is a regression tripwire for the
			// hidden-browser-launch / quadratic-scan class (~3.2s/page Phase J
			// lesson), NOT a perf claim — measured cost is ~10ms. Upgrade path:
			// drop the ceiling and rely on Benchmark* alone if the corpus
			// grows past ~100 fixtures.
			if elapsed > time.Second {
				t.Errorf("clean+llm took %s, want <1s — pathological regression?", elapsed)
			}
			rows = append(rows, result{
				Fixture: f.name, RawWords: rawWordCount(html), MDWords: words,
				LLMTokens: len(llm) / 4, MDChars: len(p.Markdown), LLMChars: len(llm),
				FactsKept: kept, FactsTotal: len(f.facts), PipelineNS: int64(elapsed),
			})
		})
	}
	if *writeResults != "" {
		writeResultsFile(t, rows)
	}
}

func writeResultsFile(t *testing.T, rows []result) {
	t.Helper()
	path := *writeResults
	// go test runs the binary with cwd = this package dir; resolve relative
	// paths against the repo root so repo-root invocations do what they say.
	if !filepath.IsAbs(path) {
		path = filepath.Join("..", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("write results: %v", err)
	}
	out := resultsFile{GeneratedAt: time.Now().UTC().Format(time.RFC3339), GoVersion: runtime.Version(), PerFixture: rows}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				out.Commit = s.Value
			}
		}
	}
	kept, total, red := 0, 0, 0.0
	for _, r := range rows {
		kept += r.FactsKept
		total += r.FactsTotal
		if r.RawWords > 0 {
			red += (1 - float64(r.MDWords)/float64(r.RawWords)) * 100
		}
	}
	if total > 0 {
		out.Aggregates.FidelityPct = float64(kept) / float64(total) * 100
	}
	out.Aggregates.MeanReductionVsRawPct = red / float64(len(rows))
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write results: %v", err)
	}
	t.Logf("wrote results to %s", *writeResults)
}

func BenchmarkClean(b *testing.B) {
	for _, f := range corpus {
		if f.dir != "clean" {
			continue
		}
		raw := clean.RawPage{HTML: loadHTML(b, f.dir, f.name), URL: "https://example.com/" + f.name, StatusCode: f.status}
		b.Run(f.name, func(b *testing.B) {
			var words int
			for b.Loop() {
				p, err := clean.Clean(context.Background(), raw)
				if err != nil {
					b.Fatal(err)
				}
				words = clean.WordCount(p.Markdown)
			}
			b.ReportMetric(float64(words)/b.Elapsed().Seconds(), "words/sec")
		})
	}
}

func BenchmarkLLM(b *testing.B) {
	for _, f := range corpus {
		if f.dir != "clean" {
			continue
		}
		raw := clean.RawPage{HTML: loadHTML(b, f.dir, f.name), URL: "https://example.com/" + f.name, StatusCode: f.status}
		b.Run(f.name, func(b *testing.B) {
			var words int
			for b.Loop() {
				p, err := clean.Clean(context.Background(), raw)
				if err != nil {
					b.Fatal(err)
				}
				clean.ToLLMText(p)
				words = clean.WordCount(p.Markdown)
			}
			b.ReportMetric(float64(words)/b.Elapsed().Seconds(), "words/sec")
		})
	}
}
