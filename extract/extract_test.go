package extract_test

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

	"github.com/motherlodelab/magpie/extract"
)

type fakeProvider struct {
	t      *testing.T
	mu     sync.Mutex
	script []string
	calls  int
	bodies []string
	// headers parallels bodies: one cloned header map per call, so the
	// session/Referer/UA wire contract is assertable.
	headers []http.Header
	status  int
}

func newFakeProvider(t *testing.T, script ...string) (*httptest.Server, *fakeProvider) {
	t.Helper()
	// Hermetic by contract: ambient MAGPIE_* env (real endpoints/keys)
	// must never redirect the fake provider's adapter.
	t.Setenv("MAGPIE_BASE_URL", "")
	t.Setenv("MAGPIE_OPENAI_API_KEY", "")
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "")
	t.Setenv("MAGPIE_OLLAMA_URL", "")
	fp := &fakeProvider{t: t, script: script, status: 200}
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
		w.WriteHeader(fp.status)
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

func (f *fakeProvider) lastBody(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal([]byte(f.bodies[len(f.bodies)-1]), &m); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	return m
}

func openAIEnvelope(raw string) string {
	b, merr := json.Marshal(raw)
	if merr != nil {
		panic(merr) // marshaling a string cannot fail
	}
	return `{"choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
}

func anthropicEnvelope(raw string) string {
	b, merr := json.Marshal(raw)
	if merr != nil {
		panic(merr) // marshaling a string cannot fail
	}
	return `{"content":[{"type":"text","text":` + string(b) + `}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
}

// openAIEnvelopeCost wraps raw output in a chat-completions body whose
// usage includes provider-computed "cost" (the OpenRouter shape).
func openAIEnvelopeCost(raw string, cost float64) string {
	b, merr := json.Marshal(raw)
	if merr != nil {
		panic(merr) // marshaling a string cannot fail
	}
	return `{"choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":` + itoa(cost) + `}}`
}

func itoa(f float64) string {
	b, merr := json.Marshal(f) // canonical float rendering
	if merr != nil {
		panic(merr) // marshaling a float cannot fail
	}
	return string(b)
}

func mustLoadSchema(t *testing.T, path string) *extract.Schema {
	t.Helper()
	sch, err := extract.LoadSchema(path)
	if err != nil {
		t.Fatalf("LoadSchema(%s): %v", path, err)
	}
	return sch
}

func TestRepairLoopInvalidThenValid(t *testing.T) {
	srv, fp := newFakeProvider(t,
		openAIEnvelope(`{"name":"Widget","price":"12.99"}`),
		openAIEnvelope(`{"name":"Widget","price":12.99}`),
	)
	ex := extract.NewOpenAI(srv.URL, "test-key", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	got, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget\n\nPrice: 12.99"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fp.callCount() != 2 {
		t.Fatalf("calls = %d, want exactly 2", fp.callCount())
	}
	if !strings.Contains(fp.bodies[1], "at '/price'") {
		t.Errorf("repair prompt missing validator text; body[1] = %q", fp.bodies[1])
	}
	if got.Record["price"] != 12.99 {
		t.Errorf("price = %v, want 12.99", got.Record["price"])
	}
}

func TestRepairLoopExhausted(t *testing.T) {
	srv, fp := newFakeProvider(t,
		openAIEnvelope(`{"name":"Widget","price":"x"}`),
		openAIEnvelope(`{"name":"Widget","price":"y"}`),
		openAIEnvelope(`{"name":"Widget","price":"z"}`),
	)
	ex := extract.NewOpenAI(srv.URL, "test-key", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err == nil {
		t.Fatal("expected error after 3 invalid attempts, got nil")
	}
	if !strings.Contains(err.Error(), "at '/price'") {
		t.Errorf("error missing validator text: %v", err)
	}
	if fp.callCount() != 3 {
		t.Errorf("calls = %d, want 3", fp.callCount())
	}
}

func TestAnthropicRepair(t *testing.T) {
	srv, fp := newFakeProvider(t,
		anthropicEnvelope(`{"name":"Widget","price":"12.99"}`),
		anthropicEnvelope(`{"name":"Widget","price":12.99}`),
	)
	ex := extract.NewAnthropic(srv.URL, "test-key", "claude-sonnet-5", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fp.callCount() != 2 {
		t.Fatalf("calls = %d, want 2", fp.callCount())
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(fp.bodies[0]), &req); err != nil {
		t.Fatalf("request not JSON: %v", err)
	}
	if _, ok := req["output_config"]; !ok {
		t.Error("anthropic request missing output_config")
	}
}

func TestTruncation(t *testing.T) {
	srv, _ := newFakeProvider(t, `{"content":[{"type":"text","text":"{}"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`)
	ex := extract.NewAnthropic(srv.URL, "k", "claude-sonnet-5", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "x"})
	if err == nil || !strings.Contains(err.Error(), "truncat") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

func TestSchemaErrorText(t *testing.T) {
	sch := mustLoadSchema(t, "../testdata/extract/price.yaml")
	raw, err := os.ReadFile("../testdata/extract/invalid-doc.json")
	if err != nil {
		t.Fatal(err)
	}
	err = sch.Validate(raw)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "at '/price'") {
		t.Errorf("error text = %q, want at '/price'", err.Error())
	}
}

func TestUnknownCoerce(t *testing.T) {
	_, err := extract.ParseSchema([]byte(`{"type":"object","properties":{"a":{"type":"string","x-magpie":{"coerce":"frobnicate"}}}}`))
	if err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("expected unknown-coerce error, got %v", err)
	}
}

func TestCoerceEURDecimal(t *testing.T) {
	cases := map[string]struct {
		in   string
		want float64
	}{
		"euro suffix + comma": {"1 234,56 €", 1234.56},
		"plain":               {"12.99", 12.99},
		"symbol only":         {"€7,5", 7.5},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := extract.Coerce("eur_decimal", c.in)
			if err != nil {
				t.Fatalf("Coerce: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestCoerceVectors(t *testing.T) {
	for _, tc := range []struct {
		kind string
		in   string
		want any
	}{{"int", "42", 42}, {"trim", "  x  ", "x"}, {"bool", "ja", true}} {
		v, err := extract.Coerce(tc.kind, tc.in)
		if err != nil {
			t.Fatalf("Coerce(%s): %v", tc.kind, err)
		}
		if v != tc.want {
			t.Errorf("Coerce(%s) = %v, want %v", tc.kind, v, tc.want)
		}
	}
	if v, err := extract.Coerce("iso_date", "12.03.2024"); err != nil || v != "2024-03-12" {
		t.Errorf("iso_date: %v %v", v, err)
	}
	if v, err := extract.ApplyRegex(`[0-9]+[.,][0-9]{2}`, "price 12,99 EUR"); err != nil || v != "12,99" {
		t.Errorf("regex: %v %v", v, err)
	}
}

func TestJSONLDWalk(t *testing.T) {
	side := json.RawMessage(`{"offers":{"price":"49.99"}}`)
	v, ok := extract.JSONLDWalk(side, "$.offers.price")
	if !ok || v != "49.99" {
		t.Errorf("walk: %v %v", v, ok)
	}
}

func TestCostTable(t *testing.T) {
	if c := extract.EstimateCost("gpt-4o-mini", 1_000_000, 1_000_000); c != 0.75 {
		t.Errorf("cost = %v, want 0.75", c)
	}
	if c := extract.EstimateCost("no-such-model", 1000, 1000); c != 0 {
		t.Errorf("unknown model cost = %v, want 0", c)
	}
	if c := extract.ProjectedCost("gpt-4o-mini", strings.Repeat("x", 4000)); c <= 0 {
		t.Errorf("projected = %v, want > 0", c)
	}
}

func TestExtractPurposeDefault(t *testing.T) {
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	var purposes []string
	ex := extract.NewOpenAI(srv.URL, "test-key", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Log = func(purpose string, _ extract.TokenUsage) { purposes = append(purposes, purpose) }
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if len(purposes) != 1 || purposes[0] != "extract" {
		t.Errorf("purposes = %v, want [extract]", purposes)
	}
}

func TestExtractPurposeSynth(t *testing.T) {
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	var purposes []string
	ex := extract.NewOpenAI(srv.URL, "test-key", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Log = func(purpose string, _ extract.TokenUsage) { purposes = append(purposes, purpose) }
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget", Purpose: "synth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(purposes) != 1 || purposes[0] != "synth" {
		t.Errorf("purposes = %v, want [synth]", purposes)
	}
}

func TestExtractPurposeRepairStillRepair(t *testing.T) {
	srv, _ := newFakeProvider(t,
		openAIEnvelope(`{"name":"Widget","price":"12.99"}`),
		openAIEnvelope(`{"name":"Widget","price":12.99}`),
	)
	var purposes []string
	ex := extract.NewOpenAI(srv.URL, "test-key", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Log = func(purpose string, _ extract.TokenUsage) { purposes = append(purposes, purpose) }
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget", Purpose: "synth"})
	if err != nil {
		t.Fatal(err)
	}
	if len(purposes) != 2 || purposes[0] != "synth" || purposes[1] != "repair" {
		t.Errorf("purposes = %v, want [synth repair]", purposes)
	}
}

// ---- LLM providers (codex exec + openrouter + opencode go/zen) ----

// logCall is one DB-free Log capture: provider/cost without SQLite.
type logCall struct {
	purpose string
	usage   extract.TokenUsage
}

func captureLog(logs *[]logCall) func(string, extract.TokenUsage) {
	return func(purpose string, u extract.TokenUsage) {
		*logs = append(*logs, logCall{purpose, u})
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	if err := w.Close(); err != nil {
		t.Errorf("close stderr pipe: %v", err)
	}
	os.Stderr = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	return string(out)
}

// stubCodexScript replaces the Codex CLI: canned exec backend recording
// argv+stdin to $CALL_LOG. Modes via $STUB_MODE: ok | no-usage |
// invalid-then-valid | old | fail.
//
// Deviations from plan/llm-providers-tests.md §3 (required, else the planned
// assertions are impossible): answers --help like a current CLI (a verbatim
// stub would fail preflight in ok mode), and dumps the --output-schema file after a ---schema--- marker (prod removes the temp
// dir, so tests could never read the file otherwise).
const stubCodexScript = `#!/bin/sh
LOG="$CALL_LOG"
echo "argv: $*" >> "$LOG"
cat >> "$LOG"
out="" ; schema=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  if [ "$prev" = "--output-schema" ]; then schema="$a"; fi
  prev="$a"
done
case "$STUB_MODE" in
  old) echo "codex 0.1.0"; exit 0;;
  fail) echo "boom" >&2; exit 1;;
esac
if [ "$1" = "--help" ]; then
  echo "Usage: codex exec [...] --output-schema <file> --json [...]"
  exit 0
fi
if [ -n "$schema" ] && [ -f "$schema" ]; then
  echo "---schema---" >> "$LOG"
  cat "$schema" >> "$LOG"
fi
if [ "$STUB_MODE" = "invalid-then-valid" ] && [ ! -f "$LOG.done" ]; then
  touch "$LOG.done"
  echo '{"type":"thread.started","thread_id":"t"}'
  echo '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}'
  echo '{"name":"Widget","price":"x"}' > "$out"
  exit 0
fi
echo '{"type":"thread.started","thread_id":"t"}'
if [ "$STUB_MODE" != "no-usage" ]; then
  echo '{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":5}}'
fi
echo '{"name":"Widget","price":12.99}' > "$out"
exit 0
`

func stubCodex(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(stubCodexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CALL_LOG", filepath.Join(dir, "calls.log"))
	t.Setenv("STUB_MODE", mode)
	return filepath.Join(dir, "calls.log")
}

func stubCalls(t *testing.T, logPath string) string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	return string(raw)
}

// schemaFromLog validates the record against the schema payload the stub
// actually received on --output-schema (no checked-in fixture to rot, and
// no circular validation against the fixture the adapter was built with).
func schemaFromLog(t *testing.T, calls string, rec map[string]any) {
	t.Helper()
	parts := strings.Split(calls, "---schema---")
	if len(parts) < 2 {
		t.Fatalf("stub log missing ---schema--- marker; log:\n%s", calls)
	}
	received, err := extract.ParseSchema([]byte(strings.TrimSpace(parts[len(parts)-1])))
	if err != nil {
		t.Fatalf("stub-received schema not parseable: %v", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := received.Validate(raw); err != nil {
		t.Fatalf("record fails stub-received schema: %v", err)
	}
}

func TestExtraHeaders_BodyExtraMerge(t *testing.T) {
	srv, fp := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	ex := extract.NewOpenAI(srv.URL, "k", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.ExtraHeaders = map[string]string{"HTTP-Referer": "https://example.com/x", "X-Title": "magpie"}
	ex.BodyExtra = map[string]any{"provider": map[string]any{"require_parameters": true}}
	if _, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"}); err != nil {
		t.Fatal(err)
	}
	if got := fp.lastHeader("HTTP-Referer"); got != "https://example.com/x" {
		t.Errorf("Referer = %q, want example URL", got)
	}
	if got := fp.lastHeader("X-Title"); got != "magpie" {
		t.Errorf("X-Title = %q, want magpie", got)
	}
	body := fp.lastBody(t)
	req, _ := body["provider"].(map[string]any)
	if req["require_parameters"] != true {
		t.Errorf("body provider.require_parameters = %v, want true", req)
	}
}

func TestAdapterIdentity_ProviderName(t *testing.T) {
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	var logs []logCall
	ex := extract.NewOpenAI(srv.URL, "k", "anthropic/claude-sonnet-4-6", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Provider = "openrouter"
	ex.Log = captureLog(&logs)
	if ex.Name() != "openrouter" {
		t.Fatalf("Name() = %q, want openrouter (leaks usage.provider rows)", ex.Name())
	}
	got, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "openrouter" {
		t.Errorf("result provider = %q, want openrouter", got.Provider)
	}
	a := extract.NewAnthropic(srv.URL, "k", "claude-sonnet-4-6", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	if a.Name() != "anthropic" {
		t.Errorf("default Name() = %q, want anthropic", a.Name())
	}
	o := extract.NewOpenAI(srv.URL, "k", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	if o.Name() != "openai" {
		t.Errorf("default Name() = %q, want openai", o.Name())
	}
}

func TestOpenRouter_RequireParametersAndCost(t *testing.T) {
	srv, fp := newFakeProvider(t, openAIEnvelopeCost(`{"name":"Widget","price":12.99}`, 0.0042))
	var logs []logCall
	ex := extract.NewOpenAI(srv.URL, "k", "anthropic/claude-sonnet-4-6", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Provider = "openrouter"
	ex.ExtraHeaders = map[string]string{"HTTP-Referer": "https://github.com/you/magpie", "X-Title": "magpie"}
	ex.BodyExtra = map[string]any{"provider": map[string]any{"require_parameters": true}}
	ex.Log = captureLog(&logs)
	if _, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	body := fp.lastBody(t)
	req, _ := body["provider"].(map[string]any)
	if req["require_parameters"] != true {
		t.Errorf("body provider.require_parameters = %v, want true (else schema is a hint)", req)
	}
	if fp.lastHeader("HTTP-Referer") == "" {
		t.Error("missing HTTP-Referer header")
	}
	if len(logs) != 1 || logs[0].usage.USDEstimate != 0.0042 {
		t.Errorf("cost = %v, want exactly 0.0042 from usage.cost (not the price table)", logs)
	}
}

func TestOpenRouter_CostFallbackAbsent(t *testing.T) {
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	var logs []logCall
	ex := extract.NewOpenAI(srv.URL, "k", "gpt-4o-mini", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Provider = "openrouter"
	ex.Log = captureLog(&logs)
	if _, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"}); err != nil {
		t.Fatal(err)
	}
	// Fake envelope carries 10 prompt + 5 completion tokens.
	want := extract.EstimateCost("gpt-4o-mini", 10, 5)
	if len(logs) != 1 || logs[0].usage.USDEstimate != want {
		t.Errorf("cost = %v, want table fallback %v", logs, want)
	}
}

func TestZen_RouteTable(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"minimax-m3", true},
		{"qwen3.7-max", true},
		{"qwen2-flash", true},
		{"claude-sonnet-4-6", true},
		{"CLAUDE-SONNET-4-6", true},
		{"glm-5.3", false},
		{"kimi-k3", false},
		{"deepseek-v4-flash", false},
		{"gpt-5.2", false},
	} {
		if got := extract.ZenUsesMessages(tc.model); got != tc.want {
			t.Errorf("ZenUsesMessages(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestZen_UnknownPrefixWarns(t *testing.T) {
	stderr := captureStderr(t, func() {
		if got := extract.ZenUsesMessages("something-new-9"); got != false {
			t.Errorf("unknown prefix routes %v, want OpenAI (false)", got)
		}
	})
	if !strings.Contains(stderr, "something-new-9") {
		t.Errorf("stderr = %q, want note naming the model", stderr)
	}
}

func TestZen_SessionHeaders(t *testing.T) {
	// Chat/completions leg (opencode-go base shape).
	srv, fp := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	oa := extract.NewOpenAI(srv.URL, "k", "glm-5.3", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	oa.Provider = "opencode-go"
	oa.SessionID = "run-123"
	oa.ExtraHeaders = map[string]string{"User-Agent": extract.MagpieUA}
	if _, err := oa.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"}); err != nil {
		t.Fatal(err)
	}
	if got := fp.lastHeader("x-opencode-session"); got != "run-123" {
		t.Errorf("session header = %q, want run-123 byte-identical", got)
	}
	if !strings.Contains(fp.lastHeader("User-Agent"), "magpie") {
		t.Errorf("UA = %q, want magpie", fp.lastHeader("User-Agent"))
	}
	// /messages leg (opencode-zen base shape).
	srv2, fp2 := newFakeProvider(t, anthropicEnvelope(`{"name":"Widget","price":12.99}`))
	an := extract.NewAnthropic(srv2.URL, "k", "claude-sonnet-4-6", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	an.Provider = "opencode-zen"
	an.SessionID = "run-456"
	an.ExtraHeaders = map[string]string{"User-Agent": extract.MagpieUA}
	if _, err := an.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"}); err != nil {
		t.Fatal(err)
	}
	if got := fp2.lastHeader("x-opencode-session"); got != "run-456" {
		t.Errorf("session header = %q, want run-456 byte-identical", got)
	}
}

func TestIsFlatRate(t *testing.T) {
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{"codex", true}, {"opencode-go", true},
		{"openai", false}, {"anthropic", false}, {"ollama", false},
		{"openrouter", false}, {"opencode-zen", false},
	} {
		if got := extract.IsFlatRateProvider(tc.p); got != tc.want {
			t.Errorf("IsFlatRateProvider(%q) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestFlatRate_NoCostWarning(t *testing.T) {
	srv, _ := newFakeProvider(t, openAIEnvelope(`{"name":"Widget","price":12.99}`))
	ex := extract.NewOpenAI(srv.URL, "k", "no-such-model-9", mustLoadSchema(t, "../testdata/extract/price.yaml"))
	ex.Provider = "opencode-go" // flat bill: unknown model stays 0, quietly
	stderr := captureStderr(t, func() {
		if _, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"}); err != nil {
			t.Errorf("Extract: %v", err)
		}
	})
	if strings.Contains(stderr, "unknown model") {
		t.Errorf("flat-rate run warned %q (bill is fixed; noise is not signal)", stderr)
	}
}

func TestCodex_ExecOK(t *testing.T) {
	logPath := stubCodex(t, "ok")
	sch := mustLoadSchema(t, "../testdata/extract/price.yaml")
	var logs []logCall
	ex := extract.NewCodexExec("gpt-5.2", sch, captureLog(&logs))
	if ex.Name() != "codex" {
		t.Fatalf("Name() = %q, want codex", ex.Name())
	}
	got, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget\n\nPrice: 12.99"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.Record["price"] != 12.99 {
		t.Errorf("price = %v, want 12.99", got.Record["price"])
	}
	if got.Provider != "codex" {
		t.Errorf("provider = %q, want codex", got.Provider)
	}
	calls := stubCalls(t, logPath)
	for _, want := range []string{"--json", "--output-schema", "--ephemeral", "--skip-git-repo-check", "--ignore-user-config", "-o", "-m", "gpt-5.2", "-"} {
		if !strings.Contains(calls, want) {
			t.Errorf("codex argv missing %q; log:\n%s", want, calls)
		}
	}
	if !strings.Contains(calls, "# Widget") {
		t.Error("prompt never reached codex stdin")
	}
	schemaFromLog(t, calls, got.Record)
	if len(logs) != 1 {
		t.Fatalf("log calls = %d, want 1", len(logs))
	}
	if logs[0].usage.PromptTokens != 100 || logs[0].usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v, want 100 prompt / 5 completion from turn.completed", logs[0].usage)
	}
}

func TestCodex_ExecMCPPoisonedConfig(t *testing.T) {
	// The user's real config carries an npx MCP server (issue #15451 class):
	// the record must still validate because we pass --ignore-user-config.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[mcp_servers.npx-evil]\ncommand = "npx"\nargs = ["-y", "evil-server"]\n`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	logPath := stubCodex(t, "ok")
	var logs []logCall
	ex := extract.NewCodexExec("gpt-5.2", mustLoadSchema(t, "../testdata/extract/price.yaml"), captureLog(&logs))
	got, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget\n\nPrice: 12.99"})
	if err != nil {
		t.Fatalf("Extract with poisoned CODEX_HOME: %v", err)
	}
	if got.Record["price"] != 12.99 {
		t.Errorf("price = %v, want 12.99", got.Record["price"])
	}
	calls := stubCalls(t, logPath)
	if !strings.Contains(calls, "--ignore-user-config") {
		t.Errorf("argv missing --ignore-user-config; log:\n%s", calls)
	}
	// The poisoned server must never reach the CLI at all — neither via
	// argv, stdin, nor the schema file. (The stub would only see it if
	// the adapter forwarded CODEX_HOME content somewhere.)
	if strings.Contains(calls, "npx") {
		t.Errorf("poisoned MCP config leaked into the codex invocation; log:\n%s", calls)
	}
}

func TestCodex_NoUsageFallback(t *testing.T) {
	stubCodex(t, "no-usage")
	var logs []logCall
	ex := extract.NewCodexExec("gpt-5.2", mustLoadSchema(t, "../testdata/extract/price.yaml"), captureLog(&logs))
	got, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.Record["price"] != 12.99 {
		t.Errorf("price = %v, want 12.99", got.Record["price"])
	}
	if len(logs) != 1 || logs[0].usage != (extract.TokenUsage{}) {
		t.Errorf("usage = %v, want zeroed (telemetry gaps must not fail records)", logs)
	}
}

func TestCodex_RepairLoop(t *testing.T) {
	logPath := stubCodex(t, "invalid-then-valid")
	ex := extract.NewCodexExec("gpt-5.2", mustLoadSchema(t, "../testdata/extract/price.yaml"), nil)
	got, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.Record["price"] != 12.99 {
		t.Errorf("price = %v, want 12.99", got.Record["price"])
	}
	calls := stubCalls(t, logPath)
	if n := strings.Count(calls, "argv: exec"); n != 2 {
		t.Errorf("exec invocations = %d, want exactly 2", n)
	}
	if !strings.Contains(calls, "at '/price'") {
		t.Errorf("second prompt missing validator text; log:\n%s", calls)
	}
}

func TestCodex_Fail(t *testing.T) {
	stubCodex(t, "fail")
	ex := extract.NewCodexExec("gpt-5.2", mustLoadSchema(t, "../testdata/extract/price.yaml"), nil)
	_, err := ex.Extract(t.Context(), extract.ExtractInput{Markdown: "# Widget"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stub stderr surfaced, got %v", err)
	}
}

func TestCodex_OldVersion(t *testing.T) {
	logPath := stubCodex(t, "old")
	ex := extract.NewCodexExec("gpt-5.2", mustLoadSchema(t, "../testdata/extract/price.yaml"), nil)
	err := ex.Preflight()
	if err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("expected upgrade error, got %v", err)
	}
	if calls := stubCalls(t, logPath); strings.Contains(calls, "# Widget") {
		t.Errorf("prompt bytes reached the CLI before preflight failed; log:\n%s", calls)
	}
}

func TestCodex_NoBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no codex anywhere
	ex := extract.NewCodexExec("gpt-5.2", mustLoadSchema(t, "../testdata/extract/price.yaml"), nil)
	err := ex.Preflight()
	if err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("expected codex-login error, got %v", err)
	}
}
