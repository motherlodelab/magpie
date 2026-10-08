package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/config"
	"github.com/motherlodelab/magpie/extract"

	"github.com/spf13/cobra"
)

func resetGlobals() {
	cfgFile, cacheDB, apiKey, proxyFile = "", "", "", ""
	maxCost = 0
}

func captureOutput(t *testing.T, fn func()) (string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	fn()
	if err := wo.Close(); err != nil {
		t.Errorf("close stdout pipe: %v", err)
	}
	if err := we.Close(); err != nil {
		t.Errorf("close stderr pipe: %v", err)
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	out, err := io.ReadAll(ro)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	errOut, err := io.ReadAll(re)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	return string(out), string(errOut)
}

func crawlPageHTML(links ...string) string {
	var sb strings.Builder
	sb.WriteString(`<html><head><title>Widget - Buy</title></head><body><h1>Widget</h1><span class="price">12.99</span><nav>`)
	for _, l := range links {
		fmt.Fprintf(&sb, `<a href="%s">x</a>`, l)
	}
	sb.WriteString(`</nav><p>Plenty of honest descriptive prose keeps the static renderer in charge without any browser escalation at all.</p>`)
	sb.WriteString(`<p>A second paragraph of harmless filler text pushes visible length safely past the two-hundred character threshold.</p>`)
	sb.WriteString(`</body></html>`)
	return sb.String()
}

// writeFileSite creates index.html + pages linked from it; returns file:// URL.
func writeFileSite(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	names := []string{}
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("p%d.html", i))
	}
	var nav strings.Builder
	for _, nm := range names {
		fmt.Fprintf(&nav, `<a href="%s">x</a>`, nm)
	}
	prose := `<p>Plenty of honest descriptive prose keeps the static renderer in charge without any browser escalation at all.</p>`
	for _, nm := range names {
		html := `<html><head><title>Widget - Buy</title></head><body><h1>Widget</h1><span class="price">12.99</span>` + prose + `</body></html>`
		if err := os.WriteFile(filepath.Join(dir, nm), []byte(html), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	index := `<html><head><title>Widget - Buy</title></head><body><h1>Widget</h1><span class="price">12.99</span><nav>` + nav.String() + `</nav>` + prose + `</body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + abs
}

func newTestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func fakeLLM(t *testing.T, doc string) *fakeProvider {
	t.Helper()
	srv, fp := newFakeProvider(t, openAIEnvelope(doc))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")
	return fp
}

func priceSchema(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../testdata/extract/price.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestCrawl_FileSiteExit0(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	fp := fakeLLM(t, `{"name":"Widget","price":12.99}`)
	seed := writeFileSite(t, 3)
	out := filepath.Join(t.TempDir(), "r.jsonl")
	err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: out,
		MaxPages: 10, MaxDepth: 3, Concurrency: 4, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	raw := mustRead(t, out)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("jsonl lines = %d, want 4 (index + 3 pages)", len(lines))
	}
	schMustValidate(t, priceSchema(t), lines)
	db := mustOpenDB(t, dbPath)
	if n := mustCount(t, db, ""); n < 1 {
		t.Errorf("llm_calls = %d, want ≥1", n)
	}
	_ = fp
}

func schMustValidate(t *testing.T, schemaPath string, lines []string) {
	t.Helper()
	// Schema-valid per record: extracted.name == Widget, price == 12.99.
	for _, ln := range lines {
		var doc struct {
			Extracted map[string]any `json:"extracted"`
		}
		if err := json.Unmarshal([]byte(ln), &doc); err != nil {
			t.Fatalf("line not JSON: %v", err)
		}
		if doc.Extracted["name"] != "Widget" || doc.Extracted["price"] != 12.99 {
			t.Errorf("record = %v, want name=Widget price=12.99", doc.Extracted)
		}
	}
}

func TestCrawl_SecondRunZeroLLMCalls(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	seed := writeFileSite(t, 3)
	mkOpts := func(out string) crawlCLIOptions {
		return crawlCLIOptions{
			Schema: priceSchema(t), Format: "jsonl", Out: out,
			MaxPages: 10, MaxDepth: 3, Concurrency: 4, SameHost: true, Rate: 1000,
			Provider: "openai", Model: "gpt-4o-mini",
		}
	}
	if err := runCrawl(t.Context(), seed, mkOpts(filepath.Join(t.TempDir(), "r1.jsonl"))); err != nil {
		t.Fatalf("run1: %v", err)
	}
	dbPath := os.Getenv("MAGPIE_CACHE_DB")
	db := mustOpenDB(t, dbPath)
	n1, err := db.LLMCallCount("")
	if err != nil {
		t.Fatal(err)
	}
	if err := runCrawl(t.Context(), seed, mkOpts(filepath.Join(t.TempDir(), "r2.jsonl"))); err != nil {
		t.Fatalf("run2: %v", err)
	}
	n2, err := db.LLMCallCount("")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != n1 {
		t.Errorf("llm_calls delta = %d, want 0 (steady state cached)", n2-n1)
	}
}

func TestCrawl_ZeroRecordsExit3(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	out := filepath.Join(t.TempDir(), "r.jsonl")
	err := runCrawl(t.Context(), "file:///nonexistent-magpie.html", crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: out,
		MaxPages: 5, Concurrency: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	})
	if codeOf(err) != 3 {
		t.Fatalf("exit = %d, want 3 (err=%v)", codeOf(err), err)
	}
}

func TestCrawl_PartialExit4(t *testing.T) {
	testEnv(t, "cache.db")
	fp := fakeLLM(t, `{"name":"Widget","price":12.99}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/good", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(crawlPageHTML("/bad"))) //nolint:errcheck // httptest local; short write unactionable
	})
	mux.HandleFunc("/bad", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	srv := newTestServer(t, mux)
	out := filepath.Join(t.TempDir(), "r.jsonl")
	err := runCrawl(t.Context(), srv+"/good", crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: out,
		MaxPages: 5, MaxDepth: 1, Concurrency: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	})
	if codeOf(err) != 4 {
		t.Fatalf("exit = %d, want 4 (err=%v)", codeOf(err), err)
	}
	raw := mustRead(t, out)
	if len(strings.TrimSpace(string(raw))) == 0 {
		t.Error("no records despite one good page")
	}
	_ = fp
}

