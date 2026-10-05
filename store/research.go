package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ResearchRun is the checkpoint row of one deep-research run: its launch
// inputs, where it is, and — once done — its report. Every research run is
// also a run_history row (command "research") that carries History and
// spend; research_runs FKs it.
type ResearchRun struct {
	RunID      string
	Question   string
	Plan       string // JSON plan at launch, opaque to store
	Options    string // JSON options, opaque to store; the caller strips secrets
	Status     string // running|writing|done|failed|stopped
	Steer      string // latest steering message (resume reads it)
	Report     string // final markdown; "" unless done
	StartedAt  time.Time
	FinishedAt time.Time // zero while in flight
}

// Fact is one claim in a run's ledger: an exact quote pinned to one stored
// snapshot version (URL + CheckedAt). The facts FK refuses a pin that names
// no stored snapshot, so a fact can never cite a page the system didn't
// fetch and store.
type Fact struct {
	FactID      string    // minted by PutFacts ("f1"…); ignored on input
	Claim       string    // as extracted; never rewritten
	Quote       string    // claimed verbatim substring of the pinned markdown
	URL         string    // hashed into the pin; read back from the snapshot
	CheckedAt   time.Time // the pin: PutSnapshot's return value
	ContentHash string    // read from the pinned snapshot by Facts; ignored on input
	Published   string    // page-stated date, "" unknown
	Confidence  float64   // [0,1], model-assigned, display-only
	Status      string    // Facts output; PutFacts always stores unverified
	Note        string    // Facts output; set by SetFactStatus
}

// Status sets, one per writer. ponytail: validated in Go, not a CHECK —
// SQLite can't ALTER a CHECK, so a new status is one map entry here
// instead of a table rebuild.
var (
	researchStates   = map[string]bool{"running": true, "writing": true}
	researchFinishes = map[string]bool{"done": true, "failed": true, "stopped": true}
	factVerdicts     = map[string]bool{"verified": true, "softened": true, "dropped": true, "contested": true}
)

// PutResearchRun records a run's launch: RunID, Question, Plan and Options
// are stored with status running and started_at now; the other fields are
// the store's and ignored on input. BeginRun(RunID, "research") must come
// first — the FK refuses a research run without its run_history row.
func (d *DB) PutResearchRun(r ResearchRun) error {
	switch {
	case r.RunID == "":
		return fmt.Errorf("store: put research run: empty run id")
	case r.Question == "":
		return fmt.Errorf("store: put research run %q: empty question", r.RunID)
	case !json.Valid([]byte(r.Plan)):
		return fmt.Errorf("store: put research run %q: plan is not valid JSON", r.RunID)
	case !json.Valid([]byte(r.Options)):
		return fmt.Errorf("store: put research run %q: options are not valid JSON", r.RunID)
	}
	_, err := d.db.Exec(`INSERT INTO research_runs(run_id, question, plan, options, status, started_at) VALUES(?,?,?,?,'running',?)`,
		r.RunID, r.Question, r.Plan, r.Options, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store: put research run %q (duplicate, or no run_history row — BeginRun first): %w", r.RunID, err)
	}
	return nil
}

// SetResearchState moves a run to running or writing and stores the latest
// steer. On a failed or stopped run it is the resume (finished_at clears).
// A done run is terminal — a follow-up is a new run, so a report is never
// erased.
func (d *DB) SetResearchState(runID, status, steer string) error {
	if !researchStates[status] {
		return fmt.Errorf("store: set research state: status %q is not running|writing", status)
	}
	return d.execOne("set research state", fmt.Sprintf("run %q unknown or done", runID),
		`UPDATE research_runs SET status=?, steer=?, finished_at=NULL WHERE run_id=? AND status!='done'`,
		status, steer, runID)
}

// FinishResearchRun ends an in-flight run as done, failed or stopped. A
// report is stored iff the status is done.
func (d *DB) FinishResearchRun(runID, status, report string) error {
	if !researchFinishes[status] {
		return fmt.Errorf("store: finish research run: status %q is not done|failed|stopped", status)
	}
	if (status == "done") != (report != "") {
		return fmt.Errorf("store: finish research run %q: a report is required iff status is done (got %s)", runID, status)
	}
	var rep any // NULL unless done
	if report != "" {
		rep = report
	}
	return d.execOne("finish research run", fmt.Sprintf("run %q unknown or not in flight", runID),
		`UPDATE research_runs SET status=?, report=?, finished_at=? WHERE run_id=? AND status IN ('running','writing')`,
		status, rep, time.Now().UTC().Format(time.RFC3339), runID)
}

// GetResearchRun reads one run; unknown ids error loudly.
func (d *DB) GetResearchRun(runID string) (ResearchRun, error) {
	r := ResearchRun{RunID: runID}
	var started string
	var finished sql.NullString
	err := d.db.QueryRow(`SELECT question, plan, options, status, steer, COALESCE(report,''), started_at, finished_at
		FROM research_runs WHERE run_id=?`, runID).
		Scan(&r.Question, &r.Plan, &r.Options, &r.Status, &r.Steer, &r.Report, &started, &finished)
	if err == sql.ErrNoRows {
		return ResearchRun{}, fmt.Errorf("store: unknown research run %q", runID)
	}
	if err != nil {
		return ResearchRun{}, fmt.Errorf("store: get research run: %w", err)
	}
	if r.StartedAt, err = time.Parse(time.RFC3339, started); err != nil {
		return ResearchRun{}, fmt.Errorf("store: get research run %q: parse started_at: %w", runID, err)
	}
	if finished.Valid {
		if r.FinishedAt, err = time.Parse(time.RFC3339, finished.String); err != nil {
			return ResearchRun{}, fmt.Errorf("store: get research run %q: parse finished_at: %w", runID, err)
		}
	}
	return r, nil
}

