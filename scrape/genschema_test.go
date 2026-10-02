package scrape_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

const validDraft = `{"type":"object","title":"Book","properties":{"title":{"type":"string"},"price":{"type":"number"}}}`

// draftDeps wires the scripted prompter over a fresh store. openai +
// gpt-4o-mini (priced, no ceiling) keeps the cost fallback quiet.
func draftDeps(t *testing.T, fx *fakePrompterExtractor) scrape.Deps {
	t.Helper()
	return scrape.Deps{
		DB: openScrapeDB(t),
		ExtractorFor: func(_, _, _ string, _ *extract.Schema, _ string) (extract.Extractor, error) {
			return fx, nil
		},
		APIKeyFor: func(string) string { return "test-key" },
	}
}

func draft(d scrape.Deps, desc, sample string) (scrape.DraftOut, error) {
	return scrape.DraftSchema(context.Background(), d, scrape.DraftOptions{
		Description: desc, Sample: sample, Provider: "openai", Model: "gpt-4o-mini",
	})
}

// draftLedger returns the one run row and every llm_calls purpose.
func draftLedger(t *testing.T, db *store.DB) (store.RunInfo, []string) {
	t.Helper()
	runs, err := db.ListRuns(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Command != "schema" {
		t.Fatalf("run rows = %+v, want exactly one schema run", runs)
	}
	calls, err := db.LLMCalls("")
	if err != nil {
		t.Fatal(err)
	}
	var purposes []string
	for _, c := range calls {
		purposes = append(purposes, c.Purpose)
	}
	return runs[0], purposes
}

// TestDraftSchema_Valid: a fenced reply is accepted on the first attempt,
// returned fence-free and indented in the model's own key order (a map
// round-trip would sort "type" after "properties"), with its title.
func TestDraftSchema_Valid(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"```json\n" + validDraft + "\n```"}}
	d := draftDeps(t, fx)
	out, err := draft(d, "book title and price", "")
	if err != nil {
		t.Fatalf("DraftSchema: %v", err)
	}
	if strings.Contains(out.Schema, "`") {
		t.Errorf("schema keeps the fence:\n%s", out.Schema)
	}
	if !strings.Contains(out.Schema, "\n  \"") {
		t.Errorf("schema is not indented:\n%s", out.Schema)
	}
	if ti, pi := strings.Index(out.Schema, `"type"`), strings.Index(out.Schema, `"properties"`); ti < 0 || ti > pi {
		t.Errorf(`"type" at %d, "properties" at %d: want the model's key order kept`, ti, pi)
	}
	if out.Title != "Book" || out.Attempts != 1 || out.Provider != "openai" || out.Model != "gpt-4o-mini" {
		t.Errorf("out = %+v, want title Book, 1 attempt, openai/gpt-4o-mini", out)
	}
	run, purposes := draftLedger(t, d.DB)
	if run.Status != "finished" || run.PagesOK != 1 {
		t.Errorf("run = %s ok %d, want finished ok 1", run.Status, run.PagesOK)
	}
	if strings.Join(purposes, ",") != "schema" {
		t.Errorf("purposes = %v, want [schema]", purposes)
	}
	if !strings.HasPrefix(fx.users[0], "Description: book title and price") {
		t.Errorf("user prompt = %q, want it to lead with the description", fx.users[0])
	}
}

// TestDraftSchema_Repair: the first reply is a single-line fence (the old
// index-slice strip panicked on it) and must repair as "not JSON"; the
// second compiles but has no property; the third is usable. Each repair
// carries the error verbatim and is logged as "repair".
func TestDraftSchema_Repair(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"```{\"type\":\"object\"}```", `{"type":"object"}`, validDraft}}
	d := draftDeps(t, fx)
	out, err := draft(d, "book title and price", "")
	if err != nil {
		t.Fatalf("DraftSchema: %v", err)
	}
	if out.Attempts != 3 || len(fx.users) != 3 {
		t.Fatalf("attempts = %d, want 3", out.Attempts)
	}
	if !strings.Contains(fx.users[1], "not JSON") || !strings.Contains(fx.users[2], "at least one property") {
		t.Errorf("repair prompts miss the verbatim errors:\n%s\n---\n%s", fx.users[1], fx.users[2])
	}
	if out.Usage.PromptTokens != 30 || out.Usage.CompletionTokens != 15 {
		t.Errorf("usage = %+v, want 30/15 summed over 3 attempts", out.Usage)
	}
	if _, purposes := draftLedger(t, d.DB); strings.Join(purposes, ",") != "schema,repair,repair" {
		t.Errorf("purposes = %v, want schema,repair,repair", purposes)
	}
}

