package store_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/store"
)

func openTempDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

func TestTablesExist(t *testing.T) {
	db := openTempDB(t)
	for _, tbl := range []string{"selector_cache", "crawl_state", "dedup", "run_history", "llm_calls"} {
		n, err := db.TableCount(tbl)
		if err != nil {
			t.Errorf("table %s: %v", tbl, err)
		}
		if n != 0 {
			t.Errorf("table %s fresh count = %d, want 0", tbl, n)
		}
	}
}

func TestForeignKeysOn(t *testing.T) {
	db := openTempDB(t)
	v, err := db.Pragma("foreign_keys")
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", v)
	}
}

func TestRunRoundTrip(t *testing.T) {
	db := openTempDB(t)
	if err := db.BeginRun("r1", "scrape"); err != nil {
		t.Fatal(err)
	}
	if err := db.LogLLMCall("r1", store.LLMCall{Provider: "openai", Model: "gpt-4o-mini", PromptTokens: 10, CompletionTokens: 5, USDEstimate: 0.01, Purpose: "extract"}); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun("r1", 1, 0, "finished"); err != nil {
		t.Fatal(err)
	}
	n, err := db.LLMCallCount("r1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("llm_calls = %d, want 1", n)
	}
	c, err := db.RunCost("r1")
	if err != nil {
		t.Fatal(err)
	}
	if c != 0.01 {
		t.Errorf("cost = %v, want 0.01", c)
	}
	// Idempotent re-open.
	db2, err := store.Open(db.Path())
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSelectorRoundTrip(t *testing.T) {
	db := openTempDB(t)
	doc := `{"fields":{"price":{"type":"css","expr":"#price"}}}`
	if err := db.PutSelectors("ex.com", "h1", doc, 3); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.GetSelectors("ex.com", "h1")
	if err != nil || !ok {
		t.Fatalf("Get = %q,%v,%v", got, ok, err)
	}
	if got != doc {
		t.Errorf("Get = %q, want %q", got, doc)
	}
	// Update path.
	if err := db.PutSelectors("ex.com", "h1", doc, 5); err != nil {
		t.Fatal(err)
	}
	// Scoped delete.
	if n, err := db.DeleteSelectors("ex.com", "h1"); err != nil || n != 1 {
		t.Fatalf("Delete = %d,%v want 1", n, err)
	}
	if _, ok, gerr := db.GetSelectors("ex.com", "h1"); gerr != nil || ok {
		t.Errorf("Get after delete = %v,%v, want miss", ok, gerr)
	}
	// Domain-wide clear leaves other domains alone.
	if err := db.PutSelectors("ex.com", "h1", doc, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSelectors("ex.com", "h2", doc, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSelectors("other.com", "h1", doc, 3); err != nil {
		t.Fatal(err)
	}
	if n, err := db.DeleteSelectors("ex.com", ""); err != nil || n != 2 {
		t.Fatalf("Delete domain = %d,%v want 2", n, err)
	}
	if _, ok, gerr := db.GetSelectors("other.com", "h1"); gerr != nil || !ok {
		t.Errorf("other domain Get = %v,%v, want untouched hit", ok, gerr)
	}
}

// TestDeleteSelectorsAll pins the no-flag clear (magpie cache clear with
// neither --domain nor --schema-hash): both args empty must evict every
// row. Regression: the pre-v0.1.4 branch ran WHERE domain=” and matched
// nothing, so the CLI's clear-all was a silent no-op.
func TestDeleteSelectorsAll(t *testing.T) {
	db := openTempDB(t)
	doc := `{"fields":{"price":{"type":"css","expr":"#price"}}}`
	for _, d := range []string{"ex.com", "other.com"} {
		if err := db.PutSelectors(d, "h1", doc, 3); err != nil {
			t.Fatal(err)
		}
	}
	n, err := db.DeleteSelectors("", "")
	if err != nil || n != 2 {
		t.Fatalf("DeleteSelectors all = %d,%v, want 2", n, err)
	}
	if _, ok, err := db.GetSelectors("ex.com", "h1"); err != nil || ok {
		t.Errorf("ex.com after clear = %v,%v, want miss", ok, err)
	}
	if _, ok, err := db.GetSelectors("other.com", "h1"); err != nil || ok {
		t.Errorf("other.com after clear = %v,%v, want miss", ok, err)
	}
	// Clearing an empty table is 0, not an error.
	if n, err := db.DeleteSelectors("", ""); err != nil || n != 0 {
		t.Errorf("re-clear = %d,%v, want 0", n, err)
	}
}

func TestFrontierVector(t *testing.T) {
	db := openTempDB(t)
	urls := []string{"http://ex.com/1", "http://ex.com/2", "http://ex.com/3", "http://ex.com/4", "http://ex.com/5"}
	if n, err := db.Enqueue("r1", urls, 0); err != nil || n != 5 {
		t.Fatalf("Enqueue = %d,%v want 5", n, err)
	}
	if n, err := db.Enqueue("r1", urls, 0); err != nil || n != 0 {
		t.Fatalf("re-Enqueue = %d,%v want 0", n, err)
	}
	claimed, err := db.Claim("r1", 2)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("Claim = %d,%v want 2", len(claimed), err)
	}
	for _, c := range claimed {
		if c.URL == "" || c.URLHash == "" {
			t.Errorf("claimed row missing fields: %+v", c)
		}
	}
	p, i, d, e, err := db.CrawlStats("r1")
	if err != nil || p != 3 || i != 2 || d != 0 || e != 0 {
		t.Fatalf("stats = %d/%d/%d/%d,%v want 3/2/0/0", p, i, d, e, err)
	}
	if n, err := db.ResetInflight("r1"); err != nil || n != 2 {
		t.Fatalf("ResetInflight = %d,%v want 2", n, err)
	}
	if p, i, d, e, serr := db.CrawlStats("r1"); serr != nil || p != 5 || i != 0 || d != 0 || e != 0 {
		t.Fatalf("stats after reset = %d/%d/%d/%d,%v want 5/0/0/0", p, i, d, e, serr)
	}
	claimed, err = db.Claim("r1", 2)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("Claim2 = %d,%v", len(claimed), err)
	}
	if err := db.MarkDone("r1", claimed[0].URLHash); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkError("r1", claimed[1].URLHash, "boom"); err != nil {
		t.Fatal(err)
	}
	if p, i, d, e, serr := db.CrawlStats("r1"); serr != nil || p != 3 || i != 0 || d != 1 || e != 1 {
		t.Fatalf("stats final = %d/%d/%d/%d,%v want 3/0/1/1", p, i, d, e, serr)
	}
}

func TestResumeRun(t *testing.T) {
	db := openTempDB(t)
	if err := db.ResumeRun("nope"); err == nil {
		t.Fatal("ResumeRun unknown id = nil, want error")
	}
	if err := db.BeginRun("r1", "crawl"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun("r1", 1, 0, "finished"); err != nil {
		t.Fatal(err)
	}
	if err := db.ResumeRun("r1"); err != nil {
		t.Fatalf("ResumeRun finished = %v", err)
	}
}

func TestSeenTwice(t *testing.T) {
	db := openTempDB(t)
	seen, err := db.Seen("r1", "abc")
	if err != nil || seen {
		t.Fatalf("Seen1 = %v,%v want false", seen, err)
	}
	seen, err = db.Seen("r1", "abc")
	if err != nil || !seen {
		t.Fatalf("Seen2 = %v,%v want true", seen, err)
	}
	hashes, err := db.LoadHashes("r1")
	if err != nil || len(hashes) != 1 || hashes[0] != "abc" {
		t.Fatalf("LoadHashes = %v,%v", hashes, err)
	}
}

func TestReopenPersists(t *testing.T) {
	db := openTempDB(t)
	path := db.Path()
	if err := db.PutSelectors("ex.com", "h1", `{"a":1}`, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Enqueue("r1", []string{"http://ex.com/1"}, 0); err != nil {
		t.Fatal(err)
	}
	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer func() {
		if cerr := db2.Close(); cerr != nil {
			t.Errorf("close: %v", cerr)
		}
	}()
	if _, ok, gerr := db2.GetSelectors("ex.com", "h1"); gerr != nil || !ok {
		t.Errorf("selectors lost across reopen: %v", gerr)
	}
	if p, _, _, _, serr := db2.CrawlStats("r1"); serr != nil || p != 1 {
		t.Errorf("pending after reopen = %d,%v want 1", p, serr)
	}
}

func TestGetRun(t *testing.T) {
	db := openTempDB(t)
	const id = "run-get-1"
	if err := db.BeginRun(id, "crawl"); err != nil {
		t.Fatalf("BeginRun: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := db.LogLLMCall(id, store.LLMCall{Provider: "fake", Model: "fake", PromptTokens: 10, CompletionTokens: 5, USDEstimate: 0.001, Purpose: "extract"}); err != nil {
			t.Fatalf("LogLLMCall: %v", err)
		}
	}
	if err := db.FinishRun(id, 3, 1, "complete"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, err := db.GetRun(id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.RunID != id || got.Command != "crawl" || got.Status != "complete" {
		t.Errorf("identity = %+v, want run-get-1/crawl/complete", got)
	}
	if got.PagesOK != 3 || got.PagesErr != 1 {
		t.Errorf("pages = %d/%d, want 3/1", got.PagesOK, got.PagesErr)
	}
	if got.PromptTokens != 20 || got.CompletionTokens != 10 {
		t.Errorf("tokens = %d/%d, want 20/10", got.PromptTokens, got.CompletionTokens)
	}
	if got.USDEstimate != 0.002 {
		t.Errorf("usd = %v, want 0.002", got.USDEstimate)
	}
}

func TestGetRun_Unknown(t *testing.T) {
	db := openTempDB(t)
	_, err := db.GetRun("run-nope-xyz")
	if err == nil {
		t.Fatal("GetRun(unknown) = nil, want loud error")
	}
	if !strings.Contains(err.Error(), "run-nope-xyz") {
		t.Errorf("error %q does not contain the run id", err)
	}
}

// --- Phase D additions: telemetry columns + pre-D migration. ---

// openPreDDB hand-builds the pre-Phase-D run_history (without the three
// fetch columns) in raw SQL, seeds one row, and hands back the path.
// The literal copy of the old DDL fails loudly if the real DDL drifts —
// that failure is the migration-coverage alarm, not a bug.
func openPreDDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pred.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close raw db: %v", err)
		}
	}()
	preDDL := `
CREATE TABLE IF NOT EXISTS run_history (
    run_id            TEXT PRIMARY KEY,
    command           TEXT NOT NULL,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    pages_ok          INTEGER NOT NULL DEFAULT 0,
    pages_err         INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    usd_estimate      REAL NOT NULL DEFAULT 0,
    status            TEXT NOT NULL DEFAULT 'running'
);`
	if _, err := db.Exec(preDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO run_history(run_id, command, started_at, status) VALUES('pre-d-run', 'crawl', '2026-01-01T00:00:00Z', 'finished')`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigration_AddsFetchColumns(t *testing.T) {
	path := openPreDDB(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(pre-D db): %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	// Old row readable; new columns report zeroed defaults.
	info, err := db.GetRun("pre-d-run")
	if err != nil {
		t.Fatalf("GetRun(pre-D row): %v", err)
	}
	if info.FetchPages != 0 || info.FetchBytes != 0 || info.FetchMs != 0 {
		t.Errorf("migrated fetch counters = %d/%d/%d, want 0/0/0", info.FetchPages, info.FetchBytes, info.FetchMs)
	}
	// And the migrated row accepts new telemetry.
	if err := db.LogFetch("pre-d-run", 100, 5); err != nil {
		t.Fatalf("LogFetch on migrated row: %v", err)
	}
	if info, err = db.GetRun("pre-d-run"); err != nil {
		t.Fatal(err)
	}
	if info.FetchPages != 1 || info.FetchBytes != 100 {
		t.Errorf("post-migration counters = %d/%d, want 1/100", info.FetchPages, info.FetchBytes)
	}
}

func TestLogFetch_RoundTrip(t *testing.T) {
	db := openTempDB(t)
	if err := db.BeginRun("r-fetch", "scrape"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.LogFetch("r-fetch", 1234, 56); err != nil {
			t.Fatal(err)
		}
	}
	info, err := db.GetRun("r-fetch")
	if err != nil {
		t.Fatal(err)
	}
	if info.FetchPages != 2 || info.FetchBytes != 2468 || info.FetchMs != 112 {
		t.Errorf("fetch counters = %d/%d/%d, want 2/2468/112", info.FetchPages, info.FetchBytes, info.FetchMs)
	}
	// FinishRun must not clobber the telemetry columns.
	if err := db.FinishRun("r-fetch", 1, 0, "finished"); err != nil {
		t.Fatal(err)
	}
	if info, err = db.GetRun("r-fetch"); err != nil {
		t.Fatal(err)
	}
	if info.FetchPages != 2 {
		t.Errorf("fetch pages after FinishRun = %d, want 2", info.FetchPages)
	}
	// Fresh run: zeroed by DEFAULT 0.
	if err := db.BeginRun("r-zero", "scrape"); err != nil {
		t.Fatal(err)
	}
	if info, err = db.GetRun("r-zero"); err != nil {
		t.Fatal(err)
	}
	if info.FetchPages != 0 || info.FetchBytes != 0 || info.FetchMs != 0 {
		t.Errorf("fresh counters = %d/%d/%d, want 0/0/0", info.FetchPages, info.FetchBytes, info.FetchMs)
	}
}

// TestMigration_AddsProxyColumn (Phase G G.1): the pre-G schema upgrades
// in place; the default is ” for non-pooled runs and SetRunProxy
// round-trips a redacted host:port.
func TestMigration_AddsProxyColumn(t *testing.T) {
	path := openPreDDB(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(pre-G db): %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	info, err := db.GetRun("pre-d-run")
	if err != nil {
		t.Fatal(err)
	}
	if info.Proxy != "" {
		t.Errorf("migrated row proxy = %q, want ''", info.Proxy)
	}
	if err := db.SetRunProxy("pre-d-run", "127.0.0.1:9050"); err != nil {
		t.Fatalf("SetRunProxy: %v", err)
	}
	if info, err = db.GetRun("pre-d-run"); err != nil {
		t.Fatal(err)
	}
	if info.Proxy != "127.0.0.1:9050" {
		t.Errorf("proxy = %q, want 127.0.0.1:9050", info.Proxy)
	}
	// Fresh runs default to '' (direct, never pooled).
	if err := db.BeginRun("g-run", "scrape"); err != nil {
		t.Fatal(err)
	}
	if info, err = db.GetRun("g-run"); err != nil {
		t.Fatal(err)
	}
	if info.Proxy != "" {
		t.Errorf("fresh run proxy = %q, want ''", info.Proxy)
	}
}

func TestListRuns(t *testing.T) {
	db := openTempDB(t)
	for _, id := range []string{"run-a", "run-b", "run-c"} {
		if err := db.BeginRun(id, "crawl"); err != nil {
			t.Fatalf("BeginRun %s: %v", id, err)
		}
		if err := db.FinishRun(id, 2, 1, "finished"); err != nil {
			t.Fatalf("FinishRun %s: %v", id, err)
		}
	}
	// started_at has second granularity; same-second runs tie on the
	// timestamp — insertion order (rowid) must break the tie newest-first.
	runs, err := db.ListRuns(0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(runs))
	}
	if runs[0].RunID != "run-c" || runs[1].RunID != "run-b" || runs[2].RunID != "run-a" {
		t.Errorf("order = [%s %s %s], want [run-c run-b run-a] (newest first)", runs[0].RunID, runs[1].RunID, runs[2].RunID)
	}
	for _, r := range runs {
		if r.Command != "crawl" || r.Status != "finished" || r.PagesOK != 2 || r.PagesErr != 1 {
			t.Errorf("row %s = %+v, want crawl/finished 2/1", r.RunID, r)
		}
		if r.StartedAt == "" {
			t.Errorf("row %s: empty started_at", r.RunID)
		}
	}
	limited, err := db.ListRuns(2)
	if err != nil {
		t.Fatalf("ListRuns(2): %v", err)
	}
	if len(limited) != 2 || limited[0].RunID != "run-c" || limited[1].RunID != "run-b" {
		t.Errorf("limited = %v, want [run-c run-b]", limited)
	}
}