func TestCrawl_RobotsExit5(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	newOrigin := func(robots func(w http.ResponseWriter, r *http.Request)) (string, *int) {
		mux := http.NewServeMux()
		mux.HandleFunc("/robots.txt", robots)
		hits := 0
		mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
			hits++
			_, _ = w.Write([]byte(crawlPageHTML())) //nolint:errcheck // httptest local; short write unactionable
		})
		srv := newTestServer(t, mux)
		return srv + "/page", &hits
	}
	deny := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("User-agent: *\nDisallow: /\n")) } //nolint:errcheck // httptest local
	unavail := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "x", http.StatusServiceUnavailable)
	}

	for name, robots := range map[string]func(w http.ResponseWriter, r *http.Request){"deny": deny, "unreachable": unavail} {
		seed, hits := newOrigin(robots)
		err := runCrawl(t.Context(), seed, crawlCLIOptions{
			Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
			MaxPages: 5, Concurrency: 2, SameHost: true, Rate: 1000,
			Provider: "openai", Model: "gpt-4o-mini",
		})
		if codeOf(err) != 5 {
			t.Errorf("%s: exit = %d, want 5 (err=%v)", name, codeOf(err), err)
		}
		if *hits != 0 {
			t.Errorf("%s: page fetches = %d, want 0", name, *hits)
		}
	}
}

func TestCrawl_IgnoreRobotsWarns(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n")) //nolint:errcheck // httptest local
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(crawlPageHTML())) //nolint:errcheck // httptest local
	})
	srv := newTestServer(t, mux)
	_, stderr := captureOutput(t, func() {
		if cerr := runCrawl(t.Context(), srv+"/page", crawlCLIOptions{
			Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
			MaxPages: 2, MaxDepth: 0, Concurrency: 2, SameHost: true, Rate: 1000,
			Provider: "openai", Model: "gpt-4o-mini", IgnoreRobots: true,
		}); cerr != nil {
			t.Errorf("ignore-robots crawl: %v", cerr)
		}
	})
	if !strings.Contains(stderr, "WARNING: --ignore-robots") {
		t.Errorf("stderr missing ignore-robots warning; got %q", stderr)
	}
}

