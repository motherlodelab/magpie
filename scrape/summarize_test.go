package scrape_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/crawl"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

var _ extract.Prompter = (*fakePrompterExtractor)(nil)

// fakePrompterExtractor scripts PromptText per call (last repeats) and
// records prompts. Extract loudly fails: firing on a prompt path would
// prove the validator bypass broken.
type fakePrompterExtractor struct {
	t            *testing.T
	promptScript []string
	promptErr    error
	mu           sync.Mutex
	calls        int
	systems      []string
	users        []string
}

func (f *fakePrompterExtractor) Name() string { return "fake-prompt" }

func (f *fakePrompterExtractor) Extract(_ context.Context, _ extract.ExtractInput) (extract.ExtractResult, error) {
	f.t.Error("Extract called on prompt path — validator bypass violated")
	return extract.ExtractResult{}, errors.New("must not be called")
}

func (f *fakePrompterExtractor) PromptText(_ context.Context, system, user string) (string, extract.TokenUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.promptErr != nil {
		return "", extract.TokenUsage{}, f.promptErr
	}
	f.systems, f.users = append(f.systems, system), append(f.users, user)
	idx := min(f.calls-1, len(f.promptScript)-1)
	return f.promptScript[idx], extract.TokenUsage{PromptTokens: 10, CompletionTokens: 5}, nil
}

func (f *fakePrompterExtractor) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePrompterExtractor) lastUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.users) == 0 {
		return ""
	}
	return f.users[len(f.users)-1]
}

// rambleScript returns 10 numbered sentences for truncation tests.
func rambleScript() string {
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString(strings.Repeat("word ", 8))
		sb.WriteString("sentence number " + strconv.Itoa(i) + ". ")
	}
	return sb.String()
}

func summarizeTestDeps(t *testing.T, db *store.DB, fx *fakePrompterExtractor, bodyHTML string) scrape.Deps {
	t.Helper()
	if bodyHTML == "" {
		bodyHTML = `<html><head><title>Page</title></head><body><h1>Page</h1><p>` + strings.Repeat("honest prose ", 80) + `</p></body></html>`
	}
	return scrape.Deps{
		DB: db,
		Fetcher: &fakeVerticalFetcher{bodies: map[string]fakeResp{
			"example.com/": {body: []byte(bodyHTML)},
		}},
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return "test-key" },
	}
}

func countTerminators(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' || s[i] == '!' || s[i] == '?' {
			n++
		}
	}
	return n
}

// TestSummarizeText_NeverFetches pins the M0d contract: SummarizeText
// consumes caller markdown and must never touch Deps.Fetcher — the nil
// fetcher makes any fetch attempt panic, so passing IS the proof. It
// also owns its run row (command "summarize") and leaves URL/Title zero
// (identity is the caller's, from the page it already holds).
func TestSummarizeText_NeverFetches(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{rambleScript()}}
	deps := scrape.Deps{
		DB: openScrapeDB(t),
		// Fetcher deliberately nil: a fetch path execution panics.
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return "test-key" },
	}
	res, err := scrape.SummarizeText(context.Background(), deps, "already-fetched page markdown", scrape.SummarizeOptions{
		MaxSentences: 3, Provider: "fake", Model: "fake",
	})
	if err != nil {
		t.Fatalf("SummarizeText: %v", err)
	}
	if n := countTerminators(res.Summary); n > 3 {
		t.Errorf("summary has %d terminators, want ≤3", n)
	}
	if res.URL != "" || res.FinalURL != "" || res.Title != "" {
		t.Errorf("identity = %q/%q/%q, want zero (the caller fills it)", res.URL, res.FinalURL, res.Title)
	}
	if got := fx.lastUser(); got != "already-fetched page markdown" {
		t.Errorf("prompt user = %q, want the caller markdown verbatim", got)
	}
	runs, err := deps.DB.ListRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Command != "summarize" {
		t.Fatalf("run rows = %+v, want exactly one summarize row", runs)
	}
}

func TestSummarize_TruncatesRamble(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{rambleScript()}}
	db := openScrapeDB(t)
	res, err := scrape.Summarize(context.Background(), summarizeTestDeps(t, db, fx, ""), "https://example.com/p", scrape.SummarizeOptions{
		MaxSentences: 3, Provider: "fake", Model: "fake",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if n := countTerminators(res.Summary); n > 3 {
		t.Errorf("summary has %d terminators, want ≤3:\n%s", n, res.Summary)
	}
	if !strings.HasPrefix(rambleScript(), res.Summary) {
		t.Errorf("summary is not a prefix of the model text (rewritten, not truncated):\n%s", res.Summary)
	}
	if res.Provider != "fake" {
		t.Errorf("provider = %q, want fake", res.Provider)
	}
	if res.Usage.PromptTokens != 10 || res.Usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v, want prompt tokens through (agents see the spend)", res.Usage)
	}
}

