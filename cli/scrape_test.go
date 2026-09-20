package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

type fakeProvider struct {
	t      *testing.T
	mu     sync.Mutex
	script []string
	calls  int
	bodies []string
	// headers parallels bodies: one cloned header map per call.
	headers []http.Header
}

func newFakeProvider(t *testing.T, script ...string) (*httptest.Server, *fakeProvider) {
	t.Helper()
	// Hermetic by contract: ambient MAGPIE_* env (real endpoints/keys)
	// must never redirect the fake provider's adapter.
	t.Setenv("MAGPIE_BASE_URL", "")
	t.Setenv("MAGPIE_OPENAI_API_KEY", "")
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "")
	t.Setenv("MAGPIE_OLLAMA_URL", "")
	fp := &fakeProvider{t: t, script: script}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			http.Error(w, rerr.Error(), http.StatusBadRequest)
			return
		}
		fp.mu.Lock()
		defer fp.mu.Unlock()
		fp.calls++
		fp.bodies = append(fp.bodies, string(body))
		fp.headers = append(fp.headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		idx := min(fp.calls-1, len(fp.script)-1)
		if _, werr := io.WriteString(w, fp.script[idx]); werr != nil {
			fp.t.Errorf("write: %v", werr)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, fp
}

func (f *fakeProvider) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func (f *fakeProvider) lastHeader(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) == 0 {
		return ""
	}
	return f.headers[len(f.headers)-1].Get(key)
}

func openAIEnvelope(raw string) string {
	b, merr := json.Marshal(raw)
	if merr != nil {
		panic(merr) // marshaling a string cannot fail
	}
	return `{"choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`
}

// openAIEnvelopeCost adds provider-computed usage.cost (the OpenRouter shape).
func openAIEnvelopeCost(raw string, cost float64) string {
	b, merr := json.Marshal(raw)
	if merr != nil {
		panic(merr) // marshaling a string cannot fail
	}
	cb, merr := json.Marshal(cost) // canonical float rendering
	if merr != nil {
		panic(merr) // marshaling a float cannot fail
	}
	return `{"choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"cost":` + string(cb) + `}}`
}

func anthropicEnvelope(raw string) string {
	b, merr := json.Marshal(raw)
	if merr != nil {
		panic(merr) // marshaling a string cannot fail
	}
	return `{"content":[{"type":"text","text":` + string(b) + `}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10}}`
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustOpenDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	return db
}

func mustCount(t *testing.T, db *store.DB, run string) int {
	t.Helper()
	n, err := db.LLMCallCount(run)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func testEnv(t *testing.T, dbName string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("APPDATA", "")
	db := filepath.Join(dir, dbName)
	t.Setenv("MAGPIE_CACHE_DB", db)
	return db
}

func codeOf(err error) int {
	if err == nil {
		return 0
	}
	return exitCode(err) // tests assert the same mapping the process edge uses
}

func TestScrapeNoSchema(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	out := filepath.Join(t.TempDir(), "out.json")
	err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "json", Out: out, Render: "static"})
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	raw := mustRead(t, out)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("stdout not JSON: %v", err)
	}
	if _, ok := doc["markdown"]; !ok {
		t.Error("missing markdown field")
	}
	db := mustOpenDB(t, dbPath)
	n := mustCount(t, db, "")
	if n != 0 {
		t.Errorf("llm_calls = %d, want 0", n)
	}
}