func TestCrawl_MaxCostExit6(t *testing.T) {
	testEnv(t, "cache.db")
	fp := fakeLLM(t, `{"name":"Widget","price":12.99}`)
	t.Setenv("MAGPIE_MAX_COST", "0.000001")
	seed := writeFileSite(t, 1)
	err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		MaxPages: 5, Concurrency: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	})
	if codeOf(err) != 6 {
		t.Fatalf("exit = %d, want 6 (err=%v)", codeOf(err), err)
	}
	if fp.callCount() != 0 {
		t.Errorf("provider calls = %d, want 0", fp.callCount())
	}
}

func TestCacheCmd_InspectClear(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	seed := writeFileSite(t, 3)
	if err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		MaxPages: 5, MaxDepth: 1, Concurrency: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	}); err != nil {
		t.Fatalf("crawl: %v", err)
	}
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"cache", "inspect", "--domain", "file"})
	stdout, _ := captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Errorf("inspect: %v", err)
		}
	})
	if !strings.Contains(stdout, "price") || !strings.Contains(stdout, "null_rate") {
		t.Errorf("inspect output missing selectors/rates; got %q", stdout)
	}
	resetGlobals()
	root2 := rootCmd()
	root2.SetArgs([]string{"cache", "clear", "--domain", "file"})
	captureOutput(t, func() {
		if err := root2.Execute(); err != nil {
			t.Errorf("clear: %v", err)
		}
	})
	resetGlobals()
	root3 := rootCmd()
	root3.SetArgs([]string{"cache", "inspect", "--domain", "file"})
	_, stderr := captureOutput(t, func() {
		if err := root3.Execute(); err != nil {
			t.Errorf("re-inspect: %v", err)
		}
	})
	if !strings.Contains(stderr, "no matching selectors") {
		t.Errorf("re-inspect stderr = %q, want cache miss note", stderr)
	}
}

func TestCacheCmd_HealTooFewSamples(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(crawlPageHTML())) //nolint:errcheck // httptest local
	})
	srv := newTestServer(t, mux)
	host := strings.TrimPrefix(srv, "http://")
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"cache", "heal", "--domain", host, "--schema", priceSchema(t), "--seed-url", srv + "/only", "--provider", "openai", "--model", "gpt-4o-mini"})
	_, stderr := captureOutput(t, func() {
		if err := root.Execute(); err == nil {
			t.Error("heal with 1 sample = nil error, want loud failure")
		}
	})
	if !strings.Contains(stderr, "at least 2") {
		t.Errorf("heal stderr = %q, want sample-count complaint", stderr)
	}
}

func TestScrape_FromCache(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	fp := fakeLLM(t, `{"name":"Widget","price":12.99}`)
	seed := writeFileSite(t, 2)
	if err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		MaxPages: 5, MaxDepth: 1, Concurrency: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	}); err != nil {
		t.Fatalf("warm crawl: %v", err)
	}
	before := fp.callCount()
	page := strings.TrimPrefix(seed, "file://")
	out := filepath.Join(t.TempDir(), "s.json")
	if err := runScrape(t.Context(), "file://"+page, scrapeOptions{
		Schema: priceSchema(t), Format: "json", Out: out, Render: "static",
		Provider: "openai", Model: "gpt-4o-mini",
	}); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	raw := mustRead(t, out)
	if !strings.Contains(string(raw), `"from_cache": true`) {
		t.Errorf("scrape output missing from_cache=true; got %s", raw)
	}
	if delta := fp.callCount() - before; delta != 0 {
		t.Errorf("scrape LLM calls = %d, want 0", delta)
	}
	db := mustOpenDB(t, dbPath)
	_ = db
}

func TestScrape_NoCacheForcesLLM(t *testing.T) {
	testEnv(t, "cache.db")
	fp := fakeLLM(t, `{"name":"Widget","price":12.99}`)
	seed := writeFileSite(t, 2)
	if err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		MaxPages: 5, MaxDepth: 1, Concurrency: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
	}); err != nil {
		t.Fatalf("warm crawl: %v", err)
	}
	before := fp.callCount()
	page := strings.TrimPrefix(seed, "file://")
	out := filepath.Join(t.TempDir(), "s.json")
	if err := runScrape(t.Context(), "file://"+page, scrapeOptions{
		Schema: priceSchema(t), Format: "json", Out: out, Render: "static",
		Provider: "openai", Model: "gpt-4o-mini", NoCache: true,
	}); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if delta := fp.callCount() - before; delta < 1 {
		t.Errorf("scrape LLM calls = %d, want ≥1 with --no-cache", delta)
	}
}

