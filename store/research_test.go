package store_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motherlodelab/magpie/store"
)

// beginResearch wires a research run the way DR2 will: the run_history
// row first (research_runs FKs it, and it carries History + spend), then
// the launch row.
func beginResearch(t *testing.T, db *store.DB, id string) {
	t.Helper()
	if err := db.BeginRun(id, "research"); err != nil {
		t.Fatalf("BeginRun(%q): %v", id, err)
	}
	if err := db.PutResearchRun(store.ResearchRun{RunID: id, Question: "q", Plan: "{}", Options: "{}"}); err != nil {
		t.Fatalf("PutResearchRun(%q): %v", id, err)
	}
}

// pinSnap stores one snapshot and returns the pin a fact cites.
func pinSnap(t *testing.T, db *store.DB, url, hash, md string) time.Time {
	t.Helper()
	at, err := db.PutSnapshot(url, hash, md, false)
	if err != nil {
		t.Fatalf("PutSnapshot(%q): %v", url, err)
	}
	return at
}

func getResearch(t *testing.T, db *store.DB, id string) store.ResearchRun {
	t.Helper()
	r, err := db.GetResearchRun(id)
	if err != nil {
		t.Fatalf("GetResearchRun(%q): %v", id, err)
	}
	return r
}

func countRows(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	n, err := db.TableCount(table)
	if err != nil {
		t.Fatalf("TableCount(%s): %v", table, err)
	}
	return n
}

func TestResearchRun_RoundTrip(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	if err := db.BeginRun("rt", "research"); err != nil {
		t.Fatal(err)
	}
	in := store.ResearchRun{
		RunID: "rt", Question: "Who makes the best widgets?",
		Plan: `{"subs":["price","reviews"]}`, Options: `{"depth":2}`,
		// store-owned fields: ignored on input
		Status: "done", Steer: "ignored", Report: "ignored", FinishedAt: time.Now(),
	}
	if err := db.PutResearchRun(in); err != nil {
		t.Fatalf("PutResearchRun: %v", err)
	}
	got := getResearch(t, db, "rt")
	if got.RunID != in.RunID || got.Question != in.Question || got.Plan != in.Plan || got.Options != in.Options {
		t.Errorf("launch fields = %+v, want %+v", got, in)
	}
	if got.Status != "running" || got.Steer != "" || got.Report != "" {
		t.Errorf("status/steer/report = %q/%q/%q, want running/\"\"/\"\"", got.Status, got.Steer, got.Report)
	}
	if got.StartedAt.IsZero() || !got.FinishedAt.IsZero() {
		t.Errorf("StartedAt = %v, FinishedAt = %v; want set, zero", got.StartedAt, got.FinishedAt)
	}
}

func TestResearchRun_PutValidation(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "dup")
	if err := db.BeginRun("v", "research"); err != nil { // so only validation can fail the "v" rows
		t.Fatal(err)
	}
	ok := store.ResearchRun{RunID: "v", Question: "q", Plan: "{}", Options: "{}"}
	with := func(f func(*store.ResearchRun)) store.ResearchRun { r := ok; f(&r); return r }
	for _, tc := range []struct {
		name string
		r    store.ResearchRun
		want string
	}{
		{"empty id", with(func(r *store.ResearchRun) { r.RunID = "" }), "empty run id"},
		{"empty question", with(func(r *store.ResearchRun) { r.Question = "" }), "question"},
		{"bad plan", with(func(r *store.ResearchRun) { r.Plan = "{" }), "plan"},
		{"bad options", with(func(r *store.ResearchRun) { r.Options = "nope" }), "options"},
		{"no run_history row", with(func(r *store.ResearchRun) { r.RunID = "orphan" }), `"orphan"`},
		{"duplicate", with(func(r *store.ResearchRun) { r.RunID = "dup" }), `"dup"`},
	} {
		err := db.PutResearchRun(tc.r)
		if err == nil {
			t.Errorf("%s: PutResearchRun = nil, want error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %s", tc.name, err, tc.want)
		}
	}
	if n := countRows(t, db, "research_runs"); n != 1 {
		t.Errorf("research_runs rows = %d, want 1 (only the dup seed)", n)
	}
}

