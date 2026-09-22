package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const ddl = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS selector_cache (
    schema_hash    TEXT NOT NULL,
    domain         TEXT NOT NULL,
    fields_json    TEXT NOT NULL,
    samples_used   INTEGER NOT NULL,
    engine_version INTEGER NOT NULL DEFAULT 1,
    synthesized_at TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (domain, schema_hash)
);

CREATE TABLE IF NOT EXISTS crawl_state (
    run_id        TEXT NOT NULL,
    url           TEXT NOT NULL,
    url_hash      TEXT NOT NULL,
    status        TEXT NOT NULL CHECK(status IN ('pending','inflight','done','error')),
    depth         INTEGER NOT NULL DEFAULT 0,
    error_msg     TEXT,
    discovered_at TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (run_id, url_hash)
);
CREATE INDEX IF NOT EXISTS idx_crawl_status ON crawl_state(run_id, status);

CREATE TABLE IF NOT EXISTS dedup (
    run_id     TEXT NOT NULL,
    url_hash   TEXT NOT NULL,
    first_seen TEXT NOT NULL,
    PRIMARY KEY (run_id, url_hash)
);

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
    status            TEXT NOT NULL DEFAULT 'running'
);

CREATE TABLE IF NOT EXISTS llm_calls (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id            TEXT NOT NULL,
    provider          TEXT NOT NULL,
    model             TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    usd_estimate      REAL NOT NULL,
    purpose           TEXT NOT NULL,
    ts                TEXT NOT NULL,
    FOREIGN KEY(run_id) REFERENCES run_history(run_id)
);

CREATE TABLE IF NOT EXISTS snapshots (
    url_hash     TEXT NOT NULL,
    url          TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    markdown     TEXT NOT NULL,
    checked_at   TEXT NOT NULL,
    changed      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (url_hash, checked_at)
);
`

// DB is a single-writer SQLite handle.
type DB struct {
	db   *sql.DB
	path string
}

// Open creates parent dirs, opens with WAL/foreign_keys pragmas in the DSN,
// and applies the full DDL idempotently.
func Open(path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty db path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ddl); err != nil {
		if cerr := db.Close(); cerr != nil {
			return nil, errors.Join(
				fmt.Errorf("store: migrate: %w", err),
				fmt.Errorf("store: close: %w", cerr),
			)
		}
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	wdb := &DB{db: db, path: path}
	if err := wdb.migrateRunHistory(); err != nil {
		_ = db.Close() //nolint:errcheck // error path; close failure would mask the real error
		return nil, err
	}
	return wdb, nil
}

// fetchColumns are the Phase D telemetry columns plus Phase G's proxy
// record; pre-D/G database files gain them (zeroed) on open via PRAGMA
// table_info + ADD COLUMN.
var fetchColumns = []struct{ name, def string }{
	{"fetch_pages", "INTEGER NOT NULL DEFAULT 0"},
	{"fetch_bytes", "INTEGER NOT NULL DEFAULT 0"},
	{"fetch_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"proxy", "TEXT NOT NULL DEFAULT ''"},
}

func (d *DB) migrateRunHistory() error {
	rows, err := d.db.Query(`PRAGMA table_info(run_history)`)
	if err != nil {
		return fmt.Errorf("store: migrate run_history: %w", err)
	}
	has := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close() //nolint:errcheck // read-only; close error unactionable
			return fmt.Errorf("store: migrate run_history: %w", err)
		}
		has[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close() //nolint:errcheck // read-only; close error unactionable
		return fmt.Errorf("store: migrate run_history: %w", err)
	}
	_ = rows.Close() //nolint:errcheck // read-only; close error unactionable
	for _, col := range fetchColumns {
		if has[col.name] {
			continue
		}
		if _, err := d.db.Exec(`ALTER TABLE run_history ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
			return fmt.Errorf("store: migrate run_history: add %s: %w", col.name, err)
		}
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NewRunID mints a run_history key: 8 random bytes + pid, so concurrent
// processes never collide and the id stays readable in logs. Falls back
// to timestamp+pid when the RNG fails (collision then needs a same-ns
// fork + broken RNG). Single home — cli, scrape, and mcp all mint run
// ids through this.
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%x-%d", b, os.Getpid())
	}
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
}

func (d *DB) Close() error { return d.db.Close() }

