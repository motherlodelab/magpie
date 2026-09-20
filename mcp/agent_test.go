package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	magpiemcp "github.com/motherlodelab/magpie/mcp"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/selector"
	"github.com/motherlodelab/magpie/store"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAgentFetcher is the mcp-side copy of the substring→bytes fake
// (third copy: vertical, scrape, mcp — test-only, diverges by key domain,
// so it stays a copy per the rule-of-three rationale). Longest match wins;
// a trailing "$" anchors to the URL end.
type fakeAgentFetcher struct {
	mu     sync.Mutex
	bodies map[string]fakeAgentResp
	order  []string
}

type fakeAgentResp struct {
	status int
	body   []byte
	err    error
	xhr    []fetch.XHRCapture // canned captures (Phase J round-trips)
}

func (f *fakeAgentFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, req.URL)
	best, bestLen := "", -1
	for sub := range f.bodies {
		key, anchored := sub, false
		if strings.HasSuffix(key, "$") {
			key, anchored = strings.TrimSuffix(key, "$"), true
		}
		matched := strings.Contains(req.URL, key)
		if anchored {
			matched = strings.HasSuffix(req.URL, key)
		}
		if matched && len(key) > bestLen {
			best, bestLen = sub, len(key)
		}
	}
	if bestLen < 0 {
		return nil, errors.New("fake fetcher: unexpected URL " + req.URL)
	}
	r := f.bodies[best]
	if r.err != nil {
		return nil, r.err
	}
	st := r.status
	if st == 0 {
		st = 200
	}
	return &fetch.FetchResponse{URL: req.URL, FinalURL: req.URL, StatusCode: st, HTML: r.body, XHR: r.xhr}, nil
}

var _ extract.Prompter = (*fakePrompterExtractor)(nil)

// fakePrompterExtractor scripts PromptText per call (last repeats).
// Extract loudly fails: firing on a prompt path breaks the bypass proof.
type fakePrompterExtractor struct {
	t            *testing.T
	promptScript []string
	promptErr    error
	mu           sync.Mutex
	calls        int
}

func (f *fakePrompterExtractor) Name() string { return "fake-prompt" }

func (f *fakePrompterExtractor) Extract(_ context.Context, _ extract.ExtractInput) (extract.ExtractResult, error) {
	f.t.Error("Extract called on prompt path — validator bypass violated")
	return extract.ExtractResult{}, errors.New("must not be called")
}

func (f *fakePrompterExtractor) PromptText(_ context.Context, _, _ string) (string, extract.TokenUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.promptErr != nil {
		return "", extract.TokenUsage{}, f.promptErr
	}
	f.calls++
	idx := min(f.calls-1, len(f.promptScript)-1)
	return f.promptScript[idx], extract.TokenUsage{PromptTokens: 10, CompletionTokens: 5}, nil
}

// agentDeps rebuilds magpiemcp.Deps in testMCPServer's shape plus Fetcher
// (rebuild, don't edit the helper) and a Prompter-returning ExtractorFor.
func agentDeps(db *store.DB, fx *fakeExtractor, px *fakePrompterExtractor, fetcher *fakeAgentFetcher) magpiemcp.Deps {
	return magpiemcp.Deps{
		DB: db,
		ScrapeDeps: scrape.Deps{
			DB:      db,
			Fetcher: fetcher,
			ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
				if px != nil {
					return px, nil
				}
				return fx, nil
			},
			APIKeyFor: func(string) string { return "test-key" },
		},
		DefaultProvider: "fake",
		DefaultModel:    "fake",
	}
}

func llmCalls(t *testing.T, db *store.DB) int {
	t.Helper()
	n, err := db.TableCount("llm_calls")
	if err != nil {
		t.Fatalf("TableCount: %v", err)
	}
	return n
}

func agentHTML(title string) []byte {
	return []byte(`<html><head><title>` + title + `</title></head><body><h1>` + title + `</h1><p>` + strings.Repeat("honest prose ", 40) + `</p></body></html>`)
}

func toolErrText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatal("want tool error, got success")
	}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestToolCatalog(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	cs := dialInMemory(t, testMCPServer(t, db, fx), nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var got []string
	for _, tl := range tools.Tools {
		got = append(got, tl.Name)
	}
	want := []string{"scrape_url", "crawl_site", "extract_structured", "get_cached_selectors",
		"batch", "map", "summarize", "diff", "brand", "list_extractors", "vertical_scrape", "search"}
	if len(got) != len(want) {
		t.Fatalf("tool count = %d (%v), want %d", len(got), got, len(want))
	}
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("missing tool %q in %v", n, got)
		}
	}
}

