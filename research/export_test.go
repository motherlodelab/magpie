package research

import (
	"context"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/scrape"
)

// JudgeOf exposes the unexported judge rule to the black-box tests.
var JudgeOf = Options.judge

// SetLimits lifts read/search pacing for tests (package vars; tests that
// call it stay serial).
func SetLimits(rps float64, burst int) { limitRPS, limitBurst = rps, burst }

// The verifier's pure functions, for the black-box tables.
var (
	NormText   = normText
	QuoteFound = quoteFound
	InRange    = inRange
	RegDomain  = regDomain
	Backends   = backends
)

// AdmitFinish exposes the judge's admission (write-now doesn't refuse it).
var AdmitFinish = (*Budget).admitFinish

// Schemas exposes the four structured-call schemas by task name.
var Schemas = map[string]*extract.Schema{"plan": planSchema, "extract": extractSchema, "judge": judgeSchema, "replan": replanSchema}

// Pivot and Stance mirror the unexported pivot/stance for TestSettle.
type (
	Pivot struct {
		ID, Claim, Domain, Status, Note string
		Evidence                        []Stance
	}
	Stance struct {
		Verdict, Domain string
		IDs             []string
	}
)

// Settle runs the two-domain rule on a mirrored pivot.
func Settle(p Pivot) (status, note string) {
	in := pivot{id: p.ID, claim: p.Claim, domain: p.Domain, status: p.Status, note: p.Note}
	for _, s := range p.Evidence {
		in.evidence = append(in.evidence, stance{verdict: s.Verdict, domain: s.Domain, ids: s.IDs})
	}
	return settle(in)
}

// SessionGap exposes the authenticated-read floor (TestRoute compares rates).
func SessionGap() time.Duration { return sessionGap }

// Route runs prepare, then one read's routing decision for rawURL — the
// hook, robots, the bucket — and reports the limit its bucket ended with.
// The black-box tests can't reach r.lim; this is the one window.
func Route(ctx context.Context, d scrape.Deps, j Job, rawURL string) (key string, authed bool, issue, detail string, limit rate.Limit, err error) {
	r, err := prepare(d, j, strings.TrimSpace(j.Question))
	if err != nil {
		return "", false, "", "", 0, err
	}
	rt := r.route(ctx, rawURL)
	return rt.key, rt.authed, rt.issue, rt.detail, r.lim.Limit(rt.key), nil
}