// Path returns the backing file path.
func (d *DB) Path() string { return d.path }

// BeginRun inserts a running run_history row.
func (d *DB) BeginRun(runID, command string) error {
	_, err := d.db.Exec(`INSERT INTO run_history(run_id, command, started_at, status) VALUES(?,?,?, 'running')`,
		runID, command, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store: begin run: %w", err)
	}
	return nil
}

// FinishRun marks a run finished with page counters.
func (d *DB) FinishRun(runID string, pagesOK, pagesErr int, status string) error {
	_, err := d.db.Exec(`UPDATE run_history SET finished_at=?, pages_ok=?, pages_err=?, status=? WHERE run_id=?`,
		time.Now().UTC().Format(time.RFC3339), pagesOK, pagesErr, status, runID)
	if err != nil {
		return fmt.Errorf("store: finish run: %w", err)
	}
	return nil
}

// LLMCall records one provider call and rolls tokens/cost into run_history.
type LLMCall struct {
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	USDEstimate      float64
	Purpose          string // synth|extract|repair
}

// LogLLMCall inserts into llm_calls and accumulates run_history totals atomically.
func (d *DB) LogLLMCall(runID string, c LLMCall) error {
	ts := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("store: log llm call: %w", err)
	}
	// ponytail: single deferred Rollback covers all error paths; harmless after Commit.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after Commit is a no-op by design
	if _, err := tx.Exec(`INSERT INTO llm_calls(run_id, provider, model, prompt_tokens, completion_tokens, usd_estimate, purpose, ts) VALUES(?,?,?,?,?,?,?,?)`,
		runID, c.Provider, c.Model, c.PromptTokens, c.CompletionTokens, c.USDEstimate, c.Purpose, ts); err != nil {
		return fmt.Errorf("store: log llm call: %w", err)
	}
	if _, err := tx.Exec(`UPDATE run_history SET prompt_tokens=prompt_tokens+?, completion_tokens=completion_tokens+?, usd_estimate=usd_estimate+? WHERE run_id=?`,
		c.PromptTokens, c.CompletionTokens, c.USDEstimate, runID); err != nil {
		return fmt.Errorf("store: roll up run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: log llm call: %w", err)
	}
	return nil
}

// RunInfo is the stored status row for a run (run_id polling).
type RunInfo struct {
	RunID            string
	Command          string
	StartedAt        string
	FinishedAt       string // "" while still running
	Status           string
	PagesOK          int
	PagesErr         int
	PromptTokens     int
	CompletionTokens int
	USDEstimate      float64
	FetchPages       int64
	FetchBytes       int64
	FetchMs          int64
	Proxy            string
}

// runCols is the run_history column list scanned into RunInfo — one home
// so GetRun and ListRuns cannot drift apart.
const runCols = "run_id, command, started_at, finished_at, status, pages_ok, pages_err, prompt_tokens, completion_tokens, usd_estimate, fetch_pages, fetch_bytes, fetch_ms, proxy"

// GetRun reads a run_history status row; unknown ids error loudly.
func (d *DB) GetRun(runID string) (RunInfo, error) {
	var r RunInfo
	var finished sql.NullString
	err := d.db.QueryRow(`SELECT `+runCols+` FROM run_history WHERE run_id=?`, runID).Scan(
		&r.RunID, &r.Command, &r.StartedAt, &finished, &r.Status, &r.PagesOK, &r.PagesErr, &r.PromptTokens, &r.CompletionTokens, &r.USDEstimate,
		&r.FetchPages, &r.FetchBytes, &r.FetchMs, &r.Proxy)
	if err == sql.ErrNoRows {
		return RunInfo{}, fmt.Errorf("store: unknown run_id %q", runID)
	}
	if err != nil {
		return RunInfo{}, fmt.Errorf("store: get run: %w", err)
	}
	r.FinishedAt = finished.String
	return r, nil
}

