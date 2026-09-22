package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/scrape"
)

func TestBatch_MixedExit0(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	stdout, _ := captureOutput(t, func() {
		err := runBatch(t.Context(), []string{"file://" + abs, "file:///nonexistent-batch-probe.html"}, batchOptions{Format: "jsonl", Concurrency: 2})
		if codeOf(err) != 0 {
			t.Errorf("exit = %d, want 0 (err=%v)", codeOf(err), err)
		}
	})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("jsonl lines = %d, want 2:\n%s", len(lines), stdout)
	}
	var r0, r1 map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &r0); err != nil {
		t.Fatalf("line 0 not JSON: %v", err)
	}
	if r0["ok"] != true || r0["markdown"] == nil {
		t.Errorf("record 0 = %v, want ok:true + markdown", r0)
	}
	if err := json.Unmarshal([]byte(lines[1]), &r1); err != nil {
		t.Fatalf("line 1 not JSON: %v", err)
	}
	if r1["ok"] != false || r1["error"] == nil {
		t.Errorf("record 1 = %v, want ok:false + error", r1)
	}
}

func TestBatch_FileAndLimits(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	list := filepath.Join(t.TempDir(), "urls.txt")
	if err := os.WriteFile(list, []byte("file://"+abs+"\n\nfile://"+abs+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ := captureOutput(t, func() {
		if err := runBatch(t.Context(), nil, batchOptions{File: list, Format: "json", Concurrency: 8}); err != nil {
			t.Fatalf("batch --file: %v", err)
		}
	})
	var doc struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json out not parseable: %v\n%s", err, stdout)
	}
	if len(doc.Results) != 2 {
		t.Errorf("results = %d, want 2 (blank line skipped)", len(doc.Results))
	}

	var urls []string
	for i := 0; i < 101; i++ {
		urls = append(urls, "file:///x.html")
	}
	if err := runBatch(t.Context(), urls, batchOptions{Format: "jsonl", Concurrency: 2}); codeOf(err) != 2 {
		t.Errorf("101 URLs exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if err := runBatch(t.Context(), []string{"file:///x.html"}, batchOptions{Format: "jsonl", Concurrency: 0}); codeOf(err) != 2 {
		t.Errorf("concurrency 0 exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
}

func TestMap_LinesAndJSON(t *testing.T) {
	testEnv(t, "cache.db")
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Sitemap: http://" + r.Host + "/s.xml\n")) //nolint:errcheck // httptest local
	})
	mux.HandleFunc("/s.xml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<urlset><url><loc>http://` + r.Host + `/a</loc></url><url><loc>http://` + r.Host + `/b</loc></url></urlset>`)) //nolint:errcheck // httptest local
	})
	site := newTestServer(t, mux)
	stdout, _ := captureOutput(t, func() {
		if err := runMap(t.Context(), site, mapOptions{}); err != nil {
			t.Fatalf("map: %v", err)
		}
	})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || lines[0] != site+"/a" || lines[1] != site+"/b" {
		t.Errorf("lines = %v, want exact loc set", lines)
	}
	stdout, _ = captureOutput(t, func() {
		if err := runMap(t.Context(), site, mapOptions{Format: "json"}); err != nil {
			t.Fatalf("map json: %v", err)
		}
	})
	var doc struct {
		URLs      []string `json:"urls"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("json out not parseable: %v", err)
	}
	if len(doc.URLs) != 2 || doc.Truncated {
		t.Errorf("json = %+v, want 2 urls untruncated", doc)
	}
}

func TestSummarize_MaxSentences(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	srv, fp := newFakeProvider(t, openAIEnvelope("First sentence here. Second sentence here. Third and fourth follow along."))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")
	abs := mustAbs(t, "../testdata/clean/article.html")
	out := filepath.Join(t.TempDir(), "out.json")
	err := runSummarize(t.Context(), "file://"+abs, summarizeOptions{
		MaxSentences: 2, Provider: "openai", Model: "gpt-4o-mini", Out: out,
	})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if fp.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", fp.callCount())
	}
	raw := mustRead(t, out)
	var doc struct {
		Summary string `json:"summary"`
		Usage   struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("out not JSON: %v", err)
	}
	if doc.Usage.PromptTokens != 100 {
		t.Errorf("usage.prompt_tokens = %d, want 100 (agents see the spend)", doc.Usage.PromptTokens)
	}
	if n := strings.Count(doc.Summary, "."); n > 2 {
		t.Errorf("summary has %d sentences, want ≤2:\n%s", n, doc.Summary)
	}
	db := mustOpenDB(t, dbPath)
	calls, err := db.LLMCalls("")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Purpose != "summarize" {
		t.Errorf("llm_calls = %+v, want one summarize row", calls)
	}
}

func TestDiff_AgainstFile(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	// Changed snapshot → non-empty diff, exit 0.
	snap := filepath.Join(t.TempDir(), "snap.txt")
	if err := os.WriteFile(snap, []byte("totally different words here"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ := captureOutput(t, func() {
		if err := runDiff(t.Context(), "file://"+abs, diffOptions{Against: snap}); err != nil {
			t.Fatalf("diff: %v", err)
		}
	})
	if !strings.Contains(stdout, "+") || !strings.Contains(stdout, "-") {
		t.Errorf("diff lacks -old/+new pairs:\n%s", stdout)
	}
	// Identical snapshot → empty diff, exit 0. The snapshot is the page's
	// own markdown, harvested from a scrape run.
	scrapeOut := filepath.Join(t.TempDir(), "scrape.json")
	if err := runScrape(t.Context(), "file://"+abs, scrapeOptions{Format: "json", Out: scrapeOut, Render: "static"}); err != nil {
		t.Fatalf("scrape: %v", err)
	}
	var sdoc struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(mustRead(t, scrapeOut), &sdoc); err != nil {
		t.Fatal(err)
	}
	snap2 := filepath.Join(t.TempDir(), "snap2.txt")
	if err := os.WriteFile(snap2, []byte(sdoc.Markdown), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _ = captureOutput(t, func() {
		if err := runDiff(t.Context(), "file://"+abs, diffOptions{Against: snap2}); err != nil {
			t.Fatalf("diff identical: %v", err)
		}
	})
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("identical diff printed %q, want empty", stdout)
	}
	if err := runDiff(t.Context(), "file://"+abs, diffOptions{}); codeOf(err) != 2 {
		t.Errorf("missing --against exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
}

func TestBrand_JSON(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/brand/shop.html")
	out := filepath.Join(t.TempDir(), "out.json")
	if err := runBrand(t.Context(), "file://"+abs, brandOptions{Out: out}); err != nil {
		t.Fatalf("brand: %v", err)
	}
	var doc struct {
		Colors []string `json:"colors"`
		Fonts  []string `json:"fonts"`
		Logo   string   `json:"logo"`
	}
	if err := json.Unmarshal(mustRead(t, out), &doc); err != nil {
		t.Fatalf("out not JSON: %v", err)
	}
	if len(doc.Colors) != 3 || doc.Logo != "https://shop.example.com/assets/logo.png" {
		t.Errorf("brand = %+v, want 3 colors + img logo", doc)
	}
	db := mustOpenDB(t, dbPath)
	if n := mustCount(t, db, ""); n != 0 {
		t.Errorf("llm_calls = %d, want 0 (zero-LLM brand)", n)
	}
}

func TestVertical_List(t *testing.T) {
	testEnv(t, "cache.db")
	stdout, _ := captureOutput(t, func() {
		cmd := newVerticalCmd()
		cmd.SetArgs([]string{"--list"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("vertical --list: %v", err)
		}
	})
	if !strings.Contains(stdout, "github_repo") {
		t.Errorf("vertical --list lacks github_repo:\n%s", stdout)
	}
	var doc struct {
		Extractors []map[string]any `json:"extractors"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("--list not JSON: %v", err)
	}
	if len(doc.Extractors) != 21 {
		t.Errorf("extractors = %d, want 21 (10 pre-H + 5 Phase H + upwork_job + 5 Phase V)", len(doc.Extractors))
	}
}

func TestVertical_Mismatch(t *testing.T) {
	testEnv(t, "cache.db")
	abs := mustAbs(t, "../testdata/clean/article.html")
	cmd := newVerticalCmd()
	cmd.SetArgs([]string{"--name", "reddit", "file://" + abs})
	if err := cmd.Execute(); codeOf(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
}

func TestExtract_PromptXor(t *testing.T) {
	testEnv(t, "cache.db")
	schema := mustAbs(t, "../testdata/extract/price.yaml")
	abs := mustAbs(t, "../testdata/clean/article.html")
	if err := runExtract(t.Context(), extractOptions{
		Schema: schema, Prompt: "summarize", ContentType: "html",
		Provider: "openai", Model: "gpt-4o-mini", File: abs,
	}); codeOf(err) != 2 {
		t.Errorf("prompt+schema exit = %d, want 2 (err=%v)", codeOf(err), err)
	}
	if err := runExtract(t.Context(), extractOptions{
		ContentType: "html", Provider: "openai", Model: "gpt-4o-mini", File: abs,
	}); codeOf(err) != 2 || !strings.Contains(err.Error(), "--schema is required") {
		t.Errorf("neither exit = %d (%v), want today's required error", codeOf(err), err)
	}
}

func TestExtract_PromptText(t *testing.T) {
	dbPath := testEnv(t, "cache.db")
	srv, fp := newFakeProvider(t, openAIEnvelope("plain summary"))
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")
	abs := mustAbs(t, "../testdata/clean/article.html")
	out := filepath.Join(t.TempDir(), "out.txt")
	err := runExtract(t.Context(), extractOptions{
		Prompt: "summarize", ContentType: "html", Provider: "openai",
		Model: "gpt-4o-mini", Out: out, File: abs,
	})
	if err != nil {
		t.Fatalf("extract --prompt: %v", err)
	}
	if fp.callCount() != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", fp.callCount())
	}
	if body := strings.TrimSpace(string(mustRead(t, out))); body != "plain summary" {
		t.Errorf("stdout = %q, want unvalidated script text", body)
	}
	db := mustOpenDB(t, dbPath)
	calls, err := db.LLMCalls("")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Purpose != "extract" {
		t.Errorf("llm_calls = %+v, want one extract row", calls)
	}
}

func TestProvider_Unknown(t *testing.T) {
	testEnv(t, "cache.db")
	t.Setenv("MAGPIE_BOGUS_API_KEY", "x") // reach the switch past the key check
	abs := mustAbs(t, "../testdata/clean/article.html")
	// A typo must never silently bill another provider.
	err := runSummarize(t.Context(), "file://"+abs, summarizeOptions{Provider: "bogus"})
	if codeOf(err) != 2 || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("exit = %d (%v), want 2 naming the provider", codeOf(err), err)
	}
}