func TestSummarize_InputCap(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"short. text."}}
	db := openScrapeDB(t)
	long := `<html><head><title>Long</title></head><body><h1>Long</h1><p>` + strings.Repeat("word ", 10000) + `</p></body></html>`
	_, err := scrape.Summarize(context.Background(), summarizeTestDeps(t, db, fx, long), "https://example.com/long", scrape.SummarizeOptions{
		MaxSentences: 2, Provider: "fake", Model: "fake",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	user := fx.lastUser()
	if words := len(strings.Fields(user)); words > scrape.MaxSummarizeInputWords {
		t.Errorf("prompt input has %d words, want ≤%d (cap %d)", words, scrape.MaxSummarizeInputWords, scrape.MaxSummarizeInputWords)
	}
}

func TestSummarize_LogsPurpose(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"one. two."}}
	db := openScrapeDB(t)
	var gotRunID string
	deps := summarizeTestDeps(t, db, fx, "")
	deps.ExtractorFor = func(_, _, _ string, _ *extract.Schema, runID string) (extract.Extractor, error) {
		gotRunID = runID
		return fx, nil
	}
	if _, err := scrape.Summarize(context.Background(), deps, "https://example.com/p", scrape.SummarizeOptions{
		MaxSentences: 2, Provider: "fake", Model: "fake",
	}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	calls, err := db.LLMCalls(gotRunID)
	if err != nil {
		t.Fatalf("LLMCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("llm_calls rows = %d, want 1", len(calls))
	}
	if calls[0].Purpose != "summarize" {
		t.Errorf("purpose = %q, want summarize", calls[0].Purpose)
	}
}

func TestSummarize_CeilingAborts(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"one. two."}}
	db := openScrapeDB(t)
	_, err := scrape.Summarize(context.Background(), summarizeTestDeps(t, db, fx, ""), "https://example.com/p", scrape.SummarizeOptions{
		MaxSentences: 2, Provider: "openai", Model: "gpt-4o-mini", MaxCost: 0.000001,
	})
	if !errors.Is(err, crawl.ErrCostCeiling) {
		t.Fatalf("err = %v, want ErrCostCeiling", err)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("ceiling abort made %d prompter calls, want 0", got)
	}
}

func TestSummarize_ModelError(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptErr: errors.New("model exploded")}
	db := openScrapeDB(t)
	_, err := scrape.Summarize(context.Background(), summarizeTestDeps(t, db, fx, ""), "https://example.com/p", scrape.SummarizeOptions{
		MaxSentences: 2, Provider: "fake", Model: "fake",
	})
	if err == nil || !strings.Contains(err.Error(), "model exploded") {
		t.Fatalf("err = %v, want verbatim model error", err)
	}
}

func TestSummarize_MissingKey(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"one. two."}}
	db := openScrapeDB(t)
	deps := summarizeTestDeps(t, db, fx, "")
	deps.APIKeyFor = func(string) string { return "" }
	_, err := scrape.Summarize(context.Background(), deps, "https://example.com/p", scrape.SummarizeOptions{
		MaxSentences: 2, Provider: "openai", Model: "gpt-4o-mini",
	})
	if !errors.Is(err, scrape.ErrMissingKey) {
		t.Fatalf("err = %v, want ErrMissingKey", err)
	}
	if got := fx.total(); got != 0 {
		t.Errorf("missing-key made %d prompter calls, want 0", got)
	}
}

// inflightFetcher serves each URL after a stagger and tracks max
// concurrent Fetch calls with atomics.
type inflightFetcher struct {
	bodies map[string]fakeResp
	cur    atomic.Int64
	max    atomic.Int64
}

func (f *inflightFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	c := f.cur.Add(1)
	for {
		m := f.max.Load()
		if c <= m || f.max.CompareAndSwap(m, c) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	f.cur.Add(-1)
	for sub, r := range f.bodies {
		if strings.Contains(req.URL, sub) {
			if r.err != nil {
				return nil, r.err
			}
			return &fetch.FetchResponse{URL: req.URL, FinalURL: req.URL, StatusCode: 200, HTML: r.body}, nil
		}
	}
	return nil, errors.New("fake fetcher: unexpected URL " + req.URL)
}

func batchTestDeps(t *testing.T, db *store.DB, f *inflightFetcher) scrape.Deps {
	t.Helper()
	return scrape.Deps{
		DB:      db,
		Fetcher: f,
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			t.Error("ExtractorFor called on markdown-only batch — zero-LLM contract violated")
			return nil, errors.New("must not be called")
		},
		APIKeyFor: func(string) string { return "test-key" },
	}
}

func batchBody(title string) []byte {
	return []byte(`<html><head><title>` + title + `</title></head><body><h1>` + title + `</h1><p>` + strings.Repeat("honest prose ", 40) + `</p></body></html>`)
}