func TestScrapeEndToEndFileURL(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	srv, fp := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")
	abs := mustAbs(t, "../testdata/clean/article.html")
	schema := mustAbs(t, "../testdata/extract/price.yaml")
	out := filepath.Join(t.TempDir(), "out.json")
	err := runScrape(t.Context(), "file://"+abs, scrapeOptions{
		Schema: schema, Format: "json", Out: out, Render: "static",
		Provider: "openai", Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if fp.callCount() < 1 {
		t.Fatalf("provider calls = 0, want ≥1")
	}
	raw := mustRead(t, out)
	var doc struct {
		Extracted map[string]any `json:"extracted"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("stdout not JSON: %v", err)
	}
	if doc.Extracted["price"] != 12.99 {
		t.Errorf("price = %v, want 12.99", doc.Extracted["price"])
	}
	db := mustOpenDB(t, dbPath)
	n := mustCount(t, db, "")
	if n < 1 {
		t.Errorf("llm_calls = %d, want ≥1", n)
	}
}

func TestScrapeRenderStaticSPAShell(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/spa-shell.html")
	out := filepath.Join(t.TempDir(), "out.json")
	// Static render must never touch the browser (no Chrome here).
	if err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "json", Out: out, Render: "static"}); err != nil {
		t.Fatalf("scrape spa-shell static: %v", err)
	}
}

func TestScrapeBadFormat(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "csv", Render: "static"})
	if codeOf(err) == 0 {
		t.Fatal("expected non-zero exit for csv")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %v, want 'not supported'", err)
	}
}

func TestMaxCostAbortsBeforeCall(t *testing.T) {
	testEnv(t, "cache.db")
	srv, fp := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")
	t.Setenv("MAGPIE_MAX_COST", "0.000001")
	abs := mustAbs(t, "../testdata/clean/article.html")
	schema := mustAbs(t, "../testdata/extract/price.yaml")
	err := runScrape(t.Context(), "file://"+abs, scrapeOptions{
		Schema: schema, Render: "static", Provider: "openai", Model: "gpt-4o-mini",
	})
	if codeOf(err) != 6 {
		t.Fatalf("exit = %d, want 6 (err=%v)", codeOf(err), err)
	}
	if fp.callCount() != 0 {
		t.Errorf("provider calls = %d, want 0", fp.callCount())
	}
}

func TestMissingKeyExit7(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_OPENAI_API_KEY", "")
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "")
	t.Setenv("MAGPIE_API_KEY", "")
	abs := mustAbs(t, "../testdata/clean/article.html")
	schema := mustAbs(t, "../testdata/extract/price.yaml")
	err := runScrape(t.Context(), "file://"+abs, scrapeOptions{
		Schema: schema, Render: "static", Provider: "openai", Model: "gpt-4o-mini",
	})
	if codeOf(err) != 7 {
		t.Fatalf("exit = %d, want 7 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "magpie config set-key") {
		t.Errorf("error = %v, want set-key hint", err)
	}
}

func TestExtractCmdStdinHTML(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")
	html := mustRead(t, "../testdata/clean/article.html")
	// Simulate stdin via temp file (no fetch involved).
	in := filepath.Join(t.TempDir(), "in.html")
	if err := os.WriteFile(in, html, 0o644); err != nil {
		t.Fatal(err)
	}
	schema := mustAbs(t, "../testdata/extract/price.yaml")
	out := filepath.Join(t.TempDir(), "out.json")
	err := runExtract(t.Context(), extractOptions{
		Schema: schema, ContentType: "html", Provider: "openai",
		Model: "gpt-4o-mini", Out: out, File: in,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	raw := mustRead(t, out)
	var doc struct {
		Extracted map[string]any `json:"extracted"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("stdout not JSON: %v", err)
	}
	if doc.Extracted["price"] != 12.99 {
		t.Errorf("price = %v", doc.Extracted["price"])
	}
	db := mustOpenDB(t, dbPath)
	n := mustCount(t, db, "")
	if n < 1 {
		t.Errorf("llm_calls = %d, want ≥1", n)
	}
}

func TestScrape_Exit8(t *testing.T) {
	testEnv(t, "cache.db")
	raw, err := os.ReadFile("../testdata/quality/challenge-akamai.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(raw) //nolint:errcheck // httptest local; short write unactionable
	}))
	t.Cleanup(srv.Close)
	err = runScrape(t.Context(), srv.URL, scrapeOptions{Format: "json", Render: "static"})
	if codeOf(err) != 8 {
		t.Fatalf("exit = %d, want 8 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "quality blocked (access-denied)") {
		t.Errorf("error = %v, want typed issue", err)
	}
}

func TestScrape_PageFormatFlags(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	out := filepath.Join(t.TempDir(), "out.txt")
	if err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "json", Render: "static", PageFormat: "llm", Out: out}); err != nil {
		t.Fatalf("llm: %v", err)
	}
	if body := string(mustRead(t, out)); !strings.Contains(body, "## Links") {
		t.Errorf("llm output lacks ## Links:\n%s", body)
	}
	if err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "json", Render: "static", PageFormat: "bogus"}); codeOf(err) != 2 {
		t.Errorf("bogus page-format exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
}

func TestScrape_PageFormatLeavesConfig(t *testing.T) {
	// --page-format llm must not trip crawl's Config.Format validation:
	// exit 0 proves the flag stayed out of cfg.Format.
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	out := filepath.Join(t.TempDir(), "out.txt")
	if err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "jsonl", Render: "static", PageFormat: "llm", Out: out}); err != nil {
		t.Fatalf("scrape: %v", err)
	}
}

