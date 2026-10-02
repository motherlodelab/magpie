package store_test

import (
	"testing"
	"time"

	"github.com/motherlodelab/magpie/store"
)

// seedRun begins (and, when status != "running", finishes) one run, then
// backdates its started_at — the RFC3339 UTC format BeginRun writes.
func seedRun(t *testing.T, db *store.DB, id, cmd, status string, ok, errs int, startedAt string) {
	t.Helper()
	if err := db.BeginRun(id, cmd); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		if err := db.FinishRun(id, ok, errs, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ExecRaw("UPDATE run_history SET started_at='" + startedAt + "' WHERE run_id='" + id + "'"); err != nil {
		t.Fatal(err)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestActivity_BucketsByOffsetDay(t *testing.T) {
	db := openTempDB(t)
	seedRun(t, db, "r1", "scrape", "finished", 1, 0, "2026-10-01T20:30:00Z")
	since := mustTime(t, "2026-09-01T00:00:00Z")
	for _, c := range []struct {
		off  time.Duration
		want string
	}{{7 * time.Hour, "2026-10-02"}, {0, "2026-10-01"}, {-5 * time.Hour, "2026-10-01"}} {
		rows, err := db.Activity(since, c.off)
		if err != nil {
			t.Fatalf("Activity(%v): %v", c.off, err)
		}
		if len(rows) != 1 || rows[0].Day != c.want {
			t.Errorf("offset %v: rows = %+v, want one on %s", c.off, rows, c.want)
		}
	}
}

func TestActivity_GroupsCommandStatus(t *testing.T) {
	db := openTempDB(t)
	const day = "2026-10-01T12:00:00Z"
	seedRun(t, db, "s1", "scrape", "finished", 2, 0, day)
	seedRun(t, db, "s2", "scrape", "finished", 1, 1, day)
	seedRun(t, db, "s3", "scrape", "error", 0, 1, day)
	seedRun(t, db, "c1", "crawl", "finished", 5, 0, day)
	if err := db.LogLLMCall("s1", store.LLMCall{Provider: "openai", Model: "m", PromptTokens: 10, CompletionTokens: 5, USDEstimate: 0.01, Purpose: "extract"}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Activity(mustTime(t, "2026-10-01T00:00:00Z"), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ActivityRow{
		{Day: "2026-10-01", Command: "crawl", Status: "finished", Runs: 1, PagesOK: 5},
		{Day: "2026-10-01", Command: "scrape", Status: "error", Runs: 1, PagesErr: 1},
		{Day: "2026-10-01", Command: "scrape", Status: "finished", Runs: 2, PagesOK: 3, PagesErr: 1, PromptTokens: 10, CompletionTokens: 5, USDEstimate: 0.01},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestActivity_SinceExcludesOlder(t *testing.T) {
	db := openTempDB(t)
	since := mustTime(t, "2026-10-01T00:00:00Z")
	seedRun(t, db, "at", "scrape", "finished", 1, 0, "2026-10-01T00:00:00Z")
	seedRun(t, db, "before", "crawl", "finished", 1, 0, "2026-09-30T23:59:59Z")
	rows, err := db.Activity(since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Command != "scrape" {
		t.Errorf("rows = %+v, want only the run at since", rows)
	}
}

func TestActivity_Empty(t *testing.T) {
	rows, err := openTempDB(t).Activity(time.Now().Add(-24*time.Hour), 0)
	if err != nil || len(rows) != 0 {
		t.Errorf("Activity on empty store = %+v, %v; want 0 rows, nil", rows, err)
	}
}

func TestActivity_SkipsMalformedStartedAt(t *testing.T) {
	db := openTempDB(t)
	seedRun(t, db, "bad", "scrape", "finished", 1, 0, "garbage") // 'g' > '2': passes the since filter
	seedRun(t, db, "good", "scrape", "finished", 1, 0, "2026-10-01T12:00:00Z")
	rows, err := db.Activity(mustTime(t, "2026-09-01T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("malformed row must be skipped, not fatal: %v", err)
	}
	if len(rows) != 1 || rows[0].Day != "2026-10-01" {
		t.Errorf("rows = %+v, want only the well-formed run", rows)
	}
}

func TestSpendByProvider(t *testing.T) {
	db := openTempDB(t)
	since := mustTime(t, "2026-10-01T00:00:00Z")
	seedRun(t, db, "r", "extract", "finished", 1, 0, "2026-10-02T00:00:00Z")
	seedRun(t, db, "old", "extract", "finished", 1, 0, "2026-09-30T00:00:00Z")
	log := func(run, provider string, usd float64, pt, ct int) {
		t.Helper()
		if err := db.LogLLMCall(run, store.LLMCall{Provider: provider, Model: "m", PromptTokens: pt, CompletionTokens: ct, USDEstimate: usd, Purpose: "extract"}); err != nil {
			t.Fatal(err)
		}
	}
	log("r", "openai", 0.10, 100, 10)
	log("r", "openai", 0.20, 200, 20)
	log("r", "anthropic", 0.50, 50, 5)
	log("r", "ollama", 0, 7, 3)
	log("old", "openai", 9, 1, 1)
	if err := db.ExecRaw("UPDATE llm_calls SET ts='2026-10-02T00:00:00Z' WHERE run_id='r'"); err != nil {
		t.Fatal(err)
	}
	if err := db.ExecRaw("UPDATE llm_calls SET ts='2026-09-30T23:59:59Z' WHERE run_id='old'"); err != nil {
		t.Fatal(err)
	}
	ps, err := db.SpendByProvider(since)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ProviderSpend{
		{Provider: "anthropic", Calls: 1, PromptTokens: 50, CompletionTokens: 5, USDEstimate: 0.50},
		{Provider: "openai", Calls: 2, PromptTokens: 300, CompletionTokens: 30, USDEstimate: 0.30},
		{Provider: "ollama", Calls: 1, PromptTokens: 7, CompletionTokens: 3},
	}
	if len(ps) != len(want) {
		t.Fatalf("SpendByProvider = %+v, want %d rows", ps, len(want))
	}
	for i := range want {
		got := ps[i]
		if got.Provider != want[i].Provider || got.Calls != want[i].Calls || got.PromptTokens != want[i].PromptTokens ||
			got.CompletionTokens != want[i].CompletionTokens || !near(got.USDEstimate, want[i].USDEstimate) {
			t.Errorf("row %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestSpendByProvider_TieByName(t *testing.T) {
	db := openTempDB(t)
	seedRun(t, db, "r", "extract", "finished", 1, 0, "2026-10-02T00:00:00Z")
	for _, p := range []string{"openai", "anthropic"} {
		if err := db.LogLLMCall("r", store.LLMCall{Provider: p, Model: "m", USDEstimate: 0.25, Purpose: "extract"}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := db.SpendByProvider(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Provider != "anthropic" || ps[1].Provider != "openai" {
		t.Errorf("tie order = %+v, want anthropic then openai", ps)
	}
}

func TestOpenTwiceWithIndex(t *testing.T) {
	path := openTempDB(t).Path()
	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("second open (idx_llm_calls_ts must be idempotent): %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