// ListRuns returns up to limit recent runs, newest first. started_at has
// second granularity (RFC3339), so rowid (insertion order) breaks ties —
// same-second runs still sort deterministically. Feeds the desktop
// History screen; limit <= 0 means all.
func (d *DB) ListRuns(limit int) ([]RunInfo, error) {
	q := `SELECT ` + runCols + ` FROM run_history ORDER BY started_at DESC, rowid DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := d.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close error unactionable
	var out []RunInfo
	for rows.Next() {
		var r RunInfo
		var finished sql.NullString
		if err := rows.Scan(&r.RunID, &r.Command, &r.StartedAt, &finished, &r.Status, &r.PagesOK, &r.PagesErr, &r.PromptTokens, &r.CompletionTokens, &r.USDEstimate,
			&r.FetchPages, &r.FetchBytes, &r.FetchMs, &r.Proxy); err != nil {
			return nil, fmt.Errorf("store: list runs: %w", err)
		}
		r.FinishedAt = finished.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// LogFetch accumulates one successful page fetch (bytes, milliseconds)
// into the run row, so cost and volume questions have one answer.
func (d *DB) LogFetch(runID string, nBytes, ms int64) error {
	_, err := d.db.Exec(`UPDATE run_history SET fetch_pages=fetch_pages+1, fetch_bytes=fetch_bytes+?, fetch_ms=fetch_ms+? WHERE run_id=?`,
		nBytes, ms, runID)
	if err != nil {
		return fmt.Errorf("store: log fetch: %w", err)
	}
	return nil
}

// SetRunProxy records the redacted host:port of the proxy entry that
// served the run's page (Phase G pool surfacing; empty = direct).
func (d *DB) SetRunProxy(runID, proxy string) error {
	_, err := d.db.Exec(`UPDATE run_history SET proxy=? WHERE run_id=?`, proxy, runID)
	if err != nil {
		return fmt.Errorf("store: set run proxy: %w", err)
	}
	return nil
}

// RunCost returns the accumulated usd_estimate for a run.
func (d *DB) RunCost(runID string) (float64, error) {
	var v float64
	err := d.db.QueryRow(`SELECT usd_estimate FROM run_history WHERE run_id=?`, runID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("store: run cost: %w", err)
	}
	return v, nil
}

// LLMCallCount counts llm_calls rows (optionally filtered by run).
func (d *DB) LLMCallCount(runID string) (int, error) {
	var n int
	var err error
	if runID == "" {
		err = d.db.QueryRow(`SELECT COUNT(*) FROM llm_calls`).Scan(&n)
	} else {
		err = d.db.QueryRow(`SELECT COUNT(*) FROM llm_calls WHERE run_id=?`, runID).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("store: count llm calls: %w", err)
	}
	return n, nil
}

// LLMCalls returns llm_calls rows for a run, oldest first (cost/provider assertions).
// Empty runID returns all rows, mirroring LLMCallCount.
func (d *DB) LLMCalls(runID string) ([]LLMCall, error) {
	q := `SELECT provider, model, prompt_tokens, completion_tokens, usd_estimate, purpose FROM llm_calls`
	var args []any
	if runID != "" {
		q += ` WHERE run_id=?`
		args = []any{runID}
	}
	q += ` ORDER BY id`
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list llm calls: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close unactionable
	var out []LLMCall
	for rows.Next() {
		var c LLMCall
		if err := rows.Scan(&c.Provider, &c.Model, &c.PromptTokens, &c.CompletionTokens, &c.USDEstimate, &c.Purpose); err != nil {
			return nil, fmt.Errorf("store: scan llm call: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list llm calls: %w", err)
	}
	return out, nil
}

// ClaimedURL is one frontier row claimed for fetching.
type ClaimedURL struct {
	URL     string
	URLHash string
	Depth   int
}

// GetSelectors returns the cached selector doc for (domain, schemaHash).
func (d *DB) GetSelectors(domain, schemaHash string) (string, bool, error) {
	var doc string
	err := d.db.QueryRow(`SELECT fields_json FROM selector_cache WHERE domain=? AND schema_hash=?`, domain, schemaHash).Scan(&doc)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get selectors: %w", err)
	}
	return doc, true, nil
}

// PutSelectors upserts a selector doc.
func (d *DB) PutSelectors(domain, schemaHash, fieldsJSON string, samplesUsed int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := d.db.Exec(`INSERT INTO selector_cache(domain, schema_hash, fields_json, samples_used, engine_version, synthesized_at, updated_at)
		VALUES(?,?,?,?,1,?,?)
		ON CONFLICT(domain, schema_hash) DO UPDATE SET fields_json=excluded.fields_json, samples_used=excluded.samples_used, updated_at=excluded.updated_at`,
		domain, schemaHash, fieldsJSON, samplesUsed, now, now)
	if err != nil {
		return fmt.Errorf("store: put selectors: %w", err)
	}
	return nil
}

// DeleteSelectors evicts cached selectors; empty hash clears the whole
// domain; both empty clears everything (magpie cache clear with no flags).
func (d *DB) DeleteSelectors(domain, schemaHash string) (int64, error) {
	var res sql.Result
	var err error
	if domain == "" && schemaHash == "" {
		res, err = d.db.Exec(`DELETE FROM selector_cache`)
	} else if schemaHash == "" {
		res, err = d.db.Exec(`DELETE FROM selector_cache WHERE domain=?`, domain)
	} else {
		res, err = d.db.Exec(`DELETE FROM selector_cache WHERE domain=? AND schema_hash=?`, domain, schemaHash)
	}
	if err != nil {
		return 0, fmt.Errorf("store: delete selectors: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete selectors: %w", err)
	}
	return n, nil
}

// Enqueue inserts pending crawl_state rows + dedup marks; returns rows inserted.
func (d *DB) Enqueue(runID string, urls []string, depth int) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: enqueue: %w", err)
	}
	// ponytail: single deferred Rollback covers all error paths; harmless after Commit.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after Commit is a no-op by design
	now := time.Now().UTC().Format(time.RFC3339)
	inserted := 0
	for _, u := range urls {
		h := sha256Hex(u)
		r, err := tx.Exec(`INSERT OR IGNORE INTO crawl_state(run_id, url, url_hash, status, depth, discovered_at, updated_at) VALUES(?,?,?,'pending',?,?,?)`,
			runID, u, h, depth, now, now)
		if err != nil {
			return 0, fmt.Errorf("store: enqueue: %w", err)
		}
		if n, err := r.RowsAffected(); err == nil && n > 0 {
			inserted++
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO dedup(run_id, url_hash, first_seen) VALUES(?,?,?)`, runID, h, now); err != nil {
			return 0, fmt.Errorf("store: enqueue: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: enqueue: %w", err)
	}
	return inserted, nil
}