func TestScrape_ScopeAndProfileFlags(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	out := filepath.Join(t.TempDir(), "out.txt")
	if err := runScrape(t.Context(), "file://"+abs, scrapeOptions{
		Format: "json", Render: "static", Include: []string{"article"},
		HeaderProfile: "firefox", Cookies: "a=b", Out: out,
	}); err != nil {
		t.Fatalf("scrape: %v", err)
	}
}

func TestScrape_VerticalUnknown(t *testing.T) {
	testEnv(t, "cache.db")
	// Nonexistent file: a fetch attempt would exit 1, so exit 2 proves
	// unknown-name validation runs pre-I/O.
	err := runScrape(t.Context(), "file:///nonexistent-vertical-probe.html", scrapeOptions{Format: "json", Render: "static", Vertical: "tumblr"})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "tumblr") {
		t.Errorf("error = %v, want name", err)
	}
}

func TestScrape_VerticalMismatch(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "json", Render: "static", Vertical: "reddit"})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if !strings.Contains(err.Error(), "does not handle") {
		t.Errorf("error = %v, want 'does not handle'", err)
	}
}

func TestScrape_VerticalHelp(t *testing.T) {
	usage := newScrapeCmd().UsageString()
	if !strings.Contains(usage, "auto") {
		t.Errorf("scrape usage lacks 'auto':\n%s", usage)
	}
	if !strings.Contains(usage, "default off") {
		t.Errorf("scrape usage lacks default-off note:\n%s", usage)
	}
}

// TestScrape_ScreenshotDoc: without --out the screenshot must surface
// the base64 PNG in a JSON envelope (content field) — regression for
// the silent-drop bug where the base64 never reached any output.
func TestScrape_ScreenshotDoc(t *testing.T) {
	png := "iVBORw0KGgo" // PNG magic, base64-shaped; no capture needed
	doc, err := screenshotDoc(scrape.Result{URL: "https://x", FinalURL: "https://x", Rendered: png})
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(doc), &env); err != nil {
		t.Fatalf("envelope not JSON: %v", doc)
	}
	if env["content"] != png {
		t.Errorf("content = %q, want the base64 PNG", env["content"])
	}
	if env["page_format"] != "screenshot" {
		t.Errorf("page_format = %q, want screenshot", env["page_format"])
	}
}

// TestScrape_PageFormatRawAndHTML: --page-format raw|html must print the
// rendered page content itself — the markdown envelope has no field for
// it, so falling through there silently dropped the requested content
// (same silent-drop class as the screenshot regression).
func TestScrape_PageFormatRawAndHTML(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	want, err := os.ReadFile("../testdata/clean/article.html")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.txt")

	err = runScrape(t.Context(), "file://"+abs, scrapeOptions{PageFormat: "raw", Out: out, Render: "static"})
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	if got := mustRead(t, out); string(got) != string(want) {
		t.Errorf("raw output %d bytes, want byte-equal body (%d)", len(got), len(want))
	}

	err = runScrape(t.Context(), "file://"+abs, scrapeOptions{PageFormat: "html", Out: out, Render: "static"})
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	htmlOut := mustRead(t, out)
	if len(htmlOut) == 0 || !strings.Contains(string(htmlOut), "<") {
		t.Errorf("html output empty or not markup (%d bytes)", len(htmlOut))
	}
}