func TestAutoProviders_Order(t *testing.T) {
	testEnv(t, "cache.db")
	for _, k := range []string{"MAGPIE_ANTHROPIC_API_KEY", "MAGPIE_OPENAI_API_KEY", "MAGPIE_OPENROUTER_API_KEY", "MAGPIE_OPENCODE_ZEN_API_KEY", "MAGPIE_OPENCODE_GO_API_KEY", "MAGPIE_API_KEY"} {
		t.Setenv(k, "")
	}
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatal(err)
	}
	got := scrape.AutoCandidates(cfg.APIKey)
	// Keyless local/CLI providers always qualify, last in documented order.
	if len(got) != 2 || got[0] != "ollama" || got[1] != "codex" {
		t.Fatalf("keyless auto = %v, want [ollama codex]", got)
	}
	t.Setenv("MAGPIE_OPENAI_API_KEY", "k")
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "k2")
	cfg, err = resolveConfig()
	if err != nil {
		t.Fatal(err)
	}
	got = scrape.AutoCandidates(cfg.APIKey)
	if len(got) != 4 || got[0] != "anthropic" || got[1] != "openai" {
		t.Fatalf("keyed auto = %v, want keyed-first in priority order", got)
	}
}

func TestAgent_CommandsRegistered(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want string
	}{
		{"batch", "batch [urls...]"},
		{"map", "map <site>"},
		{"summarize", "summarize <url>"},
		{"diff", "diff <url>"},
		{"brand", "brand <url>"},
		{"vertical", "vertical [--list]"},
	} {
		var usage string
		switch tc.cmd {
		case "batch":
			usage = newBatchCmd().UsageString()
		case "map":
			usage = newMapCmd().UsageString()
		case "summarize":
			usage = newSummarizeCmd().UsageString()
		case "diff":
			usage = newDiffCmd().UsageString()
		case "brand":
			usage = newBrandCmd().UsageString()
		case "vertical":
			usage = newVerticalCmd().UsageString()
		}
		if !strings.Contains(usage, tc.want) {
			t.Errorf("%s usage lacks %q:\n%s", tc.cmd, tc.want, usage)
		}
	}
	root := rootCmd()
	found := map[string]bool{}
	for _, c := range root.Commands() {
		found[c.Name()] = true
	}
	for _, name := range []string{"batch", "map", "summarize", "diff", "brand", "vertical"} {
		if !found[name] {
			t.Errorf("root missing %q command", name)
		}
	}
}

// TestMap_BadSiteExit2: a non-http(s) site argument is a usage error —
// it must take the documented exit-2 class with a message naming the
// requirement, not a runtime exit-1 from deep inside the sitemap client.
func TestMap_BadSiteExit2(t *testing.T) {
	testEnv(t, "cache.db")
	for _, site := range []string{"file:///tmp/x.html", "exampl.com", "htp://x"} {
		err := runMap(t.Context(), site, mapOptions{})
		if codeOf(err) != 2 {
			t.Errorf("map %q exit = %d, want 2 (err=%v)", site, codeOf(err), err)
		}
	}
}
