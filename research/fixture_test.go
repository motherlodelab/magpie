package research_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// pad builds a page the auto-render path keeps static. Probed 2026-10-05:
// a bare 199-byte article scores 3 (NeedsBrowser ⇒ rod launch); five
// ~20-word paragraphs score 1. Fails loudly instead of launching Chrome.
func pad(t *testing.T, title string, paras ...string) []byte {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html><html><head><title>%s</title></head><body><article><h1>%s</h1>", title, title)
	for _, p := range paras {
		fmt.Fprintf(&b, "<p>%s</p>", p)
	}
	for i := range 6 {
		fmt.Fprintf(&b, "<p>Background paragraph %d explains the survey method, the sample size and how respondents were chosen across regions.</p>", i)
	}
	b.WriteString(`</article><footer><a href="/about">About</a> <a href="/help">Help</a> <a href="/privacy">Privacy</a></footer></body></html>`)
	html := []byte(b.String())
	if s, _ := fetch.ScoreJSRequired(html, http.Header{"Content-Type": {"text/html"}}); fetch.NeedsBrowser(s) {
		t.Fatalf("fixture %q would launch Chrome (JS score %d) — pad it", title, s)
	}
	return html
}

// loginWall: probed 2026-10-05 — score 0, clean classifies login-required
// at 401. NOT testdata/quality/login-wall.html (scores 3 ⇒ rod launch).
const loginWall = `<!DOCTYPE html><html lang="en"><head><title>Sign in to continue</title></head><body>
<h1>Sign in</h1>
<p>Log in to continue reading this article. Members get full access to every report, the archive and the weekly briefing. Subscriptions renew monthly and you can cancel at any time from your account page.</p>
<form><input type="password" name="pw"><button>Login required</button></form>
<footer><a href="/about">About</a> <a href="/help">Help</a> <a href="/privacy">Privacy</a> <a href="/terms">Terms</a></footer>
</body></html>`

type page struct {
	status int
	html   []byte
}

// corpus is the fake vertical.Fetcher: canonical URL → page; unknown URLs
// are a plain error (unreadable "error"). It counts fetches per URL.
type corpus struct {
	mu    sync.Mutex
	pages map[string]page
	hits  map[string]int
}

func (c *corpus) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hits[req.URL]++
	p, ok := c.pages[req.URL]
	if !ok {
		return nil, fmt.Errorf("fetch: HTTP 404 for %s", req.URL)
	}
	// Echo the request URL: Result.URL comes from here, and a mismatch with
	// the visited/pin key must surface, not hide.
	return &fetch.FetchResponse{URL: req.URL, FinalURL: req.URL, StatusCode: p.status, HTML: p.html,
		Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}}, nil
}

func (c *corpus) fetches(u string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[u]
}

func (c *corpus) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, h := range c.hits {
		n += h
	}
	return n
}

// searx is the hermetic search backend: the real searxng client, pointed
// here by MAGPIE_SEARXNG_URL. q → result URLs; every query is logged in
// order. Result URLs must be absolute https:// (corpus keys too): RankURLs's
// http(s)-only gate silently drops anything else, and the run reads nothing.
type searx struct {
	*httptest.Server
	mu      sync.Mutex
	results map[string][]string
	log     []string
}

func newSearx(t *testing.T, results map[string][]string) *searx {
	t.Helper()
	s := &searx{results: results}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		s.log = append(s.log, q)
		urls := s.results[q]
		s.mu.Unlock()
		type hit struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		}
		out := struct {
			Results []hit `json:"results"`
		}{Results: []hit{}}
		for _, u := range urls {
			out.Results = append(out.Results, hit{Title: u, URL: u})
		}
		_ = json.NewEncoder(w).Encode(out) //nolint:errcheck // test server
	}))
	t.Cleanup(s.Close)
	t.Setenv("MAGPIE_SEARXNG_URL", s.URL)
	return s
}

func (s *searx) queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

// call is one recorded model call.
type call struct{ Task, Provider, Model, Material string }