// TestScrape_XHREnvelope — Phase J: the markdown envelope carries the
// xhr array only when captures exist (additive-omitempty: existing
// envelopes byte-identical). Constructed Result — the envelope function
// IS the output contract; no fetcher fake behind it (screenshotDoc
// precedent).
func TestScrape_XHREnvelope(t *testing.T) {
	canned := []fetch.XHRCapture{{
		URL: "https://x/api/data", Status: 200,
		MIMEType: "application/json", Body: `{"ok":true}`,
	}}
	doc, err := markdownDoc(scrape.Result{URL: "https://x", FinalURL: "https://x", Title: "t", XHR: canned})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		URL string             `json:"url"`
		XHR []fetch.XHRCapture `json:"xhr"`
	}
	if err := json.Unmarshal([]byte(doc), &env); err != nil {
		t.Fatalf("envelope not JSON: %v", doc)
	}
	if len(env.XHR) != 1 || env.XHR[0].URL != "https://x/api/data" ||
		env.XHR[0].Status != 200 || env.XHR[0].MIMEType != "application/json" ||
		env.XHR[0].Body != `{"ok":true}` {
		t.Errorf("xhr = %+v, want the canned capture intact", env.XHR)
	}

	// nil XHR → no xhr key at all (additiveness: no envelope drift).
	doc, err = markdownDoc(scrape.Result{URL: "https://x", FinalURL: "https://x", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("envelope not JSON: %v", doc)
	}
	if _, present := raw["xhr"]; present {
		t.Error("nil XHR must omit the xhr key entirely")
	}
}

// TestScrape_XHRJSONEnvelope — the page-format json path re-marshals the
// page's own JSON to inject the xhr array; UseNumber must keep the
// original number literals verbatim (plain Unmarshal rounds through
// float64, silently corrupting IDs above 2^53).
func TestScrape_XHRJSONEnvelope(t *testing.T) {
	rendered := `{"id":9007199254740993,"ok":true,"ts":1735689600000}`
	doc, err := xhrJSONEnvelope(rendered, []fetch.XHRCapture{{URL: "https://x/api", Status: 200}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc, "9007199254740993") {
		t.Errorf("big int corrupted by float64 round-trip:\n%s", doc)
	}
	if !strings.Contains(doc, `"xhr"`) {
		t.Error("xhr key missing from envelope")
	}
}

// TestScrape_BadCDPExit2 — --cdp-url with a bad scheme is rejected
// pre-I/O (exit 2): the target is a nonexistent file, so any fetch would
// exit differently — exit 2 proves validation ran first.
func TestScrape_BadCDPExit2(t *testing.T) {
	testEnv(t, "cache.db")
	err := runScrape(t.Context(), "file:///nonexistent-cdp-probe.html", scrapeOptions{Format: "json", CDP: "ftp://b:9222"})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if strings.Contains(err.Error(), "user:pass") {
		t.Errorf("error leaks credentials: %v", err)
	}
}

// TestScrape_BadCaptureXHRExit2 — an invalid capture regexp is a usage
// error before any I/O.
func TestScrape_BadCaptureXHRExit2(t *testing.T) {
	testEnv(t, "cache.db")
	err := runScrape(t.Context(), "file:///nonexistent-xhr-probe.html", scrapeOptions{Format: "json", CaptureXHR: []string{"[bad"}})
	if codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
}

// TestXHREnvelope_Injection — page-format json + captures: same envelope
// keys plus xhr; the rendered JSON shape is untouched otherwise.
func TestXHREnvelope_Injection(t *testing.T) {
	rendered := `{
  "url": "https://x",
  "markdown": "md"
}`
	canned := []fetch.XHRCapture{{URL: "https://x/api", Status: 200, Body: "1"}}
	doc, err := xhrJSONEnvelope(rendered, canned)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(doc), &env); err != nil {
		t.Fatalf("not JSON: %v", doc)
	}
	if env["url"] != "https://x" || env["markdown"] != "md" {
		t.Errorf("original keys lost: %v", env)
	}
	xhrs, ok := env["xhr"].([]any)
	if !ok || len(xhrs) != 1 {
		t.Fatalf("xhr = %#v, want one capture", env["xhr"])
	}
	if first := xhrs[0].(map[string]any); first["url"] != "https://x/api" || first["status"] != float64(200) {
		t.Errorf("capture = %v, want the canned capture", first)
	}
}
