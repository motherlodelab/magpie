package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Snapshot is the latest watch check-in for a URL.
type Snapshot struct {
	URL         string
	ContentHash string
	Markdown    string
	CheckedAt   time.Time
	Changed     bool
}

// PutSnapshot appends one snapshot row. Append-only, no pruning
// (ponytail: unbounded history for long-lived watches; a keep-N prune
// is the upgrade path when a user actually hits it). checked_at is
// RFC3339Nano, NOT second precision: the (url_hash, checked_at) primary
// key plus the 100ms test ticks make same-second inserts routine, so
// second precision would be a guaranteed PK-violation flake — and
// fixed-width Nano keeps lexicographic ORDER BY correct.
func (d *DB) PutSnapshot(rawURL, contentHash, markdown string, changed bool) error {
	_, err := d.db.Exec(`INSERT INTO snapshots(url_hash, url, content_hash, markdown, checked_at, changed) VALUES(?,?,?,?,?,?)`,
		sha256Hex(rawURL), rawURL, contentHash, markdown,
		time.Now().UTC().Format(time.RFC3339Nano), boolInt(changed))
	if err != nil {
		return fmt.Errorf("store: put snapshot: %w", err)
	}
	return nil
}

// LatestSnapshot returns the newest snapshot for rawURL; (zero, false,
// nil) on an empty history.
func (d *DB) LatestSnapshot(rawURL string) (Snapshot, bool, error) {
	var s Snapshot
	var checked string
	var changed int
	err := d.db.QueryRow(`SELECT url, content_hash, markdown, checked_at, changed
		FROM snapshots WHERE url_hash=? ORDER BY checked_at DESC LIMIT 1`,
		sha256Hex(rawURL)).Scan(&s.URL, &s.ContentHash, &s.Markdown, &checked, &changed)
	if err == sql.ErrNoRows {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("store: latest snapshot: %w", err)
	}
	t, err := time.Parse(time.RFC3339Nano, checked)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("store: latest snapshot: parse checked_at: %w", err)
	}
	s.CheckedAt = t
	s.Changed = changed != 0
	return s, true, nil
}

// ListSnapshots returns the newest limit snapshots for rawURL, newest
// first; empty slice (not nil error) on an unknown URL. limit <= 0
// clamps to 10.
func (d *DB) ListSnapshots(rawURL string, limit int) ([]Snapshot, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.Query(`SELECT url, content_hash, markdown, checked_at, changed
		FROM snapshots WHERE url_hash=? ORDER BY checked_at DESC LIMIT ?`,
		sha256Hex(rawURL), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list snapshots: %w", err)
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var s Snapshot
		var checked string
		var changed int
		if err := rows.Scan(&s.URL, &s.ContentHash, &s.Markdown, &checked, &changed); err != nil {
			return nil, fmt.Errorf("store: list snapshots: %w", err)
		}
		t, err := time.Parse(time.RFC3339Nano, checked)
		if err != nil {
			return nil, fmt.Errorf("store: list snapshots: parse checked_at: %w", err)
		}
		s.CheckedAt = t
		s.Changed = changed != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