func TestCrawl_FormatsCSVSQLiteJSON(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	seed := writeFileSite(t, 1) // index + p0 = 2 pages
	dbPath := os.Getenv("MAGPIE_CACHE_DB")
	mkOpts := func(format, out string) crawlCLIOptions {
		return crawlCLIOptions{
			Schema: priceSchema(t), Format: format, Out: out,
			MaxPages: 5, MaxDepth: 1, Concurrency: 2, SameHost: true, Rate: 1000,
			Provider: "openai", Model: "gpt-4o-mini",
		}
	}
	csvOut := filepath.Join(t.TempDir(), "r.csv")
	if err := runCrawl(t.Context(), seed, mkOpts("csv", csvOut)); err != nil {
		t.Fatalf("csv crawl: %v", err)
	}
	raw := strings.Split(strings.TrimSpace(string(mustRead(t, csvOut))), "\n")
	if raw[0] != "name,price" {
		t.Errorf("csv header = %q, want required-then-sorted name,price", raw[0])
	}
	if len(raw) != 3 {
		t.Errorf("csv lines = %d, want 3 (header + 2 records)", len(raw))
	}
	jsonOut := filepath.Join(t.TempDir(), "r.json")
	if err := runCrawl(t.Context(), seed, mkOpts("json", jsonOut)); err != nil {
		t.Fatalf("json crawl: %v", err)
	}
	var arr []any
	if err := json.Unmarshal(mustRead(t, jsonOut), &arr); err != nil || len(arr) != 2 {
		t.Errorf("json array len = %d, want 2 (err=%v)", len(arr), err)
	}
	if err := runCrawl(t.Context(), seed, mkOpts("sqlite", "")); err != nil {
		t.Fatalf("sqlite crawl: %v", err)
	}
	db := mustOpenDB(t, dbPath)
	n, err := db.TableCount("records")
	if err != nil || n != 2 {
		t.Errorf("records rows = %d,%v want 2", n, err)
	}
}

func TestCacheCmd_HealSuccess(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		links := ""
		if r.URL.Path == "/seed" {
			links = `<a href="/second">x</a>`
		}
		_, _ = w.Write([]byte(`<html><body><h1>Widget</h1><span class="price">12.99</span>` + links + `</body></html>`)) //nolint:errcheck // httptest local
	})
	srv := newTestServer(t, mux)
	host := strings.TrimPrefix(srv, "http://")
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"cache", "heal", "--domain", host, "--schema", priceSchema(t), "--seed-url", srv + "/seed", "--provider", "openai", "--model", "gpt-4o-mini"})
	_, stderr := captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Errorf("heal: %v", err)
		}
	})
	if !strings.Contains(stderr, "healed "+host) {
		t.Errorf("heal stderr = %q, want healed note", stderr)
	}
}

// ---- LLM providers (codex exec + openrouter + opencode go/zen) ----

// stubCodexCmdScript is a minimal canned `codex`: preflight answers plus
// one valid record via the -o file (mirrors the extract-package stub).
const stubCodexCmdScript = `#!/bin/sh
LOG="$CALL_LOG"
echo "argv: $*" >> "$LOG"
cat >> "$LOG"
if [ "$1" = "--help" ]; then
  echo "Usage: codex exec [...] --output-schema <file> --json [...]"
  exit 0
fi
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
echo '{"type":"thread.started","thread_id":"t"}'
echo '{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}'
echo '{"name":"Widget","price":12.99}' > "$out"
exit 0
`

