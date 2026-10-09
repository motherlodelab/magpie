package research

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/scrape"
	"github.com/motherlodelab/magpie/store"
)

// task is one queued search; target names the pivot fact a counter-
// evidence search checks ("" for seed and gap queries).
type task struct{ q, target string }

// spawn starts one sub-researcher; leadMu must be held. errgroup's Go
// never blocks without SetLimit, so a group goroutine (a replan) may spawn.
func (r *run) spawn(g *errgroup.Group, ctx context.Context, a Angle) {
	r.active++
	g.Go(func() error { return r.sub(ctx, g, a) })
}

// sub is one sub-researcher: search its queue (gaps and counter-evidence
// queries jump to the front), read the top unvisited hits, until the queue
// empties or staleSearches searches in a row add no usable fact. Write-now
// ends it cleanly; any other error is fatal to the group.
func (r *run) sub(ctx context.Context, g *errgroup.Group, a Angle) error {
	queue := make([]task, len(a.Queries))
	for i, q := range a.Queries {
		queue[i] = task{q: q}
	}
	searched, stale := false, 0
	var err error
	for len(queue) > 0 && stale < staleSearches && err == nil {
		t := queue[0]
		queue = queue[1:]
		if err = r.boundary(ctx); err != nil || !r.claimQuery(t.q) {
			continue
		}
		var lists [][]scrape.SearchHit
		if lists, err = r.search(ctx, t.q); err != nil {
			break
		}
		searched = true
		usable := 0
		for _, u := range r.pick(lists) {
			if err != nil {
				break
			}
			var n int
			var front []task
			n, front, err = r.read(ctx, a, u, t.target)
			usable += n
			queue = append(r.fresh(front, queue), queue...)
		}
		if usable == 0 {
			stale++
		} else {
			stale = 0
		}
	}
	r.leadMu.Lock()
	defer r.leadMu.Unlock()
	r.active--
	switch {
	case errors.Is(err, ErrWriteNow):
		return nil // siblings hit the same boundary themselves
	case err != nil:
		return err
	}
	return r.replan(ctx, g, searched)
}

// boundary is checked before every search: ctx, the operator's Control,
// then the budget (write-now, tool calls, the research share of the cap).
func (r *run) boundary(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.syncControl(); err != nil {
		return err
	}
	return r.budget.Boundary()
}

// syncControl applies the operator's Control: write-now goes to the
// budget (every later admission refuses, except a judge finishing a page
// already extracted), a new steer is persisted and logged. Cheap, so every
// read and LLM call checks it: Write now acts within one call.
// Before the research_runs row exists (the scope call) Control waits:
// there is no row to steer and no research to stop yet.
func (r *run) syncControl() error {
	if r.ctl == nil {
		return nil
	}
	r.mu.Lock()
	live := r.live
	r.mu.Unlock()
	if !live {
		return nil
	}
	writeNow, s := r.ctl.read()
	if writeNow {
		r.budget.WriteNow()
	}
	r.mu.Lock()
	changed := s != "" && s != r.steer
	if changed {
		r.steer = s
	}
	r.mu.Unlock()
	if !changed {
		return nil
	}
	if err := r.d.DB.SetResearchState(r.id, "running", s); err != nil {
		return err
	}
	r.emit(Event{Stage: "steer", Detail: s})
	return nil
}

// claimQuery marks q issued unless an equal query already was (any sub).
func (r *run) claimQuery(q string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(DedupeQueries(r.issued, []string{q})) == 0 {
		return false
	}
	r.issued = append(r.issued, q)
	return true
}

// fresh keeps the front tasks not already issued or queued, in order.
// ponytail: re-normalizes the issued list per candidate — O(n²) over a
// run's ≤ ~100 queries.
func (r *run) fresh(front, queue []task) []task {
	r.mu.Lock()
	seen := slices.Clone(r.issued)
	r.mu.Unlock()
	for _, t := range queue {
		seen = append(seen, t.q)
	}
	var out []task
	for _, t := range front {
		if len(DedupeQueries(seen, []string{t.q})) == 1 {
			out = append(out, t)
			seen = append(seen, t.q)
		}
	}
	return out
}