func TestWiden_PreservesRequired(t *testing.T) {
	// The validator lesson, locked end-to-end: infer BatchIn fresh
	// (pre-widen), then read the widened schema back off a live server
	// via ListTools. required[] + every property description must be
	// byte-identical, while scalar types must admit strings.
	sch, err := jsonschema.ForType(reflect.TypeFor[magpiemcp.BatchIn](), &jsonschema.ForOptions{})
	if err != nil {
		t.Fatalf("ForType: %v", err)
	}
	preReq := append([]string(nil), sch.Required...)
	preDesc := map[string]string{}
	for name, p := range sch.Properties {
		preDesc[name] = p.Description
	}
	if len(preDesc) == 0 {
		t.Fatal("no properties inferred — nothing is locking widening")
	}

	db := openMCPDB(t)
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil, &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}})), nil)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var raw any
	for _, tl := range tools.Tools {
		if tl.Name == "batch" {
			raw = tl.InputSchema
		}
	}
	if raw == nil {
		t.Fatal("batch tool missing from catalog")
	}
	live, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Description string `json:"description"`
			Type        any    `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(live, &doc); err != nil {
		t.Fatalf("live schema not JSON: %v", err)
	}
	sorted := func(s []string) []string { out := append([]string(nil), s...); sort.Strings(out); return out }
	if !reflect.DeepEqual(sorted(preReq), sorted(doc.Required)) {
		t.Errorf("required changed pre/post widen: %v vs %v", preReq, doc.Required)
	}
	for name, want := range preDesc {
		got, ok := doc.Properties[name]
		if !ok {
			t.Errorf("property %q lost in widening", name)
			continue
		}
		if got.Description != want {
			t.Errorf("description[%q] changed: %q vs %q", name, want, got.Description)
		}
	}
	// Non-vacuous: widening must have happened — concurrency admits strings.
	conc := doc.Properties["concurrency"]
	if !hasStringType(conc.Type) {
		t.Errorf("concurrency schema type = %v, want string union (widening missed)", conc.Type)
	}
	urls := doc.Properties["urls"]
	if !hasStringType(urls.Type) {
		t.Errorf("urls schema type = %v, want string union", urls.Type)
	}
}

func hasStringType(v any) bool {
	switch t := v.(type) {
	case string:
		return t == "string"
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && s == "string" {
				return true
			}
		}
	}
	return false
}

func TestFlex_CoercionUnits(t *testing.T) {
	// Pure flex round-trips: valid shapes coerce, bad shapes are loud.
	t.Run("int", func(t *testing.T) {
		for _, tc := range []struct {
			raw  string
			want int
			err  string
		}{
			{`3`, 3, ""}, {`"3"`, 3, ""}, {`" 3 "`, 3, ""},
			{`"abc"`, 0, "not an integer"}, {`3.5`, 0, "want integer"}, {`true`, 0, "want integer"},
		} {
			var v magpiemcp.FlexInt
			err := json.Unmarshal([]byte(tc.raw), &v)
			if tc.err == "" {
				if err != nil || int(v) != tc.want {
					t.Errorf("%s → (%d, %v), want (%d, nil)", tc.raw, v, err, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s → %v, want error containing %q", tc.raw, err, tc.err)
			}
		}
	})
	t.Run("bool", func(t *testing.T) {
		for _, tc := range []struct {
			raw  string
			want bool
			err  string
		}{
			{`true`, true, ""}, {`"true"`, true, ""}, {`"False"`, false, ""},
			{`"yes"`, false, "not a boolean"}, {`3`, false, "want boolean"},
		} {
			var v magpiemcp.FlexBool
			err := json.Unmarshal([]byte(tc.raw), &v)
			if tc.err == "" {
				if err != nil || bool(v) != tc.want {
					t.Errorf("%s → (%v, %v), want (%v, nil)", tc.raw, v, err, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s → %v, want error containing %q", tc.raw, err, tc.err)
			}
		}
	})
	t.Run("stringlist", func(t *testing.T) {
		for _, tc := range []struct {
			raw  string
			want []string
			err  string
		}{
			{`["a","b"]`, []string{"a", "b"}, ""},
			{`"[\"a\",\"b\"]"`, []string{"a", "b"}, ""},
			{`"https://x/?a=b,c"`, []string{"https://x/?a=b,c"}, ""}, // never comma-split
			{`"  https://x/  "`, []string{"https://x/"}, ""},
			{`3`, nil, "want array"},
		} {
			var v magpiemcp.StringList
			err := json.Unmarshal([]byte(tc.raw), &v)
			if tc.err == "" {
				if err != nil || !reflect.DeepEqual([]string(v), tc.want) {
					t.Errorf("%s → (%v, %v), want (%v, nil)", tc.raw, v, err, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s → %v, want error containing %q", tc.raw, err, tc.err)
			}
		}
	})
}

func TestBatch_MCP(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{} // must never be constructed — batch is markdown-only
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"example.com/good1": {body: agentHTML("One")},
		"example.com/good2": {body: agentHTML("Two")},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, nil, fetch)), nil)
	out := decodeOut(t, callTool(t, cs, "batch", map[string]any{
		"urls":        []any{"https://example.com/good1", "https://example.com/bad", "https://example.com/good2"},
		"concurrency": "2", // STRING — the validator's repro, must coerce
	}, ""))
	results, ok := out["results"].([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("results = %#v, want 3 records", out["results"])
	}
	r0 := results[0].(map[string]any)
	if r0["ok"] != true || r0["url"] != "https://example.com/good1" {
		t.Errorf("results[0] = %#v, want ok:true in input order", r0)
	}
	if r0["markdown"] == nil || r0["markdown"] == "" {
		t.Errorf("results[0] has no markdown: %#v", r0)
	}
	r1 := results[1].(map[string]any)
	if r1["ok"] != false || r1["error"] == nil || r1["error"] == "" {
		t.Errorf("results[1] = %#v, want ok:false + error string", r1)
	}
	if fx.total() != 0 {
		t.Errorf("extractor calls = %d, want 0 (markdown-only batch)", fx.total())
	}
	if n := llmCalls(t, db); n != 0 {
		t.Errorf("llm_calls rows = %d, want 0", n)
	}
}

func TestFlex_BadString(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, nil, fetch)), nil)
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "batch", Arguments: map[string]any{
			"urls":        []any{"https://example.com/x"},
			"concurrency": "abc",
		},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	text := toolErrText(t, res)
	if !strings.Contains(text, "concurrency") {
		t.Errorf("error %s does not name the param (loud, not zero-value)", text)
	}
}

func TestMap_MCP(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"example.com/robots.txt": {body: []byte("Sitemap: https://example.com/s.xml\n")},
		"example.com/s.xml":      {body: []byte(`<urlset><url><loc>https://example.com/a</loc></url><url><loc>https://example.com/b</loc></url></urlset>`)},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, nil, fetch)), nil)
	out := decodeOut(t, callTool(t, cs, "map", map[string]any{"site": "https://example.com"}, ""))
	urls, ok := out["urls"].([]any)
	if !ok || len(urls) != 2 {
		t.Fatalf("urls = %#v, want 2 locs", out["urls"])
	}
	if out["truncated"] == true {
		t.Errorf("truncated = true, want false")
	}
	if n := llmCalls(t, db); n != 0 {
		t.Errorf("llm_calls rows = %d, want 0", n)
	}
}

