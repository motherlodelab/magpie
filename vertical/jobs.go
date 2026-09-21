package vertical

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "job_posting",
			Label:    "Job posting",
			Desc:     "Job record from schema.org JobPosting JSON-LD (Greenhouse/Lever-style boards). Explicit-only: matches any page.",
			Patterns: []string{"https://{jobs-site}/jobs/{id}"},
		},
		Match:   matchHTTP,
		Extract: extractJobPosting,
		OptIn:   true,
	})
}

var (
	tagRe = regexp.MustCompile(`<[^>]*>`)
	wsRe  = regexp.MustCompile(`\s+`)
)

// stripTags removes HTML tags and collapses whitespace.
// ponytail: naive — no HTML unescape, inline tags glue words and block
// tags lose breaks; upgrade path = a clean-package HTML-to-text helper if
// a real page's description ever matters beyond keyword reading.
func stripTags(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(tagRe.ReplaceAllString(s, ""), " "))
}

func extractJobPosting(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	m, ok := firstTypedBlock(body, "JobPosting")
	if !ok {
		return nil, fmt.Errorf("vertical: job_posting: no JobPosting data at %s", u.String())
	}
	rec := map[string]any{"url": u.String()}
	if v := str(m, "title"); v != "" {
		rec["title"] = v
	}
	if org := child(m, "hiringOrganization"); org != nil {
		if v := str(org, "name"); v != "" {
			rec["organization"] = v
		}
	}
	// remote: TELECOMMUTE at top level or on any jobLocation Place; only
	// emitted when detected (never a guessed false).
	if str(m, "jobLocationType") == "TELECOMMUTE" {
		rec["remote"] = true
	}
	var locs []string
	for _, l := range blockArray(m["jobLocation"]) {
		p := anyMap(l)
		if p == nil {
			continue
		}
		if str(p, "jobLocationType") == "TELECOMMUTE" {
			rec["remote"] = true
		}
		addr := child(p, "address")
		if addr == nil {
			continue
		}
		parts := nonEmpty(str(addr, "addressLocality"), str(addr, "addressRegion"), addressCountry(addr))
		if len(parts) > 0 {
			locs = append(locs, strings.Join(parts, ", "))
		}
	}
	if len(locs) > 0 {
		rec["locations"] = locs
	}
	if v := str(m, "datePosted"); v != "" {
		rec["datePosted"] = v
	}
	if v := str(m, "validThrough"); v != "" {
		rec["validThrough"] = v
	}
	if v := str(m, "employmentType"); v != "" {
		rec["employmentType"] = v
	}
	if s := salaryMap(m["baseSalary"]); s != nil {
		rec["salary"] = s
	}
	if d := str(m, "description"); d != "" {
		rec["description"] = stripTags(d)
	}
	return rec, nil
}

// salaryMap normalizes baseSalary: a QuantitativeValue nested under
// "value" (MonetaryAmount), directly on the object, or bare numbers in
// either slot.
func salaryMap(v any) map[string]any {
	b := anyMap(v)
	if b == nil {
		if n := numVal(v); n != 0 {
			return map[string]any{"value": n}
		}
		return nil
	}
	qv := child(b, "value")
	if qv == nil {
		qv = b // minValue/maxValue may sit directly on the salary object
	}
	out := map[string]any{}
	if n := num(qv, "minValue"); n != 0 {
		out["min"] = n
	}
	if n := num(qv, "maxValue"); n != 0 {
		out["max"] = n
	}
	if u := str(qv, "unitText"); u != "" {
		out["unit"] = u
	}
	if n := numVal(b["value"]); n != 0 { // "value" as a bare number
		out["value"] = n
	}
	if c := str(b, "currency"); c != "" {
		out["currency"] = c
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
