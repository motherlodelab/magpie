package research

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
)

var (
	mdImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink  = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// folds maps what models change when they copy a quote. Markdown
	// emphasis, code, heading, blockquote and table markers and the
	// converter's backslash escapes are deleted outright.
	folds = strings.NewReplacer(
		"“", `"`, "”", `"`, "„", `"`, "‘", "'", "’", "'",
		"–", "-", "—", "-", "−", "-", "…", "...", " ", " ",
		"*", "", "_", "", "`", "", "#", "", ">", "", "|", "", `\`, "",
	)
)

// normText folds what models change when they copy a quote — case,
// whitespace runs, curly quotes, dashes, ellipses, markdown emphasis and
// heading/quote/table markers and escapes, [link](target) → link — and
// nothing else. Applied to both sides, so a fold can loosen a match but
// never fake one.
func normText(s string) string {
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(folds.Replace(strings.ToLower(s))), " ")
}

// quoteFound: minQuoteRunes ≤ len(normText(quote)) ≤ maxQuoteRunes and
// normText(markdown) contains it.
func quoteFound(markdown, quote string) bool { return quoteIn(normText(markdown), quote) }

// quoteIn is quoteFound against an already-folded page.
func quoteIn(page, quote string) bool {
	q := normText(quote)
	if n := utf8.RuneCountInString(q); n < minQuoteRunes || n > maxQuoteRunes {
		return false
	}
	return strings.Contains(page, q)
}

// inRange: an unknown or unparseable published date is kept (ponytail:
// most pages state none, and a missing date proves nothing); otherwise its
// first 10 runes as time.DateOnly must fall within [from, to] by calendar
// day (zero bounds are open).
func inRange(published string, from, to time.Time) bool {
	r := []rune(strings.TrimSpace(published))
	if len(r) < 10 {
		return true
	}
	d, err := time.Parse(time.DateOnly, string(r[:10]))
	if err != nil {
		return true
	}
	if !from.IsZero() && d.Before(calendarDay(from)) {
		return false
	}
	return to.IsZero() || !d.After(calendarDay(to))
}

func calendarDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// verdict is one judged fact's outcome.
type verdict struct{ status, note string }

// verifySource checks one page's freshly stored facts against the pin, in
// Go first: a quote not in the pinned snapshot or a date out of range is
// dropped with no LLM involved. The survivors go to one batched judge call
// (the judge never sees the page; Go already proved the quote is in it). An
// id the judge skips stays unverified, which the writer excludes. It
// returns the usable (verified or softened) verdicts by fact id.
func (r *run) verifySource(ctx context.Context, u string, pin time.Time, facts []Fact) (map[string]verdict, error) {
	snap, ok, err := r.d.DB.SnapshotAt(u, pin)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("research: verify %s: no snapshot at the pin %s", u, pin.Format(time.RFC3339Nano))
	}
	page := normText(snap.Markdown)
	from, to := r.o.Sources.From, r.o.Sources.To
	byID := map[string]bool{}
	var lines []string
	for _, f := range facts {
		switch {
		case !quoteIn(page, f.Quote):
			err = r.setStatus(f.FactID, "dropped", "quote not found in pinned snapshot")
		case !inRange(f.Published, from, to):
			err = r.setStatus(f.FactID, "dropped", fmt.Sprintf("published %s outside %s..%s", f.Published, dateOrOpen(from), dateOrOpen(to)))
		default:
			byID[f.FactID] = true
			lines = append(lines, f.FactID+" | "+oneLine(f.Claim)+" | "+oneLine(f.Quote))
		}
		if err != nil {
			return nil, err
		}
	}
	if len(lines) == 0 {
		return nil, nil
	}
	var j judged
	if err := r.call(ctx, true, "judge", judgeSchema, "Source: "+u+"\n\n"+strings.Join(lines, "\n"), judgeInstr, &j); err != nil {
		return nil, err
	}
	usable := map[string]verdict{}
	for _, v := range j.Verdicts {
		if !byID[v.ID] { // unknown, or a second verdict for one id
			continue
		}
		delete(byID, v.ID)
		out := verdict{"dropped", strings.TrimSpace("judge: " + v.Verdict + ": " + v.Reason)}
		switch claim := oneLine(v.Claim); {
		case v.Verdict == "supported":
			out = verdict{"verified", ""}
		case v.Verdict == "partial" && claim != "":
			out = verdict{"softened", claim}
		}
		if err := r.setStatus(v.ID, out.status, out.note); err != nil {
			return nil, err
		}
		if out.status != "dropped" {
			usable[v.ID] = out
		}
	}
	return usable, nil
}

func (r *run) setStatus(id, status, note string) error {
	return r.d.DB.SetFactStatus(r.id, id, status, note)
}

func dateOrOpen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.DateOnly)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// pivot is a usable fact that would change the answer: it stands only with
// a second independent domain (spec two-domain rule).
type pivot struct {
	id, claim, domain, status, note string // status: verified|softened at mark time
	evidence                        []stance
}

// stance is one counter-evidence read's position on a pivot; ids are the
// usable facts from that page (empty ⇒ ignored: a stance nothing on the
// page backs is the model's opinion, not evidence).
type stance struct {
	verdict, domain string
	ids             []string
}

// settle is pure: contradicting evidence (usable ids) ⇒ contested, note
// "contradicted by f9 (b.example)…"; else supporting evidence from another
// registrable domain ⇒ verified, note "corroborated by f7 (c.example)" (a
// softened pivot keeps its status and note); else ⇒ softened, note "Per
// <domain>: <claim or softened note>" — a single-source pivotal claim
// can't stand as stated.
func settle(p pivot) (status, note string) {
	var contra, support []string
	for _, s := range p.evidence {
		if len(s.ids) == 0 {
			continue
		}
		cite := strings.Join(s.ids, ", ") + " (" + s.domain + ")"
		switch {
		case s.verdict == "contradicts":
			contra = append(contra, cite)
		case s.verdict == "supports" && s.domain != p.domain:
			support = append(support, cite)
		}
	}
	claim := p.claim
	if p.status == "softened" {
		claim = p.note
	}
	switch {
	case len(contra) > 0:
		return "contested", "contradicted by " + strings.Join(contra, "; ")
	case len(support) > 0 && p.status == "softened":
		return p.status, p.note
	case len(support) > 0:
		return "verified", "corroborated by " + strings.Join(support, "; ")
	}
	return "softened", "Per " + p.domain + ": " + claim
}

// regDomain is the registrable domain (eTLD+1) the two-domain rule counts:
// news.a.example and a.example are one source. IPs and bare hosts that
// publicsuffix refuses count as themselves.
func regDomain(host string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}