func TestSummarize_MCP(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	px := &fakePrompterExtractor{t: t, promptScript: []string{"First sentence here. Second sentence here. Third sentence here. Fourth."}}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"example.com/article": {body: agentHTML("Article")},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, px, fetch)), nil)
	out := decodeOut(t, callTool(t, cs, "summarize", map[string]any{
		"url": "https://example.com/article", "max_sentences": "2", // STRING coerces
	}, ""))
	sum, _ := out["summary"].(string)
	if n := strings.Count(sum, "."); n > 2 {
		t.Errorf("summary has %d sentences, want ≤2:\n%s", n, sum)
	}
	usage, ok := out["usage"].(map[string]any)
	if !ok || usage["prompt_tokens"] != float64(10) {
		t.Errorf("usage = %v, want prompt tokens (agents see the spend)", out["usage"])
	}
	if fx.total() != 0 {
		t.Errorf("extractor calls = %d, want 0 (prompt path only)", fx.total())
	}
}

func TestDiff_MCP(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"example.com/page": {body: agentHTML("Page")},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, nil, fetch)), nil)
	// Identical snapshot → empty diff.
	md := decodeOut(t, callTool(t, cs, "batch", map[string]any{
		"urls": []any{"https://example.com/page"},
	}, ""))["results"].([]any)[0].(map[string]any)["markdown"].(string)
	out := decodeOut(t, callTool(t, cs, "diff", map[string]any{
		"url": "https://example.com/page", "previous_snapshot": md,
	}, ""))
	if out["diff"] != "" || out["changed"] != false {
		t.Errorf("identical diff = %#v, want empty/unchanged", out)
	}
	out2 := decodeOut(t, callTool(t, cs, "diff", map[string]any{
		"url": "https://example.com/page", "previous_snapshot": "totally different words here",
	}, ""))
	if out2["changed"] != true || out2["diff"] == "" {
		t.Errorf("changed diff = %#v, want non-empty/changed", out2)
	}
	if n := llmCalls(t, db); n != 0 {
		t.Errorf("llm_calls rows = %d, want 0", n)
	}
}

func TestBrand_MCP(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "brand", "shop.html"))
	if err != nil {
		t.Fatal(err)
	}
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"example.com/shop": {body: raw},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, nil, fetch)), nil)
	out := decodeOut(t, callTool(t, cs, "brand", map[string]any{"url": "https://example.com/shop"}, ""))
	if out["logo"] != "https://shop.example.com/assets/logo.png" {
		t.Errorf("logo = %v, want img-logo", out["logo"])
	}
	colors, ok := out["colors"].([]any)
	if !ok || len(colors) != 3 {
		t.Errorf("colors = %v, want 3", out["colors"])
	}
	if fx.total() != 0 {
		t.Errorf("extractor calls = %d, want 0", fx.total())
	}
	if n := llmCalls(t, db); n != 0 {
		t.Errorf("llm_calls rows = %d, want 0", n)
	}
}