func TestResearchRun_Lifecycle(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)

	// running → writing (+steer) → done (+report); done is terminal.
	beginResearch(t, db, "life")
	if err := db.SetResearchState("life", "writing", "focus on 2026"); err != nil {
		t.Fatalf("SetResearchState(writing): %v", err)
	}
	if got := getResearch(t, db, "life"); got.Status != "writing" || got.Steer != "focus on 2026" {
		t.Errorf("after writing = %q/%q, want writing/focus on 2026", got.Status, got.Steer)
	}
	if err := db.FinishResearchRun("life", "done", "# Report"); err != nil {
		t.Fatalf("FinishResearchRun(done): %v", err)
	}
	done := getResearch(t, db, "life")
	if done.Status != "done" || done.Report != "# Report" || done.FinishedAt.IsZero() || done.Steer != "focus on 2026" {
		t.Errorf("done = %+v, want done + report + FinishedAt + steer kept", done)
	}
	if err := db.SetResearchState("life", "running", "more"); err == nil || !strings.Contains(err.Error(), `"life"`) {
		t.Errorf("SetResearchState on done = %v, want error naming the run", err)
	}
	if err := db.FinishResearchRun("life", "failed", ""); err == nil || !strings.Contains(err.Error(), `"life"`) {
		t.Errorf("FinishResearchRun on done = %v, want error naming the run", err)
	}
	if got := getResearch(t, db, "life"); got.Status != "done" || got.Report != "# Report" {
		t.Errorf("done run changed by refused writes: %+v", got)
	}

	// failed → resume (running, steer stored, FinishedAt cleared) → done.
	beginResearch(t, db, "resume")
	if err := db.FinishResearchRun("resume", "failed", ""); err != nil {
		t.Fatalf("FinishResearchRun(failed): %v", err)
	}
	if got := getResearch(t, db, "resume"); got.Status != "failed" || got.FinishedAt.IsZero() || got.Report != "" {
		t.Errorf("failed = %+v, want failed + FinishedAt + no report", got)
	}
	if err := db.SetResearchState("resume", "running", "go deeper"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := getResearch(t, db, "resume"); got.Status != "running" || !got.FinishedAt.IsZero() || got.Steer != "go deeper" {
		t.Errorf("resumed = %+v, want running + FinishedAt zero + steer", got)
	}
	if err := db.FinishResearchRun("resume", "done", "r"); err != nil {
		t.Fatalf("FinishResearchRun after resume: %v", err)
	}
}

func TestResearchRun_FinishValidation(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "fv")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"done without report", db.FinishResearchRun("fv", "done", "")},
		{"failed with report", db.FinishResearchRun("fv", "failed", "r")},
		{"finish as running", db.FinishResearchRun("fv", "running", "")},
		{"set state done", db.SetResearchState("fv", "done", "")},
		{"unknown run finish", db.FinishResearchRun("nope", "failed", "")},
		{"unknown run set state", db.SetResearchState("nope", "running", "")},
	} {
		if tc.err == nil {
			t.Errorf("%s: nil, want error", tc.name)
		}
	}
	if got := getResearch(t, db, "fv"); got.Status != "running" || !got.FinishedAt.IsZero() || got.Report != "" {
		t.Errorf("fv changed by refused writes: %+v", got)
	}
}

func TestResearchRun_GetUnknown(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	_, err := db.GetResearchRun("research-nope-xyz")
	if err == nil || !strings.Contains(err.Error(), "research-nope-xyz") {
		t.Errorf("GetResearchRun(unknown) = %v, want error naming the id", err)
	}
}

// facts builds n valid facts pinned to (url, at), claims numbered from 1.
func facts(url string, at time.Time, n int) []store.Fact {
	out := make([]store.Fact, n)
	for i := range out {
		out[i] = store.Fact{Claim: fmt.Sprintf("claim %d", i+1), Quote: "widget", URL: url, CheckedAt: at, Confidence: 0.5}
	}
	return out
}