// Claim atomically flips up to n pending rows to inflight and returns them.
func (d *DB) Claim(runID string, n int) ([]ClaimedURL, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := d.db.Query(`UPDATE crawl_state SET status='inflight', updated_at=? WHERE run_id=? AND url_hash IN
		(SELECT url_hash FROM crawl_state WHERE run_id=? AND status='pending' LIMIT ?)
		RETURNING url, url_hash, depth`, now, runID, runID, n)
	if err != nil {
		return nil, fmt.Errorf("store: claim: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // drain-only close; error unactionable
	var out []ClaimedURL
	for rows.Next() {
		var c ClaimedURL
		if err := rows.Scan(&c.URL, &c.URLHash, &c.Depth); err != nil {
			return nil, fmt.Errorf("store: claim: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claim: %w", err)
	}
	return out, nil
}

// MarkDone marks a row done.
func (d *DB) MarkDone(runID, urlHash string) error {
	_, err := d.db.Exec(`UPDATE crawl_state SET status='done', updated_at=? WHERE run_id=? AND url_hash=?`,
		time.Now().UTC().Format(time.RFC3339), runID, urlHash)
	if err != nil {
		return fmt.Errorf("store: mark done: %w", err)
	}
	return nil
}

// MarkError marks a row errored with a message.
func (d *DB) MarkError(runID, urlHash, msg string) error {
	_, err := d.db.Exec(`UPDATE crawl_state SET status='error', error_msg=?, updated_at=? WHERE run_id=? AND url_hash=?`,
		msg, time.Now().UTC().Format(time.RFC3339), runID, urlHash)
	if err != nil {
		return fmt.Errorf("store: mark error: %w", err)
	}
	return nil
}

// CrawlStats returns pending/inflight/done/error counts for a run.
func (d *DB) CrawlStats(runID string) (pending, inflight, done, errors int, err error) {
	rows, err := d.db.Query(`SELECT status, COUNT(*) FROM crawl_state WHERE run_id=? GROUP BY status`, runID)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: crawl stats: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // drain-only close
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return 0, 0, 0, 0, fmt.Errorf("store: crawl stats: %w", err)
		}
		switch status {
		case "pending":
			pending = n
		case "inflight":
			inflight = n
		case "done":
			done = n
		case "error":
			errors = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("store: crawl stats: %w", err)
	}
	return pending, inflight, done, errors, nil
}

// ResumeRun flips a finished/interrupted run back to running.
func (d *DB) ResumeRun(runID string) error {
	res, err := d.db.Exec(`UPDATE run_history SET status='running', finished_at=NULL WHERE run_id=?`, runID)
	if err != nil {
		return fmt.Errorf("store: resume run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: resume run: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store: resume run: unknown run_id %q", runID)
	}
	return nil
}

// ResetInflight re-queues stranded inflight rows as pending.
func (d *DB) ResetInflight(runID string) (int64, error) {
	res, err := d.db.Exec(`UPDATE crawl_state SET status='pending' WHERE run_id=? AND status='inflight'`, runID)
	if err != nil {
		return 0, fmt.Errorf("store: reset inflight: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: reset inflight: %w", err)
	}
	return n, nil
}

// Seen returns true if urlHash was already recorded for the run.
func (d *DB) Seen(runID, urlHash string) (bool, error) {
	res, err := d.db.Exec(`INSERT OR IGNORE INTO dedup(run_id, url_hash, first_seen) VALUES(?,?,?)`,
		runID, urlHash, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("store: seen: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: seen: %w", err)
	}
	return n == 0, nil
}

// LoadHashes returns all dedup hashes for a run (bloom rebuild on resume).
func (d *DB) LoadHashes(runID string) ([]string, error) {
	rows, err := d.db.Query(`SELECT url_hash FROM dedup WHERE run_id=?`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: load hashes: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // drain-only close
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("store: load hashes: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: load hashes: %w", err)
	}
	return out, nil
}

// ExecRaw runs one DDL/DML statement (used for the on-demand records table).
func (d *DB) ExecRaw(stmt string) error {
	if _, err := d.db.Exec(stmt); err != nil {
		return fmt.Errorf("store: exec: %w", err)
	}
	return nil
}

// InsertRecord appends one crawl record to the on-demand records table.
func (d *DB) InsertRecord(runID, url, recordJSON string) error {
	_, err := d.db.Exec(`INSERT INTO records(run_id, url, record_json, ts) VALUES(?,?,?,?)`,
		runID, url, recordJSON, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store: insert record: %w", err)
	}
	return nil
}

// SelectorEntry is one selector_cache row (inspect helper).
type SelectorEntry struct {
	Domain        string
	SchemaHash    string
	FieldsJSON    string
	SamplesUsed   int
	SynthesizedAt string
}

// ListSelectors lists cached selector docs, optionally filtered by domain.
func (d *DB) ListSelectors(domain string) ([]SelectorEntry, error) {
	q := `SELECT domain, schema_hash, fields_json, samples_used, synthesized_at FROM selector_cache`
	var args []any
	if domain == "" {
		q += ` ORDER BY domain, schema_hash`
	} else {
		q += ` WHERE domain=? ORDER BY schema_hash`
		args = []any{domain}
	}
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list selectors: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // drain-only close
	var out []SelectorEntry
	for rows.Next() {
		var e SelectorEntry
		if err := rows.Scan(&e.Domain, &e.SchemaHash, &e.FieldsJSON, &e.SamplesUsed, &e.SynthesizedAt); err != nil {
			return nil, fmt.Errorf("store: list selectors: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list selectors: %w", err)
	}
	return out, nil
}

// allowlisted tables for the TableCount test helper.
var tables = map[string]bool{
	"selector_cache": true,
	"crawl_state":    true,
	"dedup":          true,
	"run_history":    true,
	"llm_calls":      true,
	"snapshots":      true,
	"records":        true, // created on demand by sqlite-format crawls
}

// TableCount counts rows in a table (test helper).
func (d *DB) TableCount(table string) (int, error) {
	if !tables[table] {
		return 0, fmt.Errorf("store: unknown table %q", table)
	}
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Pragma reads an integer PRAGMA (test helper).
func (d *DB) Pragma(name string) (int, error) {
	switch name {
	case "foreign_keys":
	default:
		return 0, fmt.Errorf("store: unknown pragma %q", name)
	}
	var v int
	if err := d.db.QueryRow(`PRAGMA ` + name).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