// PutFacts appends facts to a run's ledger, all or nothing, and returns the
// ids it minted in input order ("f1"… per run). Each fact is stored
// unverified; FactID, ContentHash, Status and Note are ignored on input. A
// fact whose pin (URL, CheckedAt) names no stored snapshot, or a run with
// no research_runs row, fails the whole batch.
func (d *DB) PutFacts(runID string, facts []Fact) ([]string, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	for i, f := range facts {
		var bad string
		switch {
		case f.Claim == "":
			bad = "empty claim"
		case f.Quote == "":
			bad = "empty quote"
		case f.URL == "":
			bad = "empty url"
		case f.CheckedAt.IsZero():
			bad = "zero CheckedAt (pin it with PutSnapshot's return)"
		case !(f.Confidence >= 0 && f.Confidence <= 1): // NaN fails too
			bad = fmt.Sprintf("confidence %v outside [0,1]", f.Confidence)
		}
		if bad != "" {
			return nil, fmt.Errorf("store: put facts: fact %d: %s", i, bad)
		}
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: put facts: %w", err)
	}
	// ponytail: single deferred Rollback covers all error paths; harmless after Commit.
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after Commit is a no-op by design
	ids := make([]string, len(facts))
	for i, f := range facts {
		// The id is minted in-statement and the write is the tx's first
		// statement: a SELECT-MAX-then-INSERT would be a read→write upgrade,
		// which WAL fails with SQLITE_BUSY_SNAPSHOT (no busy retry) when
		// another handle (desktop vs CLI) committed in between.
		err := tx.QueryRow(`INSERT INTO facts(run_id, fact_id, claim, quote, url_hash, checked_at, published, confidence)
			SELECT ?, 'f' || (COALESCE(MAX(CAST(substr(fact_id, 2) AS INTEGER)), 0) + 1), ?, ?, ?, ?, ?, ?
			FROM facts WHERE run_id = ?
			RETURNING fact_id`,
			runID, f.Claim, f.Quote, sha256Hex(f.URL), f.CheckedAt.UTC().Format(time.RFC3339Nano), f.Published, f.Confidence,
			runID).Scan(&ids[i])
		if err != nil {
			return nil, fmt.Errorf("store: put facts: fact %d (%s): unknown run or unpinned snapshot: %w", i, f.URL, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: put facts: %w", err)
	}
	return ids, nil
}

// Facts returns a run's ledger in id order (f10 after f9), with URL and
// ContentHash joined from each fact's pinned snapshot; an empty slice for
// an unknown run.
func (d *DB) Facts(runID string) ([]Fact, error) {
	// Inner join is safe: the FK guarantees every fact's snapshot exists.
	rows, err := d.db.Query(`SELECT f.fact_id, f.claim, f.quote, s.url, f.checked_at, s.content_hash,
			f.published, f.confidence, f.status, f.note
		FROM facts f JOIN snapshots s ON s.url_hash = f.url_hash AND s.checked_at = f.checked_at
		WHERE f.run_id = ? ORDER BY CAST(substr(f.fact_id, 2) AS INTEGER)`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: facts: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close error unactionable
	out := []Fact{}
	for rows.Next() {
		var f Fact
		var checked string
		if err := rows.Scan(&f.FactID, &f.Claim, &f.Quote, &f.URL, &checked, &f.ContentHash,
			&f.Published, &f.Confidence, &f.Status, &f.Note); err != nil {
			return nil, fmt.Errorf("store: facts: %w", err)
		}
		if f.CheckedAt, err = time.Parse(time.RFC3339Nano, checked); err != nil {
			return nil, fmt.Errorf("store: facts: %s: parse checked_at: %w", f.FactID, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: facts: %w", err)
	}
	return out, nil
}

// SetFactStatus records the verifier's verdict on one fact. unverified is
// insert-only. softened needs a note — the reworded claim, since Claim is
// never rewritten; for dropped or contested the note says why.
func (d *DB) SetFactStatus(runID, factID, status, note string) error {
	if !factVerdicts[status] {
		return fmt.Errorf("store: set fact status: status %q is not verified|softened|dropped|contested", status)
	}
	if status == "softened" && note == "" {
		return fmt.Errorf("store: set fact status: softened %q in run %q needs a note (the reworded claim)", factID, runID)
	}
	return d.execOne("set fact status", fmt.Sprintf("unknown fact %q in run %q", factID, runID),
		`UPDATE facts SET status=?, note=? WHERE run_id=? AND fact_id=?`, status, note, runID, factID)
}

// URLHash is the key crawl_state and snapshots store for rawURL (MarkDone
// and MarkError take it).
func URLHash(rawURL string) string { return sha256Hex(rawURL) }

// CrawlURL is one crawl_state row: a research run's read ledger reuses the
// crawl frontier (pending|inflight|done|error; Msg is error_msg).
type CrawlURL struct{ URL, Status, Msg string }

// CrawlURLs returns a run's crawl_state rows, oldest first; an empty slice
// for an unknown run. Research resume seeds its visited set from the done
// and error rows, and the couldn't-read list is the error rows.
func (d *DB) CrawlURLs(runID string) ([]CrawlURL, error) {
	rows, err := d.db.Query(`SELECT url, status, COALESCE(error_msg,'') FROM crawl_state
		WHERE run_id=? ORDER BY discovered_at, rowid`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: crawl urls: %w", err)
	}
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close error unactionable
	out := []CrawlURL{}
	for rows.Next() {
		var c CrawlURL
		if err := rows.Scan(&c.URL, &c.Status, &c.Msg); err != nil {
			return nil, fmt.Errorf("store: crawl urls: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: crawl urls: %w", err)
	}
	return out, nil
}