func TestFacts_MintsSequentialIDs(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "mint")
	const url = "https://example.com/source"
	at := pinSnap(t, db, url, "hash-src", "# Source\n\nwidget prices rose")

	ids, err := db.PutFacts("mint", facts(url, at, 3))
	if err != nil {
		t.Fatalf("PutFacts(3): %v", err)
	}
	if got := strings.Join(ids, ","); got != "f1,f2,f3" {
		t.Errorf("first batch ids = %s, want f1,f2,f3", got)
	}
	if ids, err = db.PutFacts("mint", facts(url, at, 9)); err != nil {
		t.Fatalf("PutFacts(9): %v", err)
	}
	if got := strings.Join(ids, ","); got != "f4,f5,f6,f7,f8,f9,f10,f11,f12" {
		t.Errorf("second batch ids = %s, want f4…f12", got)
	}

	fs, err := db.Facts("mint")
	if err != nil {
		t.Fatalf("Facts: %v", err)
	}
	if len(fs) != 12 {
		t.Fatalf("Facts = %d rows, want 12", len(fs))
	}
	for i, f := range fs { // numeric order: f10 after f9, not after f1
		if want := fmt.Sprintf("f%d", i+1); f.FactID != want {
			t.Errorf("Facts[%d].FactID = %q, want %q", i, f.FactID, want)
		}
		if f.URL != url || f.ContentHash != "hash-src" || !f.CheckedAt.Equal(at) {
			t.Errorf("Facts[%d] pin = %q/%q/%v, want joined from the snapshot", i, f.URL, f.ContentHash, f.CheckedAt)
		}
		if f.Status != "unverified" || f.Note != "" {
			t.Errorf("Facts[%d] status/note = %q/%q, want unverified/\"\"", i, f.Status, f.Note)
		}
	}
	if fs[3].Claim != "claim 1" || fs[9].Claim != "claim 7" { // ids follow input order: f4 opens the second batch
		t.Errorf("f4/f10 claims = %q/%q, want claim 1/claim 7 of the second batch", fs[3].Claim, fs[9].Claim)
	}

	none, err := db.Facts("no-such-run")
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("Facts(unknown) = %#v, %v; want empty non-nil, nil", none, err)
	}
}

func TestFacts_PinRequired(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "pin")
	const url = "https://example.com/pinned"
	at := pinSnap(t, db, url, "h", "# pinned widget")
	good := facts(url, at, 1)[0]
	unpinned := good
	unpinned.CheckedAt = at.Add(time.Nanosecond)
	unstored := good
	unstored.URL = "https://never-fetched.example/"

	for _, tc := range []struct {
		name  string
		run   string
		batch []store.Fact
		want  string
	}{
		{"unpinned checked_at", "pin", []store.Fact{good, unpinned}, "fact 1 (" + url + ")"},
		{"unstored url", "pin", []store.Fact{good, unstored}, "fact 1 (https://never-fetched.example/)"},
		{"unknown run", "no-such-run", []store.Fact{good}, "fact 0"},
	} {
		ids, err := db.PutFacts(tc.run, tc.batch)
		if err == nil {
			t.Errorf("%s: PutFacts = %v, nil; want error", tc.name, ids)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, tc.want)
		}
	}
	if n := countRows(t, db, "facts"); n != 0 {
		t.Errorf("facts rows = %d, want 0 (a bad batch stores nothing)", n)
	}
}

func TestFacts_Validation(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "val")
	const url = "https://example.com/val"
	at := pinSnap(t, db, url, "h", "# widget")
	good := facts(url, at, 1)[0]
	with := func(f func(*store.Fact)) store.Fact { x := good; f(&x); return x }
	for _, tc := range []struct {
		name string
		bad  store.Fact
	}{
		{"empty claim", with(func(f *store.Fact) { f.Claim = "" })},
		{"empty quote", with(func(f *store.Fact) { f.Quote = "" })},
		{"empty url", with(func(f *store.Fact) { f.URL = "" })},
		{"zero CheckedAt", with(func(f *store.Fact) { f.CheckedAt = time.Time{} })},
		{"confidence -0.1", with(func(f *store.Fact) { f.Confidence = -0.1 })},
		{"confidence 1.1", with(func(f *store.Fact) { f.Confidence = 1.1 })},
		{"confidence NaN", with(func(f *store.Fact) { f.Confidence = math.NaN() })},
	} {
		if _, err := db.PutFacts("val", []store.Fact{good, tc.bad}); err == nil || !strings.Contains(err.Error(), "fact 1") {
			t.Errorf("%s: PutFacts = %v, want error naming fact 1", tc.name, err)
		}
	}
	if n := countRows(t, db, "facts"); n != 0 {
		t.Errorf("facts rows = %d, want 0 (validation runs before any write)", n)
	}
	for _, empty := range [][]store.Fact{nil, {}} {
		if ids, err := db.PutFacts("val", empty); ids != nil || err != nil {
			t.Errorf("PutFacts(empty) = %v, %v; want nil, nil", ids, err)
		}
	}
	// The range is inclusive.
	if _, err := db.PutFacts("val", []store.Fact{with(func(f *store.Fact) { f.Confidence = 0 }), with(func(f *store.Fact) { f.Confidence = 1 })}); err != nil {
		t.Errorf("PutFacts(confidence 0 and 1) = %v, want nil", err)
	}
}

