package research_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/research"
	"github.com/motherlodelab/magpie/store"
)

// TestDiffEvidence builds two runs straight through the store: no loop, no
// model — DiffEvidence reads only the ledger and the pins.
func TestDiffEvidence(t *testing.T) {
	t.Parallel()
	db, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() }) //nolint:errcheck // test cleanup

	const (
		uS, uT, uU, uV, uW = "https://s.example/a", "https://t.example/a", "https://u.example/a", "https://v.example/a", "https://w.example/a"
	)
	type fact struct{ url, md, quote, status string }
	words := func(prefix string, n int) string {
		w := make([]string, n)
		for i := range w {
			w[i] = prefix
		}
		return strings.Join(w, " ")
	}
	build := func(id string, facts []fact) {
		t.Helper()
		if err := db.BeginRun(id, "research"); err != nil {
			t.Fatal(err)
		}
		if err := db.PutResearchRun(store.ResearchRun{RunID: id, Question: "q", Plan: "{}", Options: "{}"}); err != nil {
			t.Fatal(err)
		}
		for _, f := range facts {
			pin, err := db.RecordSnapshot(f.url, f.md)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := db.PutFacts(id, []store.Fact{{Claim: "c " + f.quote, Quote: f.quote, URL: f.url, CheckedAt: pin}})
			if err != nil {
				t.Fatal(err)
			}
			note := ""
			if f.status == "softened" {
				note = "reworded"
			}
			if err := db.SetFactStatus(id, ids[0], f.status, note); err != nil {
				t.Fatal(err)
			}
		}
	}
	build("prev", []fact{
		{uS, "the price is ten dollars today", "price is ten dollars", "verified"},
		{uT, "same page both runs", "same page both runs", "verified"},
		{uU, "only in the old run", "only in the old run", "verified"},
		{uW, words("old", 2500), "old old old old old", "verified"},
		{uT, "same page both runs", "same page both", "dropped"}, // invisible
	})
	build("cur", []fact{
		{uS, "the price is twenty dollars today", "the price is twenty dollars", "verified"},
		{uT, "same page both runs", "same page both runs", "contested"},
		{uV, "only in the new run", "only in the new run", "softened"},
		{uW, words("new", 2500), "new new new new new", "verified"},
		{uV, "only in the new run", "a dropped quote", "dropped"}, // invisible
	})

	d, err := research.DiffEvidence(db, "prev", "cur")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Added, []string{uV}) || !slices.Equal(d.Gone, []string{uU}) {
		t.Errorf("Added %v Gone %v, want [%s] / [%s]", d.Added, d.Gone, uV, uU)
	}
	if len(d.Changed) != 2 || d.Changed[0].URL != uS || !strings.Contains(d.Changed[0].Diff, "twenty") || d.Changed[0].TooLarge {
		t.Errorf("Changed = %+v, want S with a diff (T's hash is unchanged)", d.Changed)
	}
	if len(d.Changed) == 2 && (d.Changed[1].URL != uW || !d.Changed[1].TooLarge || d.Changed[1].Diff != "") {
		t.Errorf("Changed[1] = %+v, want W TooLarge with no diff (2500-word window)", d.Changed[1])
	}
	if len(d.Moved) != 1 || d.Moved[0].URL != uT || d.Moved[0].PrevStatus != "verified" || d.Moved[0].CurStatus != "contested" {
		t.Errorf("Moved = %+v, want T verified → contested", d.Moved)
	}
	var added, gone []string
	for _, f := range d.FactsAdded {
		added = append(added, f.Quote)
	}
	for _, f := range d.FactsGone {
		gone = append(gone, f.Quote)
	}
	slices.Sort(added)
	slices.Sort(gone)
	if want := []string{"new new new new new", "only in the new run", "the price is twenty dollars"}; !slices.Equal(added, want) {
		t.Errorf("FactsAdded = %q, want %q (dropped facts invisible)", added, want)
	}
	if want := []string{"old old old old old", "only in the old run", "price is ten dollars"}; !slices.Equal(gone, want) {
		t.Errorf("FactsGone = %q, want %q", gone, want)
	}
	if _, err := research.DiffEvidence(db, "prev", "nope"); err == nil {
		t.Error("an unknown run diffed without error")
	}
}
