package research

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
)

var (
	// A link target may hold one level of balanced parentheses
	// (wikipedia's /wiki/Go_(programming_language)) and a "title".
	mdImage = regexp.MustCompile(`!\[([^\]]*)\]\((?:[^()\s]|\([^()\s]*\))*(?:\s+"[^"]*")?\)`)
	mdLink  = regexp.MustCompile(`\[([^\]]*)\]\((?:[^()\s]|\([^()\s]*\))*(?:\s+"[^"]*")?\)`)
	// Block markers only where they are markers: a heading's #s and a
	// blockquote's >s at a line's start, a table row's pipes. Inline,
	// ">100 ms" and "#1" keep their meaning.
	mdBlock  = regexp.MustCompile(`(?m)^[ \t]*(?:(?:>[ \t]?)+|#{1,6}[ \t]+)`)
	mdTable  = regexp.MustCompile(`(?m)^[ \t]*\|.*$`)
	mdEscape = regexp.MustCompile("\\\\([!-/:-@\\[-`{-~])") // \* → * (CommonMark escapes)
	// folds maps the characters models swap when they copy a quote.
	folds = strings.NewReplacer(
		"“", `"`, "”", `"`, "„", `"`, "‘", "'", "’", "'",
		"–", "-", "—", "-", "−", "-", "…", "...", "\u00a0", " ", "`", "",
	)
)

// normText folds what models change when they copy a quote — case,
// whitespace runs, curly quotes, dashes, ellipses, code ticks, markdown
// escapes, emphasis delimiters, heading/blockquote/table markers,
// [link](target) → link — and nothing that carries meaning: an inline > or
// #, a spaced * (30 * 2) and an intraword _ (snake_case) are kept. Applied
// to both sides, so a fold can loosen a match but never fake one.
func normText(s string) string {
	s = unlink(mdImage, s)
	s = unlink(mdLink, s)
	s = mdBlock.ReplaceAllString(s, "")
	s = mdTable.ReplaceAllStringFunc(s, func(row string) string { return strings.ReplaceAll(row, "|", " ") })
	s = mdEscape.ReplaceAllString(s, "$1")
	s = stripEmphasis(s)
	return strings.Join(strings.Fields(folds.Replace(strings.ToLower(s))), " ")
}

// unlink replaces each link (or image) with its text, and keeps a link's
// edges as word boundaries: html-to-markdown glues adjacent elements
// ("[#67627](u)cmd/compile" — an issue link, then the title), which would
// otherwise make "cmd/compile" look like the end of a longer word.
func unlink(re *regexp.Regexp, s string) string {
	ms := re.FindAllStringSubmatchIndex(s, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		text := s[m[2]:m[3]]
		b.WriteString(s[last:m[0]])
		first, _ := utf8.DecodeRuneInString(text)
		if before, _ := utf8.DecodeLastRuneInString(s[:m[0]]); alnum(before) && alnum(first) {
			b.WriteByte(' ')
		}
		b.WriteString(text)
		end, _ := utf8.DecodeLastRuneInString(text)
		if after, _ := utf8.DecodeRuneInString(s[m[1]:]); alnum(after) && alnum(end) {
			b.WriteByte(' ')
		}
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// stripEmphasis deletes the runs of * and _ that CommonMark's flanking
// rules let open or close emphasis; a run that can't (spaced on both
// sides, or an underscore inside a word) is text and stays.
func stripEmphasis(s string) string {
	if !strings.ContainsAny(s, "*_") {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		c := rs[i]
		if c != '*' && c != '_' {
			b.WriteRune(c)
			i++
			continue
		}
		j := i
		for j < len(rs) && rs[j] == c {
			j++
		}
		prev, next := ' ', ' ' // a line's edge counts as whitespace
		if i > 0 {
			prev = rs[i-1]
		}
		if j < len(rs) {
			next = rs[j]
		}
		left := !unicode.IsSpace(next) && (!isPunct(next) || unicode.IsSpace(prev) || isPunct(prev))
		right := !unicode.IsSpace(prev) && (!isPunct(prev) || unicode.IsSpace(next) || isPunct(next))
		delim := left || right
		if c == '_' {
			delim = (left && (!right || isPunct(prev))) || (right && (!left || isPunct(next)))
		}
		if !delim {
			b.WriteString(string(rs[i:j]))
		}
		i = j
	}
	return b.String()
}

func isPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

// quoteFound: minQuoteRunes ≤ len(q) ≤ maxQuoteRunes and normText(markdown)
// holds q at word and number boundaries, where q is normText(quote) with
// its edge punctuation and wrapping quote marks trimmed. Models end a
// mid-sentence fragment with a period ("…platforms." where the page goes
// on ", making it…") or wrap it in quotes; the trim moves where a quote may
// end, never what it says. The boundaries keep a quote from dropping a
// prefix ("supported" inside "unsupported") or cutting a number short
// ("Go 1.1" inside "Go 1.18", "$1,000" inside "$1,000,000").
func quoteFound(markdown, quote string) bool { return quoteIn(normText(markdown), quote) }

// quoteIn is quoteFound against an already-folded page.
func quoteIn(page, quote string) bool {
	q := strings.TrimSpace(strings.Trim(normText(quote), `.,;:!?"' `))
	if n := utf8.RuneCountInString(q); n < minQuoteRunes || n > maxQuoteRunes {
		return false
	}
	for off := 0; off < len(page); {
		k := strings.Index(page[off:], q)
		if k < 0 {
			return false
		}
		if i := off + k; bounded(page, i, i+len(q)) {
			return true
		}
		off += k + 1
	}
	return false
}

// bounded: page[i:j] neither starts nor ends inside a word or a number
// (a digit run continued by "." or "," and another digit is one number).
func bounded(page string, i, j int) bool {
	first, _ := utf8.DecodeRuneInString(page[i:j])
	last, _ := utf8.DecodeLastRuneInString(page[i:j])
	before, bn := utf8.DecodeLastRuneInString(page[:i])
	after, an := utf8.DecodeRuneInString(page[j:])
	if alnum(first) && alnum(before) || alnum(last) && alnum(after) {
		return false
	}
	if unicode.IsDigit(first) && (before == '.' || before == ',') {
		if r, _ := utf8.DecodeLastRuneInString(page[:i-bn]); unicode.IsDigit(r) {
			return false
		}
	}
	if unicode.IsDigit(last) && (after == '.' || after == ',') {
		if r, _ := utf8.DecodeRuneInString(page[j+an:]); unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func alnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

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