// search runs q on every backend (one tool call each, paced per backend);
// a backend error is a warning, not fatal.
func (r *run) search(ctx context.Context, q string) ([][]scrape.SearchHit, error) {
	var lists [][]scrape.SearchHit
	for _, b := range r.backends {
		if err := r.budget.Tool(); err != nil {
			return lists, err
		}
		if err := r.lim.Wait(ctx, "search:"+b); err != nil {
			return lists, err
		}
		r.emit(Event{Stage: "search", Detail: q + " (" + b + ")"})
		recs, err := scrape.Search(ctx, r.d, q, scrape.SearchOptions{Provider: b, Limit: hitsPerSearch})
		if err != nil {
			if ctx.Err() != nil {
				return lists, ctx.Err()
			}
			r.warn("search %s: %v", b, err)
			continue
		}
		hits := make([]scrape.SearchHit, len(recs))
		for i, rec := range recs {
			hits[i] = rec.SearchHit
		}
		lists = append(lists, hits)
	}
	return lists, nil
}

// pick is the fused ranking's first readsPerSearch unvisited URLs, marked
// visited (the canonical URL is also the pin's key).
func (r *run) pick(lists [][]scrape.SearchHit) []string {
	ranked := RankURLs(lists, r.o.Sources)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range ranked {
		if len(out) == readsPerSearch {
			break
		}
		if !r.visited[s.URL] {
			r.visited[s.URL] = true
			out = append(out, s.URL)
		}
	}
	return out
}

// read is one page: its route, a tool call, pacing, a ledger row, then
// the joined scrape. A page its route skips (robots, a session error) or
// that can't be read is recorded (issue: message) and skipped; a skip
// spends no tool call. A read page is pinned, extracted and verified; its
// row is marked done last, so a stop mid-page leaves it pending and a
// resume reads it again. It returns the usable facts and the tasks to
// queue first.
func (r *run) read(ctx context.Context, a Angle, u, target string) (int, []task, error) {
	if err := r.syncControl(); err != nil {
		return 0, nil, err
	}
	rt := r.route(ctx, u)
	if rt.issue != "" {
		if err := ctx.Err(); err != nil {
			return 0, nil, err // a cancelled robots fetch reads as unreachable: not the page's fault
		}
		// Enqueue only now: ahead of Tool, a refused admission would leave
		// the URL pending, and a resume reads pending URLs.
		if _, err := r.d.DB.Enqueue(r.id, []string{u}, 0); err != nil {
			return 0, nil, err
		}
		if err := r.d.DB.MarkError(r.id, store.URLHash(u), rt.issue+": "+rt.detail); err != nil {
			return 0, nil, err
		}
		r.emit(Event{Stage: "unreadable", URL: u, Issue: rt.issue, Detail: rt.detail})
		return 0, nil, nil
	}
	if err := r.budget.Tool(); err != nil {
		return 0, nil, err
	}
	if err := r.lim.Wait(ctx, rt.key); err != nil {
		return 0, nil, err
	}
	if _, err := r.d.DB.Enqueue(r.id, []string{u}, 0); err != nil {
		return 0, nil, err
	}
	r.emit(Event{Stage: "read", URL: u, Session: rt.authed})
	res, err := scrape.Run(ctx, r.d, u, rt.opts)
	if err == nil && strings.TrimSpace(res.Markdown) == "" {
		err = &clean.QualityError{Issue: clean.IssueEmpty, URL: u}
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		issue := issueOf(err) // "<issue>: <detail>" — ListUnreadable reads it back
		if merr := r.d.DB.MarkError(r.id, store.URLHash(u), issue+": "+err.Error()); merr != nil {
			return 0, nil, merr
		}
		r.emit(Event{Stage: "unreadable", URL: u, Issue: issue, Detail: err.Error(), Session: rt.authed})
		return 0, nil, nil
	}
	pin, err := r.d.DB.RecordRunSnapshot(r.id, u, res.Markdown) // linked: Forget purges every copy the run stored
	if err != nil {
		return 0, nil, err
	}
	n, front, err := r.extractPage(ctx, a, u, res.Title, res.Markdown, pin, target, rt.authed)
	if err != nil {
		return 0, nil, err
	}
	return n, front, r.d.DB.MarkDone(r.id, store.URLHash(u))
}

// route is one read's pre-fetch decision. issue set = skip the page.
type route struct {
	opts          scrape.Options
	key           string // the limiter bucket: the host, or "session:"+domain
	authed        bool   // the read carries the user's login
	issue, detail string
}

