package research

import (
	"fmt"
	"slices"
	"strings"

	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// EvidenceDiff is "what changed since the last report" between two runs'
// usable evidence (verified, softened, contested; dropped and unverified
// facts are invisible).
type EvidenceDiff struct {
	Added, Gone []string // source URLs with usable facts in only cur / only prev, sorted
	Changed     []SourceChange
	FactsAdded  []Fact // usable in cur, no match in prev — match key (URL, normText(quote))
	FactsGone   []Fact
	Moved       []FactMove // matched, status differs
}

// SourceChange is one shared source whose pinned content differs.
type SourceChange struct {
	URL, Diff string // DiffWords(prev pin markdown, cur pin markdown)
	TooLarge  bool   // DiffWords refused the window (> 2000 words); Diff == ""
}

// FactMove is one matched fact whose verdict changed.
type FactMove struct{ URL, Quote, PrevStatus, CurStatus string }

// DiffEvidence compares two runs' evidence (spec: "what changed since the
// last report"). A shared source is Changed only when its pinned content
// hashes differ; a source pinned more than once in a run (a resumed
// re-read) compares its latest pin. Unknown runs error.
func DiffEvidence(db *store.DB, prevRunID, curRunID string) (EvidenceDiff, error) {
	prev, err := usableLedger(db, prevRunID)
	if err != nil {
		return EvidenceDiff{}, err
	}
	cur, err := usableLedger(db, curRunID)
	if err != nil {
		return EvidenceDiff{}, err
	}
	var d EvidenceDiff
	pp, cp := latestPins(prev), latestPins(cur)
	for u, c := range cp {
		p, ok := pp[u]
		switch {
		case !ok:
			d.Added = append(d.Added, u)
		case p.ContentHash != c.ContentHash:
			ch, err := sourceChange(db, p, c)
			if err != nil {
				return EvidenceDiff{}, err
			}
			d.Changed = append(d.Changed, ch)
		}
	}
	for u := range pp {
		if _, ok := cp[u]; !ok {
			d.Gone = append(d.Gone, u)
		}
	}
	slices.Sort(d.Added)
	slices.Sort(d.Gone)
	slices.SortFunc(d.Changed, func(a, b SourceChange) int { return strings.Compare(a.URL, b.URL) })

	pm, cm := byQuote(prev), byQuote(cur)
	for _, f := range cur {
		p, ok := pm[factKey(f)]
		switch {
		case !ok:
			d.FactsAdded = append(d.FactsAdded, f)
		case p.Status != f.Status:
			d.Moved = append(d.Moved, FactMove{URL: f.URL, Quote: f.Quote, PrevStatus: p.Status, CurStatus: f.Status})
		}
	}
	for _, f := range prev {
		if _, ok := cm[factKey(f)]; !ok {
			d.FactsGone = append(d.FactsGone, f)
		}
	}
	return d, nil
}

// usableLedger is a run's usable facts in id order; an unknown run errors.
func usableLedger(db *store.DB, runID string) ([]Fact, error) {
	if _, err := db.GetResearchRun(runID); err != nil {
		return nil, fmt.Errorf("research: diff: %w", err)
	}
	all, err := db.Facts(runID)
	if err != nil {
		return nil, err
	}
	var out []Fact
	for _, f := range all {
		if slices.Contains(usableStatus, f.Status) {
			out = append(out, f)
		}
	}
	return out, nil
}

// latestPins maps each source URL to its newest pinned fact.
func latestPins(facts []Fact) map[string]Fact {
	m := map[string]Fact{}
	for _, f := range facts {
		if p, ok := m[f.URL]; !ok || f.CheckedAt.After(p.CheckedAt) {
			m[f.URL] = f
		}
	}
	return m
}

func sourceChange(db *store.DB, p, c Fact) (SourceChange, error) {
	ps, _, err := db.SnapshotAt(p.URL, p.CheckedAt)
	if err != nil {
		return SourceChange{}, err
	}
	cs, _, err := db.SnapshotAt(c.URL, c.CheckedAt)
	if err != nil {
		return SourceChange{}, err
	}
	diff, err := scrape.DiffWords(ps.Markdown, cs.Markdown)
	if err != nil { // the window is past DiffWords' bound: report the change, not a diff
		return SourceChange{URL: c.URL, TooLarge: true}, nil
	}
	return SourceChange{URL: c.URL, Diff: diff}, nil
}

func factKey(f Fact) [2]string { return [2]string{f.URL, normText(f.Quote)} }

func byQuote(facts []Fact) map[[2]string]Fact {
	m := make(map[[2]string]Fact, len(facts))
	for _, f := range facts {
		if _, ok := m[factKey(f)]; !ok {
			m[factKey(f)] = f
		}
	}
	return m
}