// fakeLLM is every model in the run. It dispatches on the "TASK: <name>"
// line each instruction block starts with, validates its scripted JSON
// against the schema it was given (the real Extract does), and logs spend
// the way the CLI's log closure does, so Budget sees it.
type fakeLLM struct {
	t       *testing.T
	db      *store.DB
	perCall float64
	script  func(task, material string) (string, error)
	down    map[string]bool // provider → every call errors

	mu    sync.Mutex
	calls []call
}

type boundLLM struct {
	f                      *fakeLLM
	provider, model, runID string
}

func (f *fakeLLM) extractorFor(provider, _, model string, _ *extract.Schema, runID string) (extract.Extractor, error) {
	return &boundLLM{f: f, provider: provider, model: model, runID: runID}, nil
}

func (b *boundLLM) Name() string { return "fake" }

func (b *boundLLM) Extract(_ context.Context, in extract.ExtractInput) (extract.ExtractResult, error) {
	task := taskOf(in.PromptExtra)
	raw, err := b.f.answer(b, task, in.Markdown)
	if err != nil {
		return extract.ExtractResult{}, err
	}
	if verr := in.Schema.Validate([]byte(raw)); verr != nil {
		b.f.t.Errorf("script for %s breaks its schema: %v\n%s", task, verr, raw)
	}
	if lerr := b.f.db.LogLLMCall(b.runID, store.LLMCall{Provider: b.provider, Model: b.model,
		USDEstimate: b.f.perCall, Purpose: in.Purpose}); lerr != nil {
		b.f.t.Errorf("log llm call: %v", lerr)
	}
	return extract.ExtractResult{Raw: json.RawMessage(raw), Provider: b.provider, Model: b.model, Attempts: 1}, nil
}

// PromptText is the writer. scrape.Prompt logs it; a non-zero USDEstimate
// wins in extract.CostFor, so the write's cost lands too.
func (b *boundLLM) PromptText(_ context.Context, system, user string) (string, extract.TokenUsage, error) {
	raw, err := b.f.answer(b, taskOf(system), user)
	return raw, extract.TokenUsage{USDEstimate: b.f.perCall}, err
}

func (f *fakeLLM) answer(b *boundLLM, task, material string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{task, b.provider, b.model, material})
	down := f.down[b.provider]
	f.mu.Unlock()
	if down {
		return "", fmt.Errorf("fake: provider %s is down", b.provider)
	}
	return f.script(task, material)
}

func (f *fakeLLM) recorded(task string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Task == task {
			out = append(out, c)
		}
	}
	return out
}

func taskOf(s string) string { // "TASK: extract\n…" → "extract"
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimPrefix(line, "TASK: ")
}

// env is one hermetic research world.
type env struct {
	db     *store.DB
	web    *corpus
	search *searx
	llm    *fakeLLM
	deps   scrape.Deps
	base   int
}

func newEnv(t *testing.T, pages map[string]page, serp map[string][]string, script func(task, material string) (string, error)) *env {
	t.Helper()
	research.SetLimits(1000, 1000) // tests never sleep on pacing (export_test.go)
	t.Cleanup(func() { research.SetLimits(1, 3) })
	db, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() }) //nolint:errcheck // test cleanup
	e := &env{db: db, web: &corpus{pages: pages, hits: map[string]int{}}, search: newSearx(t, serp)}
	e.llm = &fakeLLM{t: t, db: db, perCall: 0.001, script: script, down: map[string]bool{}}
	e.deps = scrape.Deps{DB: db, ExtractorFor: e.llm.extractorFor,
		APIKeyFor: func(string) string { return "k" }, Fetcher: e.web}
	e.base = runtime.NumGoroutine() // after Open + servers: database/sql's opener is baseline
	return e
}

func (e *env) job(question string, mod func(*research.Job)) research.Job {
	j := research.Job{Question: question, Options: research.Options{
		Provider: "openai", Model: "gpt-4o-mini", MaxCostUSD: 1, Effort: "standard",
		Search: []string{"searxng"},
	}}
	if mod != nil {
		mod(&j)
	}
	return j
}

// settle: no goroutine outlives Run. Close the searx server's client conns
// first — scrape.Search's per-call transport keeps idle conns 90 s.
func (e *env) settle(t *testing.T) {
	t.Helper()
	e.search.CloseClientConnections()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > e.base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d > baseline %d\n%s", runtime.NumGoroutine(), e.base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