// route decides how u is read. A session host asks Job.Session: a login
// reads it authenticated (Cookie + capture UA, the session bucket, no
// robots — spec §6.3), a miss reads it as public, an error skips it (the
// hook's error carries no cookie). A public read must pass robots.txt
// (unreachable = disallow, RFC 9309), and a Crawl-delay floors its host.
func (r *run) route(ctx context.Context, u string) route {
	rt := route{opts: scrape.Options{RunID: r.id}, key: hostOf(u)}
	if d := sessionOf(u, r.o.Sources.Sessions); d != "" {
		cookies, ua, ok, err := r.session(u)
		switch {
		case err != nil:
			return route{issue: "error", detail: "session: " + err.Error()}
		case ok && cookies != "": // no cookie = logged out, whatever ok says: robots must apply
			rt.opts.Cookies, rt.key, rt.authed = cookies, "session:"+d, true
			if ua != "" {
				rt.opts.Headers = []string{"User-Agent: " + ua}
			}
			return rt
		}
	}
	switch ok, err := r.robots.Allowed(ctx, u); {
	case err != nil:
		return route{issue: "robots", detail: err.Error()}
	case !ok:
		return route{issue: "robots", detail: "robots.txt disallows it"}
	}
	r.lim.SetFloor(rt.key, r.robots.CrawlDelay(ctx, u)) // 0 = no Crawl-delay: a no-op
	return rt
}

// issueOf buckets a read failure for the couldn't-read list.
func issueOf(err error) string {
	if i := clean.QualityIssue(err); i != clean.IssueNone {
		return string(i)
	}
	var ce *fetch.ChallengeError
	if errors.As(err, &ce) {
		return "challenge"
	}
	return "error"
}

// extractPage extracts one pinned page's facts, stores them, verifies them
// inline, and turns the result into follow-ups: pivotal facts queue their
// counter-evidence search and gaps queue as questions (both at the front),
// and a counter-evidence page records its stance on its target.
//
// authed is the taint: a page read with the user's login composes no
// search query — no counter-evidence search, no gaps, and no findings for
// the replan (which writes new queries). Its facts still reach the judge
// and the writer, whose output never leaves as a query. The stance it
// records on a public target composes nothing, so it counts.
// ponytail: withheld findings mean a sessions-only run replans blind and
// may add angles up to its replan cap. Upgrade: a redacted finding line
// ("fN (session source: <domain>)"). And a pivotal claim read with a login
// gets no counter-evidence search, so it keeps the judge's verdict instead
// of the two-domain rule's. Upgrade: settle it single-source.
func (r *run) extractPage(ctx context.Context, a Angle, u, title, md string, pin time.Time, target string, authed bool) (int, []task, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Research question: %s\nBrief: %s\n", a.Question, r.plan.Brief)
	if s := r.steering(); s != "" {
		fmt.Fprintf(&b, "Operator steering: %s\n", s)
	}
	if c := r.pivotClaim(target); c != "" {
		fmt.Fprintf(&b, "Target claim to check: %s\n", c)
	}
	fmt.Fprintf(&b, "Source: %s — %s\n\n%s", u, oneLine(title), scrape.CapWords(md, maxPageWords))
	var ex extracted
	if err := r.call(ctx, false, "extract", extractSchema, b.String(), extractInstr, &ex); err != nil {
		if soft(ctx, err) {
			r.warn("extract %s: %v", u, err)
			return 0, nil, nil
		}
		return 0, nil, err
	}

	var facts []Fact
	var counter []string // per stored fact: its counter query when pivotal
	pivots := 0
	for _, f := range ex.Facts {
		if len(facts) == maxFactsPerPage {
			break
		}
		// A quote is evidence text, never markup: a link or image the page
		// (or an injection) put in it would pass the folded check and then
		// render wherever the ledger is shown.
		quote := strings.TrimSpace(unlinked(f.Quote))
		if oneLine(f.Claim) == "" || quote == "" {
			continue // the store refuses them, and nothing could verify them
		}
		cq := ""
		if f.Pivotal && !authed && pivots < maxPivotsPerPage && oneLine(f.CounterQuery) != "" {
			cq = oneLine(f.CounterQuery)
			pivots++
		}
		conf := f.Confidence
		if math.IsNaN(conf) {
			conf = 0
		}
		facts = append(facts, Fact{Claim: oneLine(f.Claim), Quote: quote, URL: u, CheckedAt: pin,
			Published: strings.TrimSpace(f.Published), Confidence: min(max(conf, 0), 1)})
		counter = append(counter, cq)
	}
	ids, err := r.d.DB.PutFacts(r.id, facts)
	if err != nil {
		return 0, nil, err
	}
	for i := range facts {
		facts[i].FactID = ids[i]
	}
	r.mu.Lock()
	r.facts += len(facts) // stored: counted whatever the verdicts
	r.mu.Unlock()
	usable, err := r.verifySource(ctx, u, pin, facts)
	if err != nil {
		if !soft(ctx, err) {
			return 0, nil, err
		}
		r.warn("judge %s: %v", u, err) // facts stay unverified: the writer excludes them
	}

	domain := regDomain(hostOf(u))
	var front []task
	r.mu.Lock()
	r.usable += len(usable)
	var usableIDs []string
	for i, f := range facts {
		v, ok := usable[f.FactID]
		if !ok {
			continue
		}
		usableIDs = append(usableIDs, f.FactID)
		claim := f.Claim
		if v.status == "softened" {
			claim = v.note
		}
		if !authed {
			r.findings = append(r.findings, finding(f.FactID, claim, u))
		}
		if counter[i] != "" {
			r.pivots[f.FactID] = &pivot{id: f.FactID, claim: f.Claim, domain: domain, status: v.status, note: v.note}
			front = append(front, task{q: counter[i], target: f.FactID})
		}
	}
	if p := r.pivots[target]; p != nil && (ex.Stance == "supports" || ex.Stance == "contradicts") {
		p.evidence = append(p.evidence, stance{verdict: ex.Stance, domain: domain, ids: usableIDs})
	}
	newPivots := make([]pivot, 0, len(front))
	for _, t := range front {
		newPivots = append(newPivots, *r.pivots[t.target])
	}
	r.mu.Unlock()

	// A pivot's provisional verdict is single-source (settle with no
	// evidence), stored now: if the run stops before settling, the ledger
	// already says what a lone source can support.
	for _, p := range newPivots {
		p.evidence = nil
		status, note := settle(p)
		if err := r.setStatus(p.id, status, note); err != nil {
			return 0, nil, err
		}
	}
	for _, g := range ex.Gaps[:min(len(ex.Gaps), maxGapsPerPage)] {
		if g = oneLine(g); g != "" && !authed {
			front = append(front, task{q: g})
		}
	}
	r.emit(Event{Stage: "facts", URL: u, Detail: fmt.Sprintf("%d facts, %d usable", len(facts), len(usable)), Session: authed})
	return len(usable), front, nil
}

