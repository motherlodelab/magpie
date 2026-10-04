package store_test

import (
	"testing"
	"time"

	"github.com/motherlodelab/magpie/store"
)

func TestSnapshots_RoundTrip(t *testing.T) {
	db := openTempDB(t)
	if _, err := db.PutSnapshot("https://example.com/p/1", "hash1", "markdown one", false); err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	got, ok, err := db.LatestSnapshot("https://example.com/p/1")
	if err != nil || !ok {
		t.Fatalf("LatestSnapshot = (%+v, %v, %v), want found", got, ok, err)
	}
	if got.URL != "https://example.com/p/1" || got.ContentHash != "hash1" || got.Markdown != "markdown one" {
		t.Errorf("snapshot = %+v", got)
	}
	if got.Changed {
		t.Error("Changed = true, want false")
	}
	if got.CheckedAt.IsZero() {
		t.Error("CheckedAt zero — RFC3339Nano contract broken")
	}
}

func TestSnapshots_LatestWins(t *testing.T) {
	db := openTempDB(t)
	for i, hash := range []string{"h1", "h2", "h3"} {
		if _, err := db.PutSnapshot("https://example.com/p", hash, "md", i > 0); err != nil {
			t.Fatalf("PutSnapshot %d: %v", i, err)
		}
	}
	got, ok, err := db.LatestSnapshot("https://example.com/p")
	if err != nil || !ok {
		t.Fatalf("LatestSnapshot = (%+v, %v)", ok, err)
	}
	if got.ContentHash != "h3" || !got.Changed {
		t.Errorf("latest = %+v, want h3 + changed", got)
	}
}

// Same-wall-second inserts are routine (100ms test ticks): the RFC3339Nano
// checked_at + PK contract means both rows persist. A "tidy up the
// timestamp format" refactor fails HERE, not in a nightly.
func TestSnapshots_SameSecondDoubleInsert(t *testing.T) {
	db := openTempDB(t)
	if _, err := db.PutSnapshot("https://example.com/x", "a", "before", false); err != nil {
		t.Fatalf("first PutSnapshot: %v", err)
	}
	if _, err := db.PutSnapshot("https://example.com/x", "b", "after", true); err != nil {
		t.Fatalf("second PutSnapshot (same second): %v", err)
	}
	n, err := db.TableCount("snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2 (RFC3339Nano prevents the PK collision)", n)
	}
	got, ok, err := db.LatestSnapshot("https://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.ContentHash != "b" {
		t.Errorf("latest = %+v, want the second insert", got)
	}
	if got.CheckedAt.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("checked_at = %v, want ~now", got.CheckedAt)
	}
}

func TestSnapshots_PerURLIsolation(t *testing.T) {
	db := openTempDB(t)
	if _, err := db.PutSnapshot("https://a.example/", "ha", "md-a", false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.LatestSnapshot("https://b.example/"); err != nil || ok {
		t.Errorf("b LatestSnapshot = (%v, %v), want not-found", ok, err)
	}
}

func TestSnapshots_EmptyDB(t *testing.T) {
	db := openTempDB(t)
	got, ok, err := db.LatestSnapshot("https://nothing.example/")
	if err != nil || ok {
		t.Errorf("empty DB = (%+v, %v, %v), want zero/false/nil", got, ok, err)
	}
}

func TestSnapshots_List(t *testing.T) {
	db := openTempDB(t)
	for i, hash := range []string{"h1", "h2", "h3"} {
		if _, err := db.PutSnapshot("https://example.com/p", hash, "md", i > 0); err != nil {
			t.Fatalf("PutSnapshot %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond) // distinct checked_at ordering
	}
	got, err := db.ListSnapshots("https://example.com/p", 2)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ContentHash != "h3" || got[1].ContentHash != "h2" {
		t.Errorf("order = [%s, %s], want [h3, h2] newest-first", got[0].ContentHash, got[1].ContentHash)
	}
	if got[0].CheckedAt.IsZero() {
		t.Error("CheckedAt zero — RFC3339Nano contract broken")
	}
	// limit <= 0 clamps to 10.
	all, err := db.ListSnapshots("https://example.com/p", 0)
	if err != nil || len(all) != 3 {
		t.Errorf("ListSnapshots(limit 0) = %d rows, %v; want 3, nil", len(all), err)
	}
	// Unknown URL: empty non-nil slice, nil error.
	none, err := db.ListSnapshots("https://nothing.example/", 5)
	if err != nil || none == nil || len(none) != 0 {
		t.Errorf("unknown URL = %#v, %v; want empty non-nil, nil", none, err)
	}
}

// TestSnapshots_PutReturnsPin: the returned checked_at is the stored key —
// the value a research fact pins and the watch webhook carries.
func TestSnapshots_PutReturnsPin(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	const url = "https://example.com/pin"
	at, err := db.PutSnapshot(url, "hash1", "markdown one", false)
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	latest, ok, err := db.LatestSnapshot(url)
	if err != nil || !ok {
		t.Fatalf("LatestSnapshot: %v (%v)", err, ok)
	}
	if !at.Equal(latest.CheckedAt) {
		t.Errorf("returned pin != stored: %v vs %v", at, latest.CheckedAt)
	}
	got, ok, err := db.SnapshotAt(url, at)
	if err != nil || !ok {
		t.Fatalf("SnapshotAt: %v (%v)", err, ok)
	}
	if got.Markdown != "markdown one" || got.ContentHash != "hash1" {
		t.Errorf("SnapshotAt = %+v, want the stored row", got)
	}
}

// TestSnapshotAt_PinsOlderVersion is the whole point of the pin: a newer
// version of the page must not move what an older pin reads.
func TestSnapshotAt_PinsOlderVersion(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	const url = "https://example.com/versions"
	at1, err := db.PutSnapshot(url, "h1", "version one", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutSnapshot(url, "h2", "version two", true); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.SnapshotAt(url, at1)
	if err != nil || !ok {
		t.Fatalf("SnapshotAt(at1) = (%v, %v)", ok, err)
	}
	if got.Markdown != "version one" || got.ContentHash != "h1" || got.Changed {
		t.Errorf("SnapshotAt(at1) = %+v, want version one", got)
	}
	if latest, ok, err := db.LatestSnapshot(url); err != nil || !ok || latest.Markdown != "version two" {
		t.Errorf("latest = %q (%v, %v), want version two", latest.Markdown, ok, err)
	}
}

// TestSnapshotAt_Miss: misses are (zero, false, nil); the match is exact
// (+1ns misses) but zone-independent (the same instant in ICT hits).
func TestSnapshotAt_Miss(t *testing.T) {
	t.Parallel()
	db := openTempDB(t)
	const url = "https://example.com/miss"
	at, err := db.PutSnapshot(url, "h", "md", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		url  string
		at   time.Time
		want bool
	}{
		{"unknown url", "https://nothing.example/", at, false},
		{"plus one ns", url, at.Add(time.Nanosecond), false},
		{"same instant in ICT", url, at.In(time.FixedZone("ICT", 7*3600)), true},
	} {
		got, ok, err := db.SnapshotAt(tc.url, tc.at)
		if err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
		if ok != tc.want {
			t.Errorf("%s: found = %v, want %v", tc.name, ok, tc.want)
		}
		if !ok && got != (store.Snapshot{}) {
			t.Errorf("%s: miss returned %+v, want zero Snapshot", tc.name, got)
		}
	}
}