func stubCodexCmd(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(stubCodexCmdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CALL_LOG", filepath.Join(dir, "calls.log"))
}

func scrapeOne(t *testing.T, provider, model string) error {
	t.Helper()
	seed := writeFileSite(t, 1)
	out := filepath.Join(t.TempDir(), "s.json")
	return runScrape(t.Context(), seed, scrapeOptions{
		Schema: priceSchema(t), Format: "json", Out: out, Render: "static",
		Provider: provider, Model: model,
	})
}

func TestProvider_UnknownErrors(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_WAT_API_KEY", "x") // reach the switch past the key check
	err := scrapeOne(t, "wat", "m")
	if err == nil || !strings.Contains(err.Error(), "wat") {
		t.Fatalf("expected error naming the provider, got %v (silent default bills Anthropic)", err)
	}
	if codeOf(err) != 2 {
		t.Errorf("exit = %d, want 2", codeOf(err))
	}
}

func TestProvider_CodexNeedsNoKey(t *testing.T) {
	testEnv(t, "cache.db")
	stubCodexCmd(t)
	t.Setenv("MAGPIE_API_KEY", "")
	t.Setenv("MAGPIE_CODEX_API_KEY", "")
	if err := scrapeOne(t, "codex", "gpt-5.2"); err != nil {
		t.Fatalf("codex without key: %v (want keyless preflight+extract)", err)
	}
}

func TestProvider_OllamaNeedsNoKey(t *testing.T) {
	testEnv(t, "cache.db")
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	if err := scrapeOne(t, "ollama", "llama3.1"); err != nil {
		t.Fatalf("ollama without key: %v", err)
	}
}

func TestProvider_OpenRouterNeedsKey(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_API_KEY", "")
	t.Setenv("MAGPIE_OPENROUTER_API_KEY", "")
	if err := scrapeOne(t, "openrouter", "openai/gpt-4o-mini"); codeOf(err) != 7 {
		t.Fatalf("exit = %d, want 7 (err=%v)", codeOf(err), err)
	}
}

func TestProvider_DefaultModels(t *testing.T) {
	for _, tc := range []struct{ provider, want string }{
		{"anthropic", "claude-sonnet-5"},
		{"openai", "gpt-4o-mini"},
		{"ollama", "llama3.1"},
		{"openrouter", "openai/gpt-4o-mini"},
		{"codex", "gpt-5.2"},
		{"opencode-go", "glm-5.3"},
		{"opencode-zen", "claude-sonnet-4-6"},
	} {
		if got := config.DefaultModel(tc.provider); got != tc.want {
			t.Errorf("DefaultModel(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

func TestSetKey_DashMessage(t *testing.T) {
	if err := config.SetKey("opencode-go", "bogus"); err != nil {
		if !strings.Contains(err.Error(), "MAGPIE_OPENCODE_GO_API_KEY") {
			t.Errorf("SetKey error = %q, want dash-to-underscore env name", err.Error())
		}
	}
	// Read path uses the same transform (white-box: env name is the contract).
	t.Setenv("MAGPIE_OPENCODE_GO_API_KEY", "k")
	if got := (config.Config{}).APIKey("opencode-go"); got != "k" {
		t.Errorf("APIKey(opencode-go) = %q, want env hit (dash→underscore)", got)
	}
}

func TestMaxCost_FlatRateExempt(t *testing.T) {
	// opencode-go leg: flat plan proceeds despite a near-zero ceiling.
	testEnv(t, "cache.db")
	srv, fp := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENCODE_GO_API_KEY", "k")
	t.Setenv("MAGPIE_MAX_COST", "0.000001")
	if err := scrapeOne(t, "opencode-go", "glm-5.3"); err != nil {
		t.Fatalf("opencode-go with tiny ceiling: %v (want exemption)", err)
	}
	if fp.callCount() < 1 {
		t.Error("opencode-go provider hits = 0, want ≥1 (exempt path proceeds)")
	}
	// codex leg: subscription spend proceeds too, keyless.
	testEnv(t, "cache.db")
	stubCodexCmd(t)
	t.Setenv("MAGPIE_MAX_COST", "0.000001")
	t.Setenv("MAGPIE_API_KEY", "")
	if err := scrapeOne(t, "codex", "gpt-5.2"); err != nil {
		t.Fatalf("codex with tiny ceiling: %v (want exemption)", err)
	}
	// metered control: openai aborts pre-call with 0 hits.
	testEnv(t, "cache.db")
	srv2, fp2 := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv2.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "k")
	t.Setenv("MAGPIE_MAX_COST", "0.000001")
	err := scrapeOne(t, "openai", "gpt-4o-mini")
	if codeOf(err) != 6 {
		t.Fatalf("exit = %d, want 6 (err=%v)", codeOf(err), err)
	}
	if fp2.callCount() != 0 {
		t.Errorf("metered provider calls = %d, want 0 (aborted pre-call)", fp2.callCount())
	}
}

func TestOpenRouter_CostRow(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	srv, _ := newFakeProvider(t, openAIEnvelopeCost(`{"name":"Widget","price":12.99}`, 0.0042))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENROUTER_API_KEY", "test-key")
	if err := scrapeOne(t, "openrouter", "anthropic/claude-sonnet-4-6"); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	db := mustOpenDB(t, dbPath)
	calls, err := db.LLMCalls("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range calls {
		if c.Provider == "openrouter" {
			found = true
			if c.USDEstimate != 0.0042 {
				t.Errorf("usd_estimate = %v, want 0.0042 from usage.cost", c.USDEstimate)
			}
		}
	}
	if !found {
		t.Errorf("no openrouter row in llm_calls (%d rows)", len(calls))
	}
}

func TestZen_SessionHeaders(t *testing.T) {
	// chat/completions leg on the Go base.
	testEnv(t, "cache.db")
	srv, fp := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENCODE_GO_API_KEY", "k")
	if err := scrapeOne(t, "opencode-go", "glm-5.3"); err != nil {
		t.Fatalf("go chat leg: %v", err)
	}
	if got := fp.lastHeader("x-opencode-session"); got == "" {
		t.Error("go leg missing x-opencode-session header")
	}
	if !strings.Contains(fp.lastHeader("User-Agent"), "magpie") {
		t.Errorf("go leg UA = %q, want magpie", fp.lastHeader("User-Agent"))
	}
	// /messages leg on the Zen base (fresh DB: file-domain cache is per-DB).
	testEnv(t, "cache.db")
	srv2, fp2 := newFakeProvider(t, anthropicEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv2.URL)
	t.Setenv("MAGPIE_OPENCODE_ZEN_API_KEY", "k")
	if err := scrapeOne(t, "opencode-zen", "claude-sonnet-4-6"); err != nil {
		t.Fatalf("zen messages leg: %v", err)
	}
	if got := fp2.lastHeader("x-opencode-session"); got == "" {
		t.Error("zen leg missing x-opencode-session header")
	}
	if !strings.Contains(fp2.lastHeader("User-Agent"), "magpie") {
		t.Errorf("zen leg UA = %q, want magpie", fp2.lastHeader("User-Agent"))
	}
}

func TestProviderHelp_ListsAll(t *testing.T) {
	resetGlobals()
	ids := []string{"anthropic", "openai", "ollama", "openrouter", "codex", "opencode-go", "opencode-zen"}
	root := rootCmd()
	usages := map[string]string{}
	for _, c := range root.Commands() {
		if f := c.Flags().Lookup("provider"); f != nil {
			usages[c.Name()] = f.Usage
		}
		for _, sub := range c.Commands() {
			if f := sub.Flags().Lookup("provider"); f != nil {
				usages[c.Name()+" "+sub.Name()] = f.Usage
			}
		}
	}
	for _, cmd := range []string{"scrape", "crawl", "extract", "cache heal", "research"} {
		u, ok := usages[cmd]
		if !ok {
			t.Fatalf("no --provider flag found on %s (have %v)", cmd, usages)
		}
		for _, id := range ids {
			if !strings.Contains(u, id) {
				t.Errorf("%s --provider help missing %q (got %q)", cmd, id, u)
			}
		}
	}
}

// --- Phase D additions: crawl scope flags, bad-glob exit 2, SSRF exit-2
// wiring, --no-sitemap e2e. ---

func TestCrawlFlags_HelpText(t *testing.T) {
	resetGlobals()
	root := rootCmd()
	var crawlCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "crawl" {
			crawlCmd = c
			break
		}
	}
	if crawlCmd == nil {
		t.Fatal("no crawl command on the root")
	}
	for _, flag := range []string{"path-prefix", "include", "exclude", "allow-subdomains", "no-sitemap", "corpus"} {
		if crawlCmd.Flags().Lookup(flag) == nil {
			t.Errorf("crawl is missing --%s", flag)
		}
	}
}

func TestCrawl_BadGlobExit2(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	// 200 ** pairs blow the ≤4 cap; the seed points at a closed port so any
	// dial attempt would be the WRONG failure — the glob must be rejected
	// before any I/O.
	seed := "http://" + closedPortCLI(t) + "/"
	err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		MaxPages: 2, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini",
		Include: []string{strings.Repeat("a/**/b/", 200) + "c"},
	})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "**") {
		t.Errorf("error %v does not name the glob problem", err)
	}
}

