package store

import (
	"database/sql"
	"fmt"
	"strings"
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

// PutSnapshot appends one snapshot row and returns the checked_at it
// stored — the exact pin SnapshotAt reads back and a research fact cites.
// Append-only, no pruning (ponytail: unbounded history for long-lived
// watches; a keep-N prune is the upgrade path when a user actually hits
// it, and it must skip rows pinned by facts — the FK refuses to delete
// them anyway). checked_at is RFC3339Nano, NOT second precision: the
// (url_hash, checked_at) primary key plus the 100ms test ticks make
// same-second inserts routine, so second precision would be a guaranteed
// PK-violation flake. RFC3339Nano trims trailing zeros, so it is not
// fixed-width: lexicographic ORDER BY can misorder two rows only when the
// earlier lands exactly on a 10^-k s boundary and the later arrives within
// 10^-k s of it — negligible, and exact-match reads (SnapshotAt) don't care.
func (d *DB) PutSnapshot(rawURL, contentHash, markdown string, changed bool) (time.Time, error) {
	now := time.Now().UTC() // UTC() also strips the monotonic reading: parse(format(now)) == now
	_, err := d.db.Exec(`INSERT INTO snapshots(url_hash, url, content_hash, markdown, checked_at, changed) VALUES(?,?,?,?,?,?)`,
		sha256Hex(rawURL), rawURL, contentHash, markdown,
		now.Format(time.RFC3339Nano), boolInt(changed))
	if err != nil {
		return time.Time{}, fmt.Errorf("store: put snapshot: %w", err)
	}
	return now, nil
}

// RecordSnapshot is the check-in every reader shares (desktop scrapes,
// research reads): hash the markdown (hex SHA-256, watch's content hash),
// mark it changed against the latest version — the first version is a
// baseline, not a change (watch's rule) — append it, and return the pin a
// store.Fact cites. Empty or whitespace-only markdown is an error: there is
// nothing to pin. Watch keeps its own path (it needs the previous markdown
// for the diff).
// ponytail: latest-then-put is not atomic — two readers of one URL at once
// can both mark changed; the flag is display-only and both pins stay exact.
func (d *DB) RecordSnapshot(rawURL, markdown string) (time.Time, error) {
	if strings.TrimSpace(markdown) == "" {
		return time.Time{}, fmt.Errorf("store: record snapshot: empty markdown for %q", rawURL)
	}
	hash := sha256Hex(markdown)
	prev, ok, err := d.LatestSnapshot(rawURL)
	if err != nil {
		return time.Time{}, err
	}
	return d.PutSnapshot(rawURL, hash, markdown, ok && prev.ContentHash != hash)
}

// LatestSnapshot returns the newest snapshot for rawURL; (zero, false,
// nil) on an empty history.
func (d *DB) LatestSnapshot(rawURL string) (Snapshot, bool, error) {
	return d.oneSnapshot("latest snapshot", `WHERE url_hash=? ORDER BY checked_at DESC LIMIT 1`, sha256Hex(rawURL))
}

// SnapshotAt returns the exact version stored at checkedAt (a value
// PutSnapshot returned, in any time zone) — older pins survive newer
// versions. (zero, false, nil) on a miss.
func (d *DB) SnapshotAt(rawURL string, checkedAt time.Time) (Snapshot, bool, error) {
	return d.oneSnapshot("snapshot at", `WHERE url_hash=? AND checked_at=?`,
		sha256Hex(rawURL), checkedAt.UTC().Format(time.RFC3339Nano))
}

// oneSnapshot runs the shared one-row query, scan and parse; where is the
// clause after FROM snapshots.
func (d *DB) oneSnapshot(op, where string, args ...any) (Snapshot, bool, error) {
	var s Snapshot
	var checked string
	var changed int
	err := d.db.QueryRow(`SELECT url, content_hash, markdown, checked_at, changed FROM snapshots `+where, args...).
		Scan(&s.URL, &s.ContentHash, &s.Markdown, &checked, &changed)
	if err == sql.ErrNoRows {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("store: %s: %w", op, err)
	}
	t, err := time.Parse(time.RFC3339Nano, checked)
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("store: %s: parse checked_at: %w", op, err)
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
	defer func() { _ = rows.Close() }() //nolint:errcheck // read-only; close error unactionable
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
