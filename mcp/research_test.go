package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/fetch"
	magpiemcp "github.com/motherlodelab/magpie/mcp"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const researchQuote = "The alpha index rose 12 percent in 2025"

// researchHTML is a static, browser-free page (the JS heuristic needs
// visible text and a few links).
func researchHTML(t *testing.T, para string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><title>Fixture</title></head><body><article><h1>Fixture</h1><p>" + para + "</p>")
	for i := range 6 {
		fmt.Fprintf(&b, "<p>Background paragraph %d explains the survey method, the sample size and how respondents were chosen across regions.</p>", i)
	}
	b.WriteString(`</article><footer><a href="/a">About</a> <a href="/h">Help</a> <a href="/p">Privacy</a></footer></body></html>`)
	html := []byte(b.String())
	if s, _ := fetch.ScoreJSRequired(html, http.Header{"Content-Type": {"text/html"}}); fetch.NeedsBrowser(s) {
		t.Fatalf("fixture would launch Chrome (JS score %d)", s)
	}
	return html
}

// taskLLM answers every research call by its TASK tag and logs spend the
// way the CLI's extractor closure does.
type taskLLM struct{ db *store.DB }

type boundTask struct {
	l                      *taskLLM
	provider, model, runID string
}

func (b *boundTask) Name() string { return "fake-research" }

var factID = regexp.MustCompile(`(f\d+) \| `)

func (b *boundTask) answer(task, material string) string {
	switch task {
	case "plan":
		return `{"brief":"How the alpha index moved.","angles":[{"question":"How did the alpha index move?","queries":["alpha index"]}]}`
	case "extract":
		if strings.Contains(material, "alpha.example") {
			return `{"facts":[{"claim":"The alpha index rose 12% in 2025.","quote":"` + researchQuote + `","published":"","confidence":0.9,"pivotal":false,"counter_query":""}],"gaps":[],"stance":"none"}`
		}
		return `{"facts":[],"gaps":[],"stance":"none"}`
	case "judge":
		return `{"verdicts":[{"id":"` + factID.FindStringSubmatch(material)[1] + `","verdict":"supported","claim":"","reason":"stated"}]}`
	case "replan":
		return `{"done":true,"reason":"answered","angles":[]}`
	case "write":
		return "The alpha index rose 12% in 2025 [" + regexp.MustCompile(`\[(f\d+)\]`).FindStringSubmatch(material)[1] + "]."
	}
	return "{}"
}

func taskOfText(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimPrefix(line, "TASK: ")
}

func (b *boundTask) Extract(_ context.Context, in extract.ExtractInput) (extract.ExtractResult, error) {
	raw := b.answer(taskOfText(in.PromptExtra), in.Markdown)
	if err := b.l.db.LogLLMCall(b.runID, store.LLMCall{Provider: b.provider, Model: b.model, USDEstimate: 0.001, Purpose: in.Purpose}); err != nil {
		return extract.ExtractResult{}, err
	}
	return extract.ExtractResult{Raw: json.RawMessage(raw), Provider: b.provider, Model: b.model, Attempts: 1}, nil
}

func (b *boundTask) PromptText(_ context.Context, system, user string) (string, extract.TokenUsage, error) {
	return b.answer(taskOfText(system), user), extract.TokenUsage{USDEstimate: 0.001}, nil
}

// researchServer is one hermetic research world behind the MCP server:
// searxng over httptest, a fake fetcher, the task LLM.
func researchServer(t *testing.T, maxCost float64, opts *sdk.ClientOptions) (*sdk.ClientSession, *store.DB) {
	t.Helper()
	db := openMCPDB(t)
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"results":[{"title":"a","url":"https://alpha.example/report","content":""},{"title":"b","url":"https://beta.example/notes","content":""}]}`) //nolint:errcheck // test server
	}))
	t.Cleanup(searx.Close)
	t.Setenv("MAGPIE_SEARXNG_URL", searx.URL)
	l := &taskLLM{db: db}
	srv := magpiemcp.NewServer(magpiemcp.Deps{
		DB: db,
		ScrapeDeps: scrape.Deps{
			DB: db,
			Fetcher: &fakeAgentFetcher{bodies: map[string]fakeAgentResp{
				"alpha.example/report": {body: researchHTML(t, researchQuote+" across all surveyed regions, the annual report found.")},
				"beta.example/notes":   {body: researchHTML(t, "Nothing on this page bears on the question, though it is long enough to read.")},
			}},
			ExtractorFor: func(p, _, m string, _ *extract.Schema, runID string) (extract.Extractor, error) {
				return &boundTask{l: l, provider: p, model: m, runID: runID}, nil
			},
			APIKeyFor: func(string) string { return "test-key" },
		},
		DefaultProvider: "openai", DefaultModel: "gpt-4o-mini", MaxCost: maxCost,
	})
	return dialInMemory(t, srv, opts), db
}

var researchArgs = map[string]any{"question": "How did the alpha index move in 2025?", "effort": "quick", "search_providers": []string{"searxng"}}

func TestResearch_Tool(t *testing.T) {
	var mu sync.Mutex
	notes := 0
	cs, _ := researchServer(t, 0, &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			defer mu.Unlock()
			notes++
			if req.Params.Message == "" {
				t.Error("progress notification with empty message")
			}
		},
	})
	out := decodeOut(t, callTool(t, cs, "research", researchArgs, "r1"))
	if out["status"] != "done" || !strings.Contains(fmt.Sprint(out["report"]), "https://alpha.example/report") {
		t.Errorf("out = %v, want done with a report citing alpha", out)
	}
	cited, _ := out["cited"].([]any)
	if len(cited) != 1 || cited[0].(map[string]any)["quote"] != researchQuote {
		t.Errorf("cited = %v, want alpha's one fact", out["cited"])
	}
	mu.Lock()
	defer mu.Unlock()
	if notes < 1 {
		t.Error("0 progress notifications, want ≥ 1")
	}
}

func TestResearch_Poll(t *testing.T) {
	cs, db := researchServer(t, 0, nil)
	first := decodeOut(t, callTool(t, cs, "research", researchArgs, ""))
	before := llmCalls(t, db)
	poll := decodeOut(t, callTool(t, cs, "research", map[string]any{"run_id": first["run_id"]}, ""))
	if poll["status"] != first["status"] || poll["report"] != first["report"] || poll["verified"] != first["verified"] {
		t.Errorf("poll = %v, want the run's status, report and counts %v", poll, first)
	}
	if after := llmCalls(t, db); after != before {
		t.Errorf("a poll made %d LLM calls, want 0", after-before)
	}
}

func TestResearch_DefaultCap(t *testing.T) {
	cs, db := researchServer(t, 0, nil)
	out := decodeOut(t, callTool(t, cs, "research", researchArgs, ""))
	rr, err := db.GetResearchRun(fmt.Sprint(out["run_id"]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Options, `"max_cost_usd":1`) {
		t.Errorf("stored options %s, want the $1 default cap", rr.Options)
	}
}

func TestResearch_Errors(t *testing.T) {
	cs, _ := researchServer(t, 0, nil)
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"effort": "quick"}, "question"},
		{map[string]any{"question": "q", "from": "2026-13-01"}, "from"},
	} {
		if msg := toolErrText(t, callTool(t, cs, "research", tc.args, "")); !strings.Contains(msg, tc.want) {
			t.Errorf("error %s does not name %q", msg, tc.want)
		}
	}
}
