package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/research"
)

// taskProvider is an OpenAI-shaped fake that answers by the "TASK: <name>"
// tag each research instruction block carries (the request body has it in
// the system prompt for the writer, after the material for the rest).
type taskProvider struct {
	mu    sync.Mutex
	tasks []string
}

var taskTag = regexp.MustCompile(`TASK: (\w+)`)

func newTaskProvider(t *testing.T, answer func(task, body string) string) *taskProvider {
	t.Helper()
	t.Setenv("MAGPIE_ANTHROPIC_API_KEY", "")
	tp := &taskProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) //nolint:errcheck // test fake
		task := ""
		if m := taskTag.FindStringSubmatch(string(b)); m != nil {
			task = m[1]
		}
		tp.mu.Lock()
		tp.tasks = append(tp.tasks, task)
		tp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openAIEnvelope(answer(task, string(b)))) //nolint:errcheck // test fake
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MAGPIE_BASE_URL", srv.URL)
	return tp
}

func (tp *taskProvider) calls() []string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]string(nil), tp.tasks...)
}

// researchPage is a static, browser-free fixture page.
func researchPage(t *testing.T, para string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><title>Fixture</title></head><body><article><h1>Fixture</h1><p>" + para + "</p>")
	for i := range 6 {
		fmt.Fprintf(&b, "<p>Background paragraph %d explains the survey method, the sample size and how respondents were chosen across regions.</p>", i)
	}
	b.WriteString(`</article><footer><a href="/a">About</a> <a href="/h">Help</a> <a href="/p">Privacy</a></footer></body></html>`)
	if s, _ := fetch.ScoreJSRequired([]byte(b.String()), http.Header{"Content-Type": {"text/html"}}); fetch.NeedsBrowser(s) {
		t.Fatalf("fixture would launch Chrome (JS score %d)", s)
	}
	return b.String()
}

func runRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetGlobals()
	stdout, stderr = captureOutput(t, func() {
		root := rootCmd()
		root.SetArgs(args)
		err = root.Execute()
	})
	return stdout, stderr, err
}

// TestResearchCmd_Validation: every misconfiguration is refused before any
// spend — on the existing exit codes, with zero requests at the model.
func TestResearchCmd_Validation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		pipe bool // stdin is a pipe, not a terminal
		key  bool
		want int
		msg  string
	}{
		{"bad effort", []string{"--effort", "max"}, false, true, 2, "effort"},
		{"bad date", []string{"--from", "2026-13-01"}, false, true, 2, "--from"},
		{"from after to", []string{"--from", "2026-02-01", "--to", "2026-01-01"}, false, true, 2, "sources.from"},
		{"unknown search backend", []string{"--search-provider", "bing"}, false, true, 2, "search"},
		{"no --yes without a terminal", nil, true, true, 2, "--yes is required"},
		{"missing key", nil, false, false, 7, "missing API key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testEnv(t, "cache.db")
			tp := newTaskProvider(t, func(string, string) string { return "{}" })
			key := ""
			if tc.key {
				key = "test-key"
			}
			t.Setenv("MAGPIE_OPENAI_API_KEY", key)
			if tc.pipe {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				old := os.Stdin
				os.Stdin = r
				t.Cleanup(func() { os.Stdin = old; _ = r.Close(); _ = w.Close() }) //nolint:errcheck // test cleanup
			} else {
				tc.args = append(tc.args, "--yes")
			}
			args := append([]string{"research", "--provider", "openai", "--search-provider", "duckduckgo"}, tc.args...)
			_, _, err := runRoot(t, append(args, "what changed?")...)
			if got := codeOf(err); got != tc.want || err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("exit %d (%v), want %d naming %q", got, err, tc.want, tc.msg)
			}
			if n := len(tp.calls()); n != 0 {
				t.Errorf("a refused run made %d model requests", n)
			}
		})
	}
}