// TestFacts_PinRoundTrip walks the chain DR2's verifier walks: fact → its
// pin → the exact stored markdown, which contains the quote — even after
// a newer version of the page lands.
func TestFacts_PinRoundTrip(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "chain")
	const url = "https://example.com/chain"
	at := pinSnap(t, db, url, "h-old", "# Prices\n\nThe widget costs $12 as of March 2026.")
	if _, err := db.PutFacts("chain", []store.Fact{{
		Claim: "Widgets cost $12", Quote: "The widget costs $12", URL: url, CheckedAt: at,
		Published: "2026-03-01", Confidence: 0.8,
	}}); err != nil {
		t.Fatalf("PutFacts: %v", err)
	}
	if _, err := db.PutSnapshot(url, "h-new", "# Prices\n\nThe widget costs $15.", true); err != nil {
		t.Fatal(err)
	}
	fs, err := db.Facts("chain")
	if err != nil || len(fs) != 1 {
		t.Fatalf("Facts = %d rows, %v; want 1", len(fs), err)
	}
	f := fs[0]
	if f.Published != "2026-03-01" || f.Confidence != 0.8 || f.ContentHash != "h-old" {
		t.Errorf("fact = %+v, want published/confidence kept and the old version's hash", f)
	}
	snap, ok, err := db.SnapshotAt(f.URL, f.CheckedAt)
	if err != nil || !ok {
		t.Fatalf("SnapshotAt(fact pin) = (%v, %v), want found", ok, err)
	}
	if !strings.Contains(snap.Markdown, f.Quote) || snap.ContentHash != f.ContentHash {
		t.Errorf("pinned snapshot %q (hash %s) does not back quote %q (hash %s)", snap.Markdown, snap.ContentHash, f.Quote, f.ContentHash)
	}
}

func TestSetFactStatus(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	beginResearch(t, db, "st")
	const url = "https://example.com/st"
	if _, err := db.PutFacts("st", facts(url, pinSnap(t, db, url, "h", "# widget"), 2)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFactStatus("st", "f1", "verified", ""); err != nil {
		t.Errorf("verified, no note: %v", err)
	}
	if err := db.SetFactStatus("st", "f2", "softened", ""); err == nil {
		t.Error("softened without a note = nil, want error")
	}
	if err := db.SetFactStatus("st", "f2", "softened", "claim 2, roughly"); err != nil {
		t.Errorf("softened with note: %v", err)
	}
	for _, tc := range []struct{ name, run, fact, status, want string }{
		{"unverified is insert-only", "st", "f1", "unverified", "unverified"},
		{"bogus status", "st", "f1", "bogus", "bogus"},
		{"unknown fact", "st", "f9", "dropped", `"f9"`},
		{"unknown run", "nope", "f1", "verified", `"nope"`},
	} {
		if err := db.SetFactStatus(tc.run, tc.fact, tc.status, "why"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want error mentioning %s", tc.name, err, tc.want)
		}
	}
	fs, err := db.Facts("st")
	if err != nil || len(fs) != 2 {
		t.Fatalf("Facts = %d rows, %v", len(fs), err)
	}
	if fs[0].Status != "verified" || fs[0].Note != "" {
		t.Errorf("f1 = %q/%q, want verified/\"\"", fs[0].Status, fs[0].Note)
	}
	if fs[1].Status != "softened" || fs[1].Note != "claim 2, roughly" || fs[1].Claim != "claim 2" {
		t.Errorf("f2 = %q/%q claim %q, want softened + note, claim never rewritten", fs[1].Status, fs[1].Note, fs[1].Claim)
	}
}

// TestFacts_TwoHandlesConcurrent stands in for desktop + CLI on one cache.db:
// two store.Open handles (two real connections through WAL), each minting 20
// single-fact batches on its own run. The in-statement INSERT…SELECT…RETURNING
// minting must yield exactly f1…f20 per run regardless of interleaving — a
// SELECT-MAX-then-INSERT would collide or gap under the same schedule.
func TestFacts_TwoHandlesConcurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "conc.db")
	dbs := make([]*store.DB, 2)
	for i := range dbs {
		db, err := store.Open(path)
		if err != nil {
			t.Fatalf("Open handle %d: %v", i, err)
		}
		dbs[i] = db
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}

	// one run per handle, both wired through BeginRun (the facts FK needs it)
	runs := []string{"conc-a", "conc-b"}
	for i, id := range runs {
		beginResearch(t, dbs[i], id)
	}

	var wg sync.WaitGroup
	for i, id := range runs {
		wg.Add(1)
		go func(db *store.DB, runID string) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				at, err := db.PutSnapshot("https://example.com/"+runID, "hash", "# page", false)
				if err != nil {
					t.Errorf("PutSnapshot: %v", err)
					return
				}
				ids, err := db.PutFacts(runID, []store.Fact{{
					Claim:      fmt.Sprintf("claim %d", n),
					Quote:      "page",
					URL:        "https://example.com/" + runID,
					CheckedAt:  at,
					Confidence: 0.5,
				}})
				if err != nil {
					t.Errorf("PutFacts %d: %v", n, err)
					return
				}
				if len(ids) != 1 {
					t.Errorf("PutFacts %d returned %d ids, want 1", n, len(ids))
					return
				}
			}
		}(dbs[i], id)
	}
	wg.Wait()

	for _, runID := range runs {
		fs, err := dbs[0].Facts(runID)
		if err != nil {
			t.Fatalf("Facts(%q): %v", runID, err)
		}
		if len(fs) != 20 {
			t.Fatalf("Facts(%q) = %d rows, want 20", runID, len(fs))
		}
		for n, f := range fs {
			if want := fmt.Sprintf("f%d", n+1); f.FactID != want {
				t.Fatalf("Facts(%q)[%d].FactID = %q, want %q", runID, n, f.FactID, want)
			}
		}
	}
}