// TestDraftSchema_GivesUp: three unusable replies end in one error and an
// error run row.
func TestDraftSchema_GivesUp(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{"no schema here"}}
	d := draftDeps(t, fx)
	_, err := draft(d, "book title", "")
	if err == nil || !strings.Contains(err.Error(), "still unusable after 3 attempts") {
		t.Fatalf("err = %v, want the give-up error", err)
	}
	if fx.total() != 3 {
		t.Errorf("calls = %d, want 3", fx.total())
	}
	if run, _ := draftLedger(t, d.DB); run.Status != "error" {
		t.Errorf("run status = %s, want error", run.Status)
	}
}

// TestDraftSchema_Input: a blank or oversized description fails before any
// call or run row (runes, not bytes); the sample is capped on the way in.
func TestDraftSchema_Input(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptScript: []string{validDraft}}
	d := draftDeps(t, fx)
	for _, desc := range []string{"", "  \n\t ", strings.Repeat("a", 2001)} {
		if _, err := draft(d, desc, ""); err == nil {
			t.Errorf("description of %d bytes: err = nil, want a refusal", len(desc))
		}
	}
	if fx.total() != 0 {
		t.Errorf("calls = %d, want 0 (validated before Prompt)", fx.total())
	}
	if runs, err := d.DB.ListRuns(0); err != nil || len(runs) != 0 {
		t.Errorf("runs = %v (%v), want none (validated before BeginRun)", runs, err)
	}
	if _, err := draft(d, strings.Repeat("é", 2000), ""); err != nil {
		t.Errorf("2000 runes (4000 bytes): %v, want accepted (the cap counts runes)", err)
	}
	if _, err := draft(d, "book", strings.Repeat("word ", 5000)); err != nil {
		t.Fatalf("DraftSchema with sample: %v", err)
	}
	_, sample, ok := strings.Cut(fx.lastUser(), "values):\n")
	if !ok {
		t.Fatalf("user prompt has no sample section:\n%.200s", fx.lastUser())
	}
	if n := len(strings.Fields(sample)); n != scrape.MaxDraftSampleWords {
		t.Errorf("sample reached the prompt with %d words, want the %d cap", n, scrape.MaxDraftSampleWords)
	}
}

// TestDraftSchema_ProviderError: a provider error returns at once — no
// repair attempt — and the run row lands error.
func TestDraftSchema_ProviderError(t *testing.T) {
	fx := &fakePrompterExtractor{t: t, promptErr: errors.New("model exploded")}
	d := draftDeps(t, fx)
	_, err := draft(d, "book title", "")
	if err == nil || !strings.Contains(err.Error(), "model exploded") {
		t.Fatalf("err = %v, want the provider error verbatim", err)
	}
	if fx.total() != 1 {
		t.Errorf("calls = %d, want exactly 1 (never repaired)", fx.total())
	}
	if run, _ := draftLedger(t, d.DB); run.Status != "error" {
		t.Errorf("run status = %s, want error", run.Status)
	}
}

// TestDraftSchema_RejectsRefs pins the prompt-injection guard (the schema
// compiler's default loader reads local files). Leg one's target WOULD
// compile, so without the guard the reply is accepted on attempt 1 — the
// leak. Leg two's target is missing and its key is a JSON escape nested in
// "items": the guard's message (not the loader's "no such file") proves it
// ran before ParseSchema read anything, and decode-then-walk, not a
// raw-text grep, is what finds the key.
func TestDraftSchema_RejectsRefs(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.json")
	if err := os.WriteFile(target, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := "file://" + filepath.ToSlash(filepath.Join(dir, "missing.json"))
	escaped := `{"type":"object","title":"Book","properties":{"x":{"type":"array","items":{"\u0024ref":"` + missing + `"}}}}`
	if strings.Contains(escaped, "$") {
		t.Fatal("leg two must carry no literal $ (a raw-text grep would then find it)")
	}
	for name, reply := range map[string]string{
		"file ref":    `{"type":"object","title":"Book","properties":{"x":{"$ref":"file://` + filepath.ToSlash(target) + `"}}}`,
		"escaped ref": escaped,
	} {
		fx := &fakePrompterExtractor{t: t, promptScript: []string{reply, validDraft}}
		out, err := draft(draftDeps(t, fx), "book title", "")
		if err != nil {
			t.Fatalf("%s: DraftSchema: %v", name, err)
		}
		if out.Attempts != 2 || len(fx.users) != 2 {
			t.Errorf("%s: attempts = %d, want 2 (the ref reply repaired, never compiled)", name, out.Attempts)
			continue
		}
		if !strings.Contains(fx.users[1], `"$ref" is not allowed`) {
			t.Errorf("%s: repair prompt misses the guard error:\n%s", name, fx.users[1])
		}
		if strings.Contains(out.Schema, "$ref") {
			t.Errorf("%s: returned schema keeps a $ref:\n%s", name, out.Schema)
		}
	}
}