// closedPortCLI is the cli-local copy of crawl's closedPort helper (the
// crawl package's is internal to its own test build).
func closedPortCLI(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestScrape_PrivateExit2(t *testing.T) {
	testEnv(t, "cache.db")
	// MAGPIE_STRICT_SSRF=1 opts the test binary OUT of the test-binary
	// relaxation, so the production-strict path is exercised end to end:
	// pre-dial rejection, no listener needed (127.0.0.1:9 is unroutable).
	t.Setenv("MAGPIE_STRICT_SSRF", "1")
	err := runScrape(t.Context(), "http://127.0.0.1:9/", scrapeOptions{
		Format: "json", Out: filepath.Join(t.TempDir(), "s.json"), Render: "static",
	})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error %v does not name the blocked address", err)
	}
}

func TestCrawl_NoSitemapZeroFetch_CLI(t *testing.T) {
	testEnv(t, "cache.db")
	fakeLLM(t, `{"name":"Widget","price":12.99}`)
	sitemapHits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow:\nSitemap: /sitemap.xml\n")) //nolint:errcheck // httptest local
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		sitemapHits++
		_, _ = w.Write([]byte(`<urlset><url><loc>never-fetched</loc></url></urlset>`)) //nolint:errcheck // httptest local
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(crawlPageHTML())) //nolint:errcheck // httptest local
	})
	srv := newTestServer(t, mux)
	err := runCrawl(t.Context(), srv+"/page", crawlCLIOptions{
		Schema: priceSchema(t), Format: "jsonl", Out: filepath.Join(t.TempDir(), "r.jsonl"),
		MaxPages: 2, MaxDepth: 0, SameHost: true, Rate: 1000,
		Provider: "openai", Model: "gpt-4o-mini", NoSitemap: true,
	})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if sitemapHits != 0 {
		t.Errorf("sitemap hits = %d, want 0 with --no-sitemap", sitemapHits)
	}
}