func TestResearchCmd_Happy(t *testing.T) {
	testEnv(t, "cache.db")
	const quote = "The alpha index rose 12 percent in 2025"
	pages := map[string]string{
		"/alpha": researchPage(t, quote+" across all surveyed regions, the annual report found."),
		"/beta":  researchPage(t, "Nothing here bears on the question, though the page is long enough to read."),
	}
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, pages[r.URL.Path]) //nolint:errcheck // test server
	}))
	t.Cleanup(site.Close)
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"results":[{"title":"a","url":"%s/alpha","content":""},{"title":"b","url":"%s/beta","content":""}]}`, site.URL, site.URL) //nolint:errcheck // test server
	}))
	t.Cleanup(searx.Close)
	t.Setenv("MAGPIE_SEARXNG_URL", searx.URL)

	tp := newTaskProvider(t, func(task, body string) string {
		switch task {
		case "plan":
			return `{"brief":"How the alpha index moved.","angles":[{"question":"How did the alpha index move?","queries":["alpha index"]}]}`
		case "extract":
			if strings.Contains(body, site.URL+"/alpha") {
				return `{"facts":[{"claim":"The alpha index rose 12% in 2025.","quote":"` + quote + `","published":"","confidence":0.9,"pivotal":false,"counter_query":""}],"gaps":[],"stance":"none"}`
			}
			return `{"facts":[],"gaps":[],"stance":"none"}`
		case "judge":
			id := regexp.MustCompile(`(f\d+) \| `).FindStringSubmatch(body)
			return `{"verdicts":[{"id":"` + id[1] + `","verdict":"supported","claim":"","reason":"stated"}]}`
		case "replan":
			return `{"done":true,"reason":"answered","angles":[]}`
		case "write":
			id := regexp.MustCompile(`\[(f\d+)\] verified`).FindStringSubmatch(body)
			return "The alpha index rose 12% in 2025 [" + id[1] + "]."
		}
		return "{}"
	})
	t.Setenv("MAGPIE_OPENAI_API_KEY", "test-key")

	stdout, stderr, err := runRoot(t, "research", "--yes", "--json", "--search-provider", "searxng", "--effort", "quick",
		"--provider", "openai", "--model", "gpt-4o-mini", "How did the alpha index move in 2025?")
	if err != nil {
		t.Fatalf("research: %v\nstderr:\n%s", err, stderr)
	}
	var rep research.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not a Report: %v\n%s", err, stdout)
	}
	if rep.Status != "done" || len(rep.Cited) != 1 || !strings.Contains(rep.Markdown, site.URL+"/alpha") {
		t.Errorf("report = %s, cited %v:\n%s", rep.Status, rep.Cited, rep.Markdown)
	}
	est, search := strings.Index(stderr, "estimate:"), strings.Index(stderr, "search ")
	if est < 0 || search < 0 || est > search {
		t.Errorf("stderr must print the estimate before the first search:\n%s", stderr)
	}
	if calls := tp.calls(); !strings.Contains(strings.Join(calls, " "), "write") {
		t.Errorf("model tasks = %v, want a write", calls)
	}
}

// TestApprovePlan: a Ctrl-C while the plan was drafted or at the prompt
// declines it; --yes or "y" runs it (review of #60: the prompt must not
// turn a Ctrl-C into write-now).
func TestApprovePlan(t *testing.T) {
	plan := research.Plan{Brief: "b", Angles: []research.Angle{{Question: "q", Queries: []string{"q"}}}}
	interrupted := func() chan os.Signal {
		sig := make(chan os.Signal, 1)
		sig <- os.Interrupt
		return sig
	}
	stdin := func(t *testing.T, input string) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if input != "" {
			if _, err := io.WriteString(w, input); err != nil {
				t.Fatal(err)
			}
		}
		old := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = old; _ = w.Close(); _ = r.Close() }) //nolint:errcheck // test cleanup
	}
	for _, tc := range []struct {
		name  string
		yes   bool
		input string
		sig   chan os.Signal
		run   bool
	}{
		{"--yes", true, "", make(chan os.Signal, 1), true},
		{"Ctrl-C while drafting, --yes", true, "", interrupted(), false},
		{"y at the prompt", false, "y\n", make(chan os.Signal, 1), true},
		{"no at the prompt", false, "n\n", make(chan os.Signal, 1), false},
		{"Ctrl-C at the prompt", false, "", interrupted(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin(t, tc.input)
			var err error
			captureOutput(t, func() { _, err = approvePlan(plan, tc.yes, tc.sig) })
			if ran := err == nil; ran != tc.run || (err != nil && !errors.Is(err, errDeclined)) {
				t.Errorf("approvePlan = %v, want run %v (else errDeclined)", err, tc.run)
			}
		})
	}
}