func TestVertical_MCP(t *testing.T) {
	ghRaw, err := os.ReadFile(filepath.Join("..", "testdata", "vertical", "github-repo.json"))
	if err != nil {
		t.Fatal(err)
	}
	db := openMCPDB(t)
	fx := &fakeExtractor{}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"github.com/o/r$":   {body: agentHTML("o/r")},
		"api.github.com/":   {body: ghRaw},
		"example.com/plain": {body: agentHTML("Plain")},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, fx, nil, fetch)), nil)

	list := decodeOut(t, callTool(t, cs, "list_extractors", map[string]any{}, ""))
	exs, ok := list["extractors"].([]any)
	if !ok || len(exs) != 16 {
		t.Fatalf("extractors = %#v, want exactly 16 (10 pre-H + 5 Phase H + upwork_job)", list["extractors"])
	}
	names := map[string]bool{}
	for _, e := range exs {
		names[e.(map[string]any)["name"].(string)] = true
	}
	if !names["github_repo"] {
		t.Errorf("extractors lack github_repo: %v", list["extractors"])
	}

	hit := decodeOut(t, callTool(t, cs, "vertical_scrape", map[string]any{
		"url": "https://github.com/o/r", "vertical": "github_repo",
	}, ""))
	if hit["vertical"] != "github_repo" || hit["record"] == nil {
		t.Errorf("vertical_scrape = %#v, want record", hit)
	}
	if fx.total() != 0 {
		t.Errorf("extractor calls = %d, want 0 (zero-LLM vertical)", fx.total())
	}
	if n := llmCalls(t, db); n != 0 {
		t.Errorf("llm_calls rows = %d, want 0", n)
	}

	// Mismatch is a loud typed error, not an empty record.
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "vertical_scrape", Arguments: map[string]any{
			"url": "https://example.com/plain", "vertical": "github_repo",
		},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if text := toolErrText(t, res); !strings.Contains(text, "does not handle") {
		t.Errorf("mismatch error = %s, want ErrURLMismatch wording", text)
	}
}

func TestExtractStructured_PromptAndSchemaString(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	px := &fakePrompterExtractor{t: t, promptScript: []string{"plain answer"}}
	fetch := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}}
	deps := agentDeps(db, fx, nil, fetch)
	deps.ScrapeDeps.ExtractorFor = func(_, _, _ string, sch *extract.Schema, _ string) (extract.Extractor, error) {
		if sch == nil {
			return px, nil // prompt path
		}
		return fx, nil // schema path
	}
	cs := dialInMemory(t, magpiemcp.NewServer(deps), nil)

	// JSON-string schema coerces to a map (FlexMap proof).
	out := decodeOut(t, callTool(t, cs, "extract_structured", map[string]any{
		"content": "# Widget\n\nA fine widget.", "content_type": "markdown",
		"schema": `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`,
	}, ""))
	ex, ok := out["extracted"].(map[string]any)
	if !ok || ex["title"] != "Widget" {
		t.Errorf("extracted = %v, want title Widget", out["extracted"])
	}

	// Prompt path returns text with no validator.
	out2 := decodeOut(t, callTool(t, cs, "extract_structured", map[string]any{
		"content": "# Widget\n\nA fine widget.", "content_type": "markdown",
		"prompt": "what is this",
	}, ""))
	if out2["content"] != "plain answer" {
		t.Errorf("content = %v, want prompt text verbatim", out2["content"])
	}

	// prompt + schema together is a loud xor error.
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "extract_structured", Arguments: map[string]any{
			"content": "x", "prompt": "y",
			"schema": map[string]any{"type": "object"},
		},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if text := toolErrText(t, res); !strings.Contains(text, "mutually exclusive") {
		t.Errorf("xor error = %s, want mutual-exclusion", text)
	}
}

func flexTrue() *magpiemcp.FlexBool {
	b := magpiemcp.FlexBool(true)
	return &b
}