const preDR0URL = "https://example.com/pre-dr0"

// openPreDR0 hand-builds what v0.1.23 leaves on disk — today's run_history
// (fetch + error columns) and snapshots, no research tables — in raw SQL,
// seeds one run and one snapshot, and hands back the path. The literal DDL
// copy fails loudly if the real DDL drifts: that failure is the
// migration-coverage alarm, not a bug.
func openPreDR0(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "predr0.db")
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
    fetch_pages       INTEGER NOT NULL DEFAULT 0,
    fetch_bytes       INTEGER NOT NULL DEFAULT 0,
    fetch_ms          INTEGER NOT NULL DEFAULT 0,
    proxy             TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'running',
    error_kind        TEXT NOT NULL DEFAULT '',
    error_msg         TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS snapshots (
    url_hash     TEXT NOT NULL,
    url          TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    markdown     TEXT NOT NULL,
    checked_at   TEXT NOT NULL,
    changed      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (url_hash, checked_at)
);`
	if _, err := db.Exec(preDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO run_history(run_id, command, started_at, status) VALUES('pre-dr0-run', 'research', '2026-10-01T00:00:00Z', 'running')`); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(preDR0URL))
	if _, err := db.Exec(`INSERT INTO snapshots(url_hash, url, content_hash, markdown, checked_at, changed) VALUES(?, ?, 'h-pre', '# Pre-DR0 page

old but pinned', '2026-10-01T00:00:00.123456789Z', 0)`, hex.EncodeToString(sum[:]), preDR0URL); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResearch_OpensPreDR0File(t *testing.T) {
	t.Parallel()
	path := openPreDR0(t)
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(pre-DR0 file): %v", err)
	}
	for table, want := range map[string]int{"research_runs": 0, "facts": 0, "snapshots": 1, "run_history": 1} {
		if n := countRows(t, db, table); n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}
	snap, ok, err := db.LatestSnapshot(preDR0URL)
	if err != nil || !ok || snap.ContentHash != "h-pre" {
		t.Fatalf("seeded snapshot = %+v, %v, %v; want h-pre", snap, ok, err)
	}
	if err := db.PutResearchRun(store.ResearchRun{RunID: "pre-dr0-run", Question: "q", Plan: "{}", Options: "{}"}); err != nil {
		t.Fatalf("PutResearchRun on the seeded run: %v", err)
	}
	if _, err := db.PutFacts("pre-dr0-run", []store.Fact{{Claim: "c", Quote: "old but pinned", URL: preDR0URL, CheckedAt: snap.CheckedAt}}); err != nil {
		t.Fatalf("PutFacts pinning the seeded snapshot: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	fs, err := db.Facts("pre-dr0-run")
	if err != nil || len(fs) != 1 || fs[0].ContentHash != "h-pre" {
		t.Errorf("Facts after reopen = %+v, %v; want 1 fact pinned to h-pre", fs, err)
	}
	if r, err := db.GetRun("pre-dr0-run"); err != nil || r.Command != "research" {
		t.Errorf("seeded run_history row = %+v, %v; want intact", r, err)
	}
}