func TestBatch_InflightLimit(t *testing.T) {
	f := &inflightFetcher{bodies: map[string]fakeResp{}}
	var urls []string
	for i := 0; i < 5; i++ {
		host := "example.com/b" + strconv.Itoa(i)
		f.bodies[host] = fakeResp{body: batchBody("B" + strconv.Itoa(i))}
		urls = append(urls, "https://"+host)
	}
	db := openScrapeDB(t)
	if _, err := scrape.Batch(context.Background(), batchTestDeps(t, db, f), urls, scrape.BatchOptions{Concurrency: 2}); err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if m := f.max.Load(); m > 2 {
		t.Errorf("max inflight = %d, want ≤2", m)
	}
}

func TestBatch_OrderAndPerURLError(t *testing.T) {
	f := &inflightFetcher{bodies: map[string]fakeResp{
		// Staggered completion would invert arrival order without
		// collect-then-write; the sleep is inside Fetch, keyed per URL.
		"example.com/good1": {body: batchBody("One")},
		"example.com/good2": {body: batchBody("Two")},
	}}
	db := openScrapeDB(t)
	urls := []string{"https://example.com/good1", "https://example.com/missing", "https://example.com/good2"}
	items, err := scrape.Batch(context.Background(), batchTestDeps(t, db, f), urls, scrape.BatchOptions{Concurrency: 2})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	for i, u := range urls {
		if items[i].URL != u {
			t.Errorf("items[%d].URL = %q, want input order %q", i, items[i].URL, u)
		}
	}
	if !items[0].OK || items[0].Markdown == "" {
		t.Errorf("items[0] = %+v, want ok with markdown", items[0])
	}
	if items[1].OK || items[1].Error == "" {
		t.Errorf("items[1] = %+v, want ok:false with error string", items[1])
	}
	if !items[2].OK {
		t.Errorf("items[2] = %+v, want ok", items[2])
	}
}

func TestBatch_QualityErrorIsData(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "quality", "challenge-akamai.html"))
	if err != nil {
		t.Fatal(err)
	}
	db := openScrapeDB(t)
	deps := scrape.Deps{
		DB: db,
		Fetcher: &fakeVerticalFetcher{bodies: map[string]fakeResp{
			"example.com/blocked": {status: 403, body: raw},
		}},
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			t.Error("ExtractorFor called on quality-blocked batch item")
			return nil, errors.New("must not be called")
		},
		APIKeyFor: func(string) string { return "test-key" },
	}
	items, err := scrape.Batch(context.Background(), deps, []string{"https://example.com/blocked"}, scrape.BatchOptions{Concurrency: 1, Render: "static"})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if items[0].OK || !strings.Contains(items[0].Error, "access-denied") {
		t.Errorf("items[0] = %+v, want ok:false with typed quality error", items[0])
	}
}

func TestBatch_Over100(t *testing.T) {
	db := openScrapeDB(t)
	f := &inflightFetcher{}
	var urls []string
	for i := 0; i < 101; i++ {
		urls = append(urls, "https://example.com/"+strconv.Itoa(i))
	}
	if _, err := scrape.Batch(context.Background(), batchTestDeps(t, db, f), urls, scrape.BatchOptions{Concurrency: 2}); err == nil {
		t.Error("101 URLs: want pre-I/O error")
	}
	if m := f.max.Load(); m != 0 {
		t.Errorf("rejected batch still fetched (max inflight %d)", m)
	}
}

func TestBatch_BadLimit(t *testing.T) {
	db := openScrapeDB(t)
	f := &inflightFetcher{}
	if _, err := scrape.Batch(context.Background(), batchTestDeps(t, db, f), []string{"https://example.com/x"}, scrape.BatchOptions{Concurrency: 0}); err == nil {
		t.Error("concurrency 0: want pre-I/O error")
	}
}

func TestSummarize_AutoFallback(t *testing.T) {
	good := &fakePrompterExtractor{t: t, promptScript: []string{"one. two. three. four."}}
	bad := &fakePrompterExtractor{t: t, promptErr: errors.New("anthropic down")}
	db := openScrapeDB(t)
	deps := summarizeTestDeps(t, db, good, "")
	deps.APIKeyFor = func(p string) string {
		if p == "anthropic" || p == "openai" {
			return "k"
		}
		return ""
	}
	deps.ExtractorFor = func(p, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
		if p == "anthropic" {
			return bad, nil
		}
		return good, nil
	}
	res, err := scrape.Summarize(context.Background(), deps, "https://example.com/p", scrape.SummarizeOptions{
		MaxSentences: 2, Provider: "auto", Model: "fake",
	})
	if err != nil {
		t.Fatalf("Summarize auto: %v", err)
	}
	if res.Provider != "openai" {
		t.Errorf("provider = %q, want openai (failure falls through in order)", res.Provider)
	}
	if n := countTerminators(res.Summary); n > 2 {
		t.Errorf("summary has %d terminators, want ≤2", n)
	}
	if got := bad.total(); got != 1 {
		t.Errorf("failing provider calls = %d, want 1 attempt", got)
	}
}