// TestIn_RoundTrip locks every flex-bearing In to its shadow decoder: a
// struct field missing from the shadow would silently drop input (schema
// inference reads the struct, decoding reads the shadow), so each struct
// round-trips fully populated — including string-coerced scalars.
func TestIn_RoundTrip(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		want := magpiemcp.BatchIn{
			URLs: []string{"https://example.com/a"}, Concurrency: 8, Render: "static",
			Profile: "chrome", Cookies: "a=b", Include: []string{"article"}, Exclude: []string{"nav"},
			OnlyMainContent: flexTrue(),
		}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got magpiemcp.BatchIn
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip = %+v, want %+v (shadow dropped a field?)", got, want)
		}
		// String-coerced scalars land too.
		var coerced magpiemcp.BatchIn
		if err := json.Unmarshal([]byte(`{"urls":"https://example.com/a","concurrency":"2","only_main_content":"true"}`), &coerced); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if int(coerced.Concurrency) != 2 || coerced.OnlyMainContent == nil || !bool(*coerced.OnlyMainContent) {
			t.Errorf("coerced = %+v, want concurrency 2 + only_main true", coerced)
		}
		if len(coerced.URLs) != 1 || coerced.URLs[0] != "https://example.com/a" {
			t.Errorf("coerced urls = %v, want single URL intact", coerced.URLs)
		}
	})
	t.Run("scrape", func(t *testing.T) {
		want := magpiemcp.ScrapeIn{
			URL: "https://example.com/a", Schema: magpiemcp.FlexMap{"type": "object"},
			Render: "static", UseCache: flexTrue(), PageFormat: "llm",
			Include: []string{"article"}, Exclude: []string{"nav"}, OnlyMainContent: flexTrue(),
			Profile: "chrome", Cookies: "a=b",
			Actions: []string{"click #consent", "wait-for .row:nth-child(30)"}, Lang: "fr-CA,fr;q=0.9",
		}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got magpiemcp.ScrapeIn
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip = %+v, want %+v (shadow dropped a field?)", got, want)
		}
		// String-coerced actions list lands too (flex shadow, Phase H).
		var coerced magpiemcp.ScrapeIn
		if err := json.Unmarshal([]byte(`{"actions":"click #x","lang":"fr"}`), &coerced); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(coerced.Actions) != 1 || coerced.Actions[0] != "click #x" || coerced.Lang != "fr" {
			t.Errorf("coerced = %+v, want single action + lang", coerced)
		}
	})
	t.Run("crawl", func(t *testing.T) {
		want := magpiemcp.CrawlIn{
			URL: "https://example.com", MaxPages: 5, MaxDepth: 2,
			SameHost: flexTrue(), Schema: magpiemcp.FlexMap{"type": "object"}, RunID: "r1",
		}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got magpiemcp.CrawlIn
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip = %+v, want %+v (shadow dropped a field?)", got, want)
		}
	})
	t.Run("extract", func(t *testing.T) {
		want := magpiemcp.ExtractIn{
			Content: "# hi", ContentType: "markdown",
			Schema: magpiemcp.FlexMap{"type": "object"}, Prompt: "",
		}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got magpiemcp.ExtractIn
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip = %+v, want %+v (shadow dropped a field?)", got, want)
		}
	})
	t.Run("summarize", func(t *testing.T) {
		want := magpiemcp.SummarizeIn{
			URL: "https://example.com/a", MaxSentences: 3, Provider: "openai", Model: "m",
		}
		raw, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got magpiemcp.SummarizeIn
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip = %+v, want %+v (shadow dropped a field?)", got, want)
		}
	})
}

// TestStringArgs_E2E drives string scalars through the real transport
// (validation-before-unmarshal): the flex-unit table proves decode shapes,
// this proves the widened schema admits them and handlers honor them.

