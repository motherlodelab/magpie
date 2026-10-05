package research

import "github.com/motherlodelab/magpie/extract"

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
)

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