// --- Phase G: --proxy-file flag + search command. ---

// TestProxyFileFlag_MalformedExit2: a malformed pool file fails with
// exit 2 naming the line number, and nothing is dialed (origin counter
// frozen at zero — the parse error precedes any request).
func TestProxyFileFlag_MalformedExit2(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_PROXY_FILE", "") // the flag's os.Setenv must not leak past this test
	resetGlobals()
	var originHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits++
		_, _ = w.Write([]byte("never served")) //nolint:errcheck // test server
	}))
	t.Cleanup(origin.Close)
	pool := filepath.Join(t.TempDir(), "pool.txt")
	if err := os.WriteFile(pool, []byte("# header\nhttp://ok.example:3128\nnot-a-proxy-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := rootCmd()
	root.SetArgs([]string{"scrape", origin.URL, "--render", "static", "--proxy-file", pool})
	var err error
	captureOutput(t, func() {
		err = root.Execute()
	})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("err = %v, want the pool line number", err)
	}
	if originHits != 0 {
		t.Errorf("origin hits = %d, want 0 (malformed pool fails pre-I/O)", originHits)
	}
	resetGlobals()
}

// TestProxyFileFlag_ValidServes: a valid single-entry pool rides the
// proxy and the scrape exits 0.
func TestProxyFileFlag_ValidServes(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_PROXY_FILE", "") // the flag's os.Setenv must not leak past this test
	resetGlobals()
	var proxyHits int
	proxied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		p := httputil.NewSingleHostReverseProxy(mustURL(t, "http://"+r.Host))
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(proxied.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>via pool</title></head><body><p>" + strings.Repeat("honest descriptive prose for the cleaner ", 20) + "</p></body></html>")) //nolint:errcheck // test server
	}))
	t.Cleanup(origin.Close)
	pool := filepath.Join(t.TempDir(), "pool.txt")
	if err := os.WriteFile(pool, []byte("http://"+strings.TrimPrefix(proxied.URL, "http://")), 0o600); err != nil {
		t.Fatal(err)
	}
	root := rootCmd()
	root.SetArgs([]string{"scrape", origin.URL, "--render", "static", "--proxy-file", pool})
	captureOutput(t, func() {
		if err := root.Execute(); err != nil {
			t.Errorf("scrape with pool: %v", err)
		}
	})
	if proxyHits == 0 {
		t.Error("proxy hits = 0, want ≥1 (the pool must serve the request)")
	}
	resetGlobals()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestSearchCmd_Validation: exits only — the endpoint seam lives in
