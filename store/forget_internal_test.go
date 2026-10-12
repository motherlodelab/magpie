package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteResearchRun_Atomic (v0.1.31, review of #53): a forget that fails
// part-way changes nothing — before, the facts went first, so a retry found
// no pins and the cited page text was orphaned for good. Internal: the
// failure is a planted trigger on the run's last table.
func TestDeleteResearchRun_Atomic(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() }) //nolint:errcheck // teardown
	if err := d.BeginRun("r", "research"); err != nil {
		t.Fatal(err)
	}
	if err := d.PutResearchRun(ResearchRun{RunID: "r", Question: "q", Plan: "{}", Options: "{}"}); err != nil {
		t.Fatal(err)
	}
	pin, err := d.RecordRunSnapshot("r", "https://gated.test/a", "gated page text")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.PutFacts("r", []Fact{{Claim: "c", Quote: "q", URL: "https://gated.test/a", CheckedAt: pin}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`CREATE TRIGGER block_forget BEFORE DELETE ON research_runs BEGIN SELECT RAISE(ABORT, 'planted failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteResearchRun("r"); err == nil || !strings.Contains(err.Error(), "planted failure") {
		t.Fatalf("forget with a failing last step = %v, want the planted failure", err)
	}
	// RED on v0.1.30: facts 0 and the snapshot gone — half a forget.
	if got, err := d.Facts("r"); err != nil || len(got) != 1 {
		t.Errorf("facts after a failed forget = %d, %v; want 1 (rolled back)", len(got), err)
	}
	if _, ok, err := d.SnapshotAt("https://gated.test/a", pin); err != nil || !ok {
		t.Errorf("snapshot after a failed forget = %v, %v; want kept (rolled back)", ok, err)
	}
	// The retry, once the failure is gone, still finds and purges the copy.
	if _, err := d.db.Exec(`DROP TRIGGER block_forget`); err != nil {
		t.Fatal(err)
	}
	if n, err := d.DeleteResearchRun("r"); err != nil || n != 1 {
		t.Errorf("retry = %d, %v; want 1 copy purged", n, err)
	}
}