// TestStringArgs_E2E drives string scalars through the real transport
// (validation-before-unmarshal): the flex-unit table proves decode shapes,
// this proves the widened schema admits them and handlers honor them.
func TestStringArgs_E2E(t *testing.T) {
	const scoped = `<html><head><title>Shell</title></head><body><nav>nav-marker-text</nav><article><h1>Scoped Headline</h1><p>Enough honest prose in the scoped article branch to survive trafilatura extraction cleanly.</p></article></body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(scoped)) //nolint:errcheck // httptest local
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	db := openMCPDB(t)
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	deps := agentDeps(db, fx, nil, &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}})
	deps.ScrapeDeps.Fetcher = nil // live localhost: nil falls back to the static fetcher
	cs := dialInMemory(t, magpiemcp.NewServer(deps), nil)

	// Single-string include + string use_cache, honored end to end.
	out := decodeOut(t, callTool(t, cs, "scrape_url", map[string]any{
		"url": srv.URL, "render": "static", "include": "article", "use_cache": "true",
	}, ""))
	md, _ := out["markdown"].(string)
	if !strings.Contains(md, "Scoped Headline") || strings.Contains(md, "nav-marker-text") {
		t.Errorf("string include/use_cache not honored:\n%s", md)
	}

	// String "true" honored as true: seeded selector cache + schema hits
	// with zero extractor calls (a false would run the LLM path).
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	domain := strings.ToLower(u.Host)
	sch, err := extract.ParseSchema([]byte(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := json.Marshal(selector.SelectorDoc{
		SchemaHash: selector.SchemaHash(sch), Domain: domain,
		Fields: map[string]selector.FieldSelector{"title": {Type: "css", Expr: "title"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSelectors(domain, selector.SchemaHash(sch), string(doc), 3); err != nil {
		t.Fatal(err)
	}
	before := fx.total()
	out2 := decodeOut(t, callTool(t, cs, "scrape_url", map[string]any{
		"url": srv.URL, "render": "static", "schema": testSchemaMap, "use_cache": "true",
	}, ""))
	if _, ok := out2["extracted"].(map[string]any); !ok {
		t.Fatalf("cache-hit out has no extracted: %v", out2)
	}
	if got := fx.total() - before; got != 0 {
		t.Errorf("string use_cache made %d extractor calls, want 0 (true honored)", got)
	}

	// String max_pages + string same_host through crawl_site.
	seed := origin3Pages(t)
	out3 := decodeOut(t, callTool(t, cs, "crawl_site", map[string]any{
		"url": seed, "max_pages": "2", "same_host": "true",
	}, ""))
	if out3["pages_crawled"].(float64) != 2 {
		t.Errorf("pages_crawled = %v, want 2 (string max_pages coerced)", out3["pages_crawled"])
	}
}

// --- Phase D additions: crawl_site scope fields + fetch telemetry in
// usage. Coercion lives at this layer; the SEMANTICS of the scope flags
// (which links survive) are locked by crawl/scope_test.go — crawl.Run has
// no fetcher seam, so honoring subdomains e2e here would mean dialing a
// fabricated subdomain; the coercion + wiring is what this layer owns.

func TestCrawlSite_ScopeFieldsCoerce(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	deps := agentDeps(db, fx, nil, &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}})
	deps.ScrapeDeps.Fetcher = nil // live localhost: nil falls back to the static fetcher
	cs := dialInMemory(t, magpiemcp.NewServer(deps), nil)

	// ALL stringy shapes coerce through the real transport: string bools,
	// string max_pages, JSON-array-string globs.
	out := decodeOut(t, callTool(t, cs, "crawl_site", map[string]any{
		"url":              origin3Pages(t),
		"max_pages":        "3",
		"max_depth":        "1",
		"same_host":        "true",
		"allow_subdomains": "true",
		"no_sitemap":       "true",
		"path_prefix":      "/",
		"include":          `["**"]`,
		"exclude":          `["**/*.pdf"]`,
	}, ""))
	if out["pages_crawled"].(float64) != 3 {
		t.Errorf("pages_crawled = %v, want 3 (stringy scope fields coerced and honored)", out["pages_crawled"])
	}
}

func TestCrawlSite_UsageHasFetchCounters(t *testing.T) {
	db := openMCPDB(t)
	fx := &fakeExtractor{script: map[string]any{"title": "Widget"}}
	deps := agentDeps(db, fx, nil, &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}})
	deps.ScrapeDeps.Fetcher = nil
	cs := dialInMemory(t, magpiemcp.NewServer(deps), nil)

	out := decodeOut(t, callTool(t, cs, "crawl_site", map[string]any{
		"url": origin3Pages(t), "max_pages": "3",
	}, ""))
	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing from crawl_site out: %v", out)
	}
	for _, k := range []string{"fetch_pages", "fetch_bytes", "fetch_ms"} {
		v, ok := usage[k].(float64)
		if !ok {
			t.Errorf("usage[%q] missing or not a number: %v", k, usage[k])
			continue
		}
		if k == "fetch_pages" && v < 1 {
			t.Errorf("fetch_pages = %v, want ≥1 after a real crawl", usage[k])
		}
	}
	// Status poll shares the same usage shape.
	runID, _ := out["run_id"].(string)
	out2 := decodeOut(t, callTool(t, cs, "crawl_site", map[string]any{"run_id": runID}, ""))
	usage2, ok := out2["usage"].(map[string]any)
	if !ok {
		t.Fatalf("status usage missing: %v", out2)
	}
	for _, k := range []string{"fetch_pages", "fetch_bytes", "fetch_ms"} {
		if _, ok := usage2[k].(float64); !ok {
			t.Errorf("status usage[%q] missing or not a number: %v", k, usage2[k])
		}
	}
}

// --- Phase G G.5: the search tool. ---

// TestMCP_ScreenshotActionRejected — H.7 gate 4: the screenshot action
// verb never crosses the MCP boundary (an action line would hand an
// agent a server-side file-write to an arbitrary path). The typed error
// is the contract; the pre-flight fires before scrape.Run, so no browser
// ever launches (a rod launch attempt in this sandbox fails with other
// text entirely).
func TestMCP_ScreenshotActionRejected(t *testing.T) {
	db := openMCPDB(t)
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil, &fakeAgentFetcher{bodies: map[string]fakeAgentResp{}})), nil)
	res := callTool(t, cs, "scrape_url", map[string]any{
		"url":     "https://example.com/x",
		"actions": []any{"click #consent", "screenshot /etc/cron.d/x"},
	}, "")
	if !res.IsError {
		t.Fatal("want tool error")
	}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"CLI-only", "screenshot", "page_format screenshot"} {
		if !strings.Contains(text, want) {
			t.Errorf("error %s missing %q", text, want)
		}
	}
	// The comment line must NOT trip the pre-flight (parser-grammar parity):
	// the same payload surfaces scrape's own validation error, never the
	// CLI-only rejection (which fires first, in handleScrape).
	res2, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "scrape_url", Arguments: map[string]any{
			"url": "https://example.com/x", "render": "static",
			"actions": []any{"# screenshot notes"},
		},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res2.IsError {
		t.Fatal("want the static+actions validation error")
	}
	text2 := toolErrText(t, res2)
	if strings.Contains(text2, "CLI-only") {
		t.Errorf("comment line tripped the screenshot rejection: %s", text2)
	}
	if !strings.Contains(text2, "browser rendering") {
		t.Errorf("comment line skipped to scrape validation: %s", text2)
	}
}

// TestMCP_SearchMissingKey: agentDeps resolves keys via APIKeyFor; a
// nil-key variant surfaces the typed missing-key error text (exit 7 at
// the CLI edge).
func TestMCP_SearchMissingKey(t *testing.T) {
	db := openMCPDB(t)
	deps := agentDeps(db, &fakeExtractor{}, nil, &fakeAgentFetcher{})
	deps.ScrapeDeps.APIKeyFor = func(string) string { return "" }
	cs := dialInMemory(t, magpiemcp.NewServer(deps), nil)
	res := callTool(t, cs, "search", map[string]any{"query": "q", "provider": "brave"}, "")
	if !res.IsError {
		t.Fatal("want tool error")
	}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"missing API key", "brave"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("content %s missing %q", raw, want)
		}
	}
}

// TestMCP_SearchUnknownProvider: the error names the valid set.
func TestMCP_SearchUnknownProvider(t *testing.T) {
	db := openMCPDB(t)
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil, &fakeAgentFetcher{})), nil)
	res := callTool(t, cs, "search", map[string]any{"query": "q", "provider": "altavista"}, "")
	if !res.IsError {
		t.Fatal("want tool error")
	}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"altavista", "brave", "serper", "serpapi", "searxng", "exa", "duckduckgo"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("content %s missing %q", raw, want)
		}
	}
}

// TestMCP_PageFormatUnion: the format error message names the new
// values (the MCP boundary sees scrape's exact validation string).
func TestMCP_PageFormatUnion(t *testing.T) {
	db := openMCPDB(t)
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil, &fakeAgentFetcher{})), nil)
	res := callTool(t, cs, "scrape_url", map[string]any{"url": "https://example.com", "page_format": "xml", "render": "static"}, "")
	if !res.IsError {
		t.Fatal("want tool error")
	}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"html", "raw", "screenshot"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("content %s missing %q", raw, want)
		}
	}
}

// TestMCP_SearchZeroKeyProviderSmoke: searxng needs no key; the call
// proceeds to the (dead local) endpoint and fails on the DIAL, not on
// key or provider validation — proving zero-key providers pass the
// pre-flight without any network. (Happy paths live in scrape's
// fixture tests; the default suite stays hermetic.)
func TestMCP_SearchZeroKeyProviderSmoke(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := dead.URL
	dead.Close() // closed port: connection refused, zero external dials
	t.Setenv("MAGPIE_SEARXNG_URL", url)
	db := openMCPDB(t)
	deps := agentDeps(db, &fakeExtractor{}, nil, &fakeAgentFetcher{})
	deps.ScrapeDeps.APIKeyFor = func(string) string { return "" }
	cs := dialInMemory(t, magpiemcp.NewServer(deps), nil)
	res := callTool(t, cs, "search", map[string]any{"query": "q", "provider": "searxng", "limit": json.Number("2")}, "")
	if !res.IsError {
		t.Fatal("dead endpoint must error")
	}
	raw, err := json.Marshal(res.Content)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "missing API key") || strings.Contains(s, "unknown") {
		t.Fatalf("zero-key provider rejected pre-flight: %s", s)
	}
	if !strings.Contains(s, "refused") && !strings.Contains(s, "connect") {
		t.Errorf("error %s should be a dial failure", s)
	}
}

// TestScrapeOut_XHRShape — the agent-facing capture surface: ScrapeOut
// marshals the xhr array when captures exist and omits the key entirely
// otherwise (additive-omitempty — no envelope drift without capture_xhr).
// (capture-xhr forces the browser path in scrape.Run, so the value-level
// round trip is the browser-tagged live test; this pins the output shape.)
func TestScrapeOut_XHRShape(t *testing.T) {
	canned := []fetch.XHRCapture{{
		URL: "https://spa.example/api/data", Status: 200,
		MIMEType: "application/json", Body: `{"ok":true}`,
	}}
	raw, err := json.Marshal(magpiemcp.ScrapeOut{URL: "https://spa.example/", XHR: canned})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	xhrs, ok := out["xhr"].([]any)
	if !ok || len(xhrs) != 1 {
		t.Fatalf("out xhr = %#v, want exactly one capture", out["xhr"])
	}
	first, _ := xhrs[0].(map[string]any)
	if first["url"] != "https://spa.example/api/data" || first["status"] != float64(200) ||
		first["mime"] != "application/json" || first["body"] != `{"ok":true}` {
		t.Errorf("capture = %#v, want the canned capture intact", first)
	}

	raw, err = json.Marshal(magpiemcp.ScrapeOut{URL: "https://spa.example/"})
	if err != nil {
		t.Fatal(err)
	}
	var bare map[string]any
	if err := json.Unmarshal(raw, &bare); err != nil {
		t.Fatal(err)
	}
	if _, present := bare["xhr"]; present {
		t.Error("nil XHR must omit the xhr key entirely")
	}
}

// TestScrapeURL_CaptureXHRStaticRejected — the options error crosses the
// MCP boundary as a tool error (house wording, no fetch).
func TestScrapeURL_CaptureXHRStaticRejected(t *testing.T) {
	db := openMCPDB(t)
	ff := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"https://x.example/": {body: agentHTML("X")},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil, ff)), nil)
	res := callTool(t, cs, "scrape_url", map[string]any{
		"url": "https://x.example/", "render": "static", "capture_xhr": []string{"/api/"},
	}, "")
	if !res.IsError {
		t.Fatal("capture_xhr + render=static must be a tool error")
	}
	text, merr := json.Marshal(res.Content)
	if merr != nil {
		t.Fatal(merr)
	}
	for _, want := range []string{"capture-xhr", "static"} {
		if !strings.Contains(string(text), want) {
			t.Errorf("error %s missing %q", text, want)
		}
	}
	if len(ff.order) != 0 {
		t.Errorf("fetcher called %v times; rejection must be pre-I/O", len(ff.order))
	}
}

// TestScrapeURL_CDPBadSchemeRejected — cdp_url is validated through the
// same scrape.Run boundary (scheme check), pre-I/O, redacted.
func TestScrapeURL_CDPBadSchemeRejected(t *testing.T) {
	db := openMCPDB(t)
	ff := &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
		"https://x.example/": {body: agentHTML("X")},
	}}
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil, ff)), nil)
	res := callTool(t, cs, "scrape_url", map[string]any{
		"url": "https://x.example/", "cdp_url": "ftp://user:pass@b:9222",
	}, "")
	if !res.IsError {
		t.Fatal("bad cdp_url scheme must be a tool error")
	}
	text, merr := json.Marshal(res.Content)
	if merr != nil {
		t.Fatal(merr)
	}
	if strings.Contains(string(text), "user:pass") {
		t.Errorf("tool error leaks credentials: %s", text)
	}
}

// TestCrawlSite_FlexBoolParams — the CrawlIn bools coerce like the house
// pattern (string "true" and JSON true; absent = nil). allow_subdomains/
// no_sitemap are pinned here because their shadow decoding shipped BROKEN
// (silently dropped pre-Phase-J) — without the pin the fix can regress
// invisibly, which is exactly what happened to them the first time.
func TestCrawlSite_FlexBoolParams(t *testing.T) {
	var in magpiemcp.CrawlIn
	if err := json.Unmarshal([]byte(`{"sitemap_only":"true","auto_throttle":true,"allow_subdomains":"true","no_sitemap":true}`), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if in.SitemapOnly == nil || !bool(*in.SitemapOnly) {
		t.Errorf("sitemap_only = %v, want true", in.SitemapOnly)
	}
	if in.AutoThrottle == nil || !bool(*in.AutoThrottle) {
		t.Errorf("auto_throttle = %v, want true", in.AutoThrottle)
	}
	if in.AllowSubdomains == nil || !bool(*in.AllowSubdomains) {
		t.Errorf("allow_subdomains = %v, want true (was silently dropped pre-Phase-J)", in.AllowSubdomains)
	}
	if in.NoSitemap == nil || !bool(*in.NoSitemap) {
		t.Errorf("no_sitemap = %v, want true (was silently dropped pre-Phase-J)", in.NoSitemap)
	}
	var absent magpiemcp.CrawlIn
	if err := json.Unmarshal([]byte(`{}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.SitemapOnly != nil || absent.AutoThrottle != nil {
		t.Error("absent bools must stay nil")
	}
}

// TestCrawlSite_SitemapConflictToolError — the crawl-side predicate fires
// at the MCP edge too; CLI and MCP can never drift.
func TestCrawlSite_SitemapConflictToolError(t *testing.T) {
	db := openMCPDB(t)
	cs := dialInMemory(t, magpiemcp.NewServer(agentDeps(db, &fakeExtractor{}, nil,
		&fakeAgentFetcher{bodies: map[string]fakeAgentResp{}})), nil)
	res := callTool(t, cs, "crawl_site", map[string]any{
		"url": "https://example.com/", "sitemap_only": true, "no_sitemap": true,
	}, "")
	if !res.IsError {
		t.Fatal("sitemap_only + no_sitemap must be a tool error")
	}
	text, merr := json.Marshal(res.Content)
	if merr != nil {
		t.Fatal(merr)
	}
	if !strings.Contains(string(text), "contradictory") {
		t.Errorf("error %s missing the contradiction wording", text)
	}
}