func (r *run) pivotClaim(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pivots[id]; p != nil {
		return p.claim
	}
	return ""
}

func finding(id, claim, u string) string {
	return id + " " + oneLine(claim) + " (" + regDomain(hostOf(u)) + ")"
}

// replan is the lead's turn after a sub-researcher reports (leadMu held):
// at most SubResearchers replans per run (EstimateRun's count), only after
// a sub that searched, and only while a slot is free. New angles fill free
// slots; "done" ends replanning.
func (r *run) replan(ctx context.Context, g *errgroup.Group, searched bool) error {
	n := efforts[r.o.Effort].SubResearchers
	free := n - r.active
	if !searched || r.settled || r.replans >= n || free < 1 {
		return nil
	}
	r.replans++
	var b strings.Builder
	fmt.Fprintf(&b, "Brief: %s\n", r.plan.Brief)
	r.mu.Lock()
	if r.steer != "" {
		fmt.Fprintf(&b, "Operator steering: %s\n", r.steer)
	}
	findings := r.findings[max(0, len(r.findings)-maxFindings):]
	b.WriteString("Angles so far:\n")
	for _, a := range r.plan.Angles {
		fmt.Fprintf(&b, "- %s\n", a.Question)
	}
	fmt.Fprintf(&b, "Findings (latest %d):\n%s\n", len(findings), strings.Join(findings, "\n"))
	r.mu.Unlock()
	spent, err := r.d.DB.RunCost(r.id)
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "Spent: $%.4f of $%.2f. Free slots: %d.", spent, r.o.MaxCostUSD, free)

	var rp replanned
	if err := r.call(ctx, false, "replan", replanSchema, b.String(), replanInstr(free), &rp); err != nil {
		if errors.Is(err, ErrWriteNow) {
			return nil
		}
		if soft(ctx, err) {
			r.warn("replan: %v", err)
			return nil
		}
		return err
	}
	if rp.Done {
		r.settled = true
		r.emit(Event{Stage: "replan", Detail: "done: " + oneLine(rp.Reason)})
		return nil
	}
	add := tidyPlan(Plan{Angles: rp.Angles}, free).Angles
	r.emit(Event{Stage: "replan", Detail: fmt.Sprintf("%d new angle(s): %s", len(add), oneLine(rp.Reason))})
	for _, a := range add {
		r.mu.Lock()
		r.plan.Angles = append(r.plan.Angles, a)
		r.mu.Unlock()
		r.spawn(g, ctx, a)
	}
	return nil
}

// settlePivots applies the two-domain rule to every pivot once research
// has ended.
func (r *run) settlePivots() error {
	r.mu.Lock()
	ps := make([]pivot, 0, len(r.pivots))
	for _, p := range r.pivots {
		ps = append(ps, *p)
	}
	r.mu.Unlock()
	for _, p := range ps {
		status, note := settle(p)
		if err := r.setStatus(p.id, status, note); err != nil {
			return err
		}
	}
	return nil
}
