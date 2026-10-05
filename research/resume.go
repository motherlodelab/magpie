package research

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/motherlodelab/magpie/store"
)

// resumeJob replaces the caller's question and options with the stored
// ones; only a higher cap is taken from the caller.
func resumeJob(j Job, rr store.ResearchRun) (Job, error) {
	var o Options
	if err := json.Unmarshal([]byte(rr.Options), &o); err != nil {
		return Job{}, fmt.Errorf("research: resume %s: options: %w", rr.RunID, err)
	}
	o.MaxCostUSD = max(o.MaxCostUSD, j.Options.MaxCostUSD)
	j.Question, j.Options = rr.Question, o
	return j, nil
}

// load reads a resumed run's state, writing nothing: the stored plan and
// steer, the visited set from the read ledger's done and error rows (a
// crashed pending row is simply read again), the reads made, and the
// ledger's counts and findings.
func (r *run) load(rr store.ResearchRun) error {
	if err := json.Unmarshal([]byte(rr.Plan), &r.plan); err != nil {
		return fmt.Errorf("research: resume %s: plan: %w", r.id, err)
	}
	r.steer = rr.Steer
	urls, err := r.d.DB.CrawlURLs(r.id)
	if err != nil {
		return err
	}
	r.reads = len(urls)
	for _, c := range urls {
		if c.Status == "done" || c.Status == "error" {
			r.visited[c.URL] = true
		}
	}
	facts, err := r.d.DB.Facts(r.id)
	if err != nil {
		return err
	}
	for _, f := range facts {
		r.facts++
		if slices.Contains(usableStatus, f.Status) {
			r.usable++
			r.findings = append(r.findings, finding(f.FactID, f.Claim, f.URL))
		}
	}
	return nil
}