// scrape's internal tests, the CLI contract is codes + hint text.
func TestSearchCmd_Validation(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_PROXY_FILE", "") // the flag's os.Setenv must not leak past this test
	t.Setenv("MAGPIE_BRAVE_API_KEY", "")
	t.Setenv("MAGPIE_API_KEY", "")

	// Unknown provider → exit 2 naming the valid set.
	resetGlobals()
	root := rootCmd()
	root.SetArgs([]string{"search", "q", "--provider", "altavista"})
	var err error
	captureOutput(t, func() {
		err = root.Execute()
	})
	if codeOf(err) != 2 {
		t.Errorf("bogus provider exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	for _, p := range []string{"brave", "duckduckgo", "searxng"} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("err %q must name %q", err, p)
		}
	}
	resetGlobals()

	// Missing key → exit 7 with the set-key hint.
	root2 := rootCmd()
	root2.SetArgs([]string{"search", "q", "--provider", "brave"})
	_, stderr2 := captureOutput(t, func() {
		err = root2.Execute()
	})
	if codeOf(err) != 7 {
		t.Errorf("missing-key exit = %d, want 7 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(stderr2, "MAGPIE_BRAVE_API_KEY") || !strings.Contains(stderr2, "config set-key brave") {
		t.Errorf("stderr = %q, want the engine's own key hint (QA ST9)", stderr2)
	}
	resetGlobals()

	// Negative scrape-top → exit 2.
	root3 := rootCmd()
	root3.SetArgs([]string{"search", "q", "--scrape-top", "-1"})
	captureOutput(t, func() {
		err = root3.Execute()
	})
	if codeOf(err) != 2 {
		t.Errorf("scrape-top -1 exit = %d, want 2", codeOf(err))
	}
	resetGlobals()

	// Help lists the flags (contract surface for docs/GUI).
	root4 := rootCmd()
	root4.SetArgs([]string{"search", "--help"})
	out, _ := captureOutput(t, func() {
		if err := root4.Execute(); err != nil {
			t.Errorf("help: %v", err)
		}
	})
	for _, want := range []string{"--provider", "--limit", "--scrape-top", "--out"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
	resetGlobals()
}

// --- Phase I: corpus mode (schema-less RAG ingestion) ---

func TestCrawl_CorpusCLI(t *testing.T) {
	testEnv(t, "cache.db") // keyless: no fakeLLM, no MAGPIE_* key env, no --schema
	seed := writeFileSite(t, 3)
	out := filepath.Join(t.TempDir(), "c.jsonl")
	err := runCrawl(t.Context(), seed, crawlCLIOptions{
		Corpus: true, Format: "jsonl", Out: out,
		MaxPages: 10, MaxDepth: 3, Concurrency: 4, SameHost: true, Rate: 1000,
	})
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	raw := mustRead(t, out)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("jsonl lines = %d, want 4 (index + 3 pages)", len(lines))
	}
	for i, ln := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("line %d not JSON: %v", i, err)
		}
		if len(rec) != 4 {
			t.Errorf("line %d has %d keys, want exactly url,title,depth,markdown", i, len(rec))
		}
		for _, k := range []string{"url", "title", "depth", "markdown"} {
			if _, ok := rec[k]; !ok {
				t.Errorf("line %d missing key %q", i, k)
			}
		}
		if md, _ := rec["markdown"].(string); strings.TrimSpace(md) == "" {
			t.Errorf("line %d has empty markdown", i)
		}
	}
}

func TestCrawl_CorpusCLIViolations(t *testing.T) {
	testEnv(t, "cache.db")
	seed := writeFileSite(t, 1)
	rows := []crawlCLIOptions{
		{Corpus: true, Schema: priceSchema(t), Format: "jsonl"}, // corpus + schema
		{Corpus: true, Format: "csv"},
		{Corpus: true, Format: "json"},
		{Corpus: true, Format: "sqlite"},
	}
	for i, o := range rows {
		err := runCrawl(t.Context(), seed, o)
		if codeOf(err) != 2 {
			t.Errorf("row %d exit = %d (%v), want 2", i, codeOf(err), err)
		}
		if !strings.Contains(err.Error(), "corpus") {
			t.Errorf("row %d error %v does not name corpus", i, err)
		}
	}
}

// TestLLMProvidersMatchExtract (QA ST9): config is a leaf and keeps its own
// copy of the LLM provider names (the generic key's allowlist); this drift
// test fails the day extract gains a provider config doesn't know.
func TestLLMProvidersMatchExtract(t *testing.T) {
	got := slices.Sorted(slices.Values(config.LLMProviders()))
	want := slices.Sorted(slices.Values(extract.ProviderNames()))
	if !slices.Equal(got, want) {
		t.Errorf("config.LLMProviders() = %v, extract.ProviderNames() = %v: keep them in step", got, want)
	}
}
