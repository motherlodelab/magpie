package vertical

// Upwork job postings. Upwork sits behind Cloudflare's hardest challenge
// mode: from datacenter IPs every upwork.com URL (developer docs included)
// returns 403 with a challenge shell, and the shell survives browser
// escalation. This extractor therefore ships on the surface anonymous
// visitors still get - the og/meta unfurl tags that power link previews -
// plus a best-effort pass over the page's embedded __NEXT_DATA__ state.
// The embedded field map is NOT validated against a live sample from this
// network; treat source:"meta" as the contract and "embedded" as
// best-effort until a clean-IP capture confirms the shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:  "upwork_job",
			Label: "Upwork job",
			Desc:  "Job posting via the page's og/meta unfurl tags, plus a best-effort pass over embedded state JSON. Upwork challenges datacenter IPs; expect a typed challenge error there.",
			Patterns: []string{
				"https://www.upwork.com/jobs/~{ciphertext}",
				"https://www.upwork.com/jobs/{slug}~{ciphertext}",
				"https://www.upwork.com/job/{slug}~{ciphertext}",
			},
		},
		Match:   matchUpwork,
		Extract: extractUpwork,
	})
}

var upworkHosts = []string{"upwork.com", "www.upwork.com"}

// matchUpwork accepts job posting shapes only: /jobs/~<cipher>,
// /jobs/<slug>~<cipher> and /job/<slug>~<cipher>. Search, profiles and
// the developer docs never carry a job payload (and the docs are walled).
func matchUpwork(u *url.URL) bool {
	if !hostIs(u, upworkHosts...) {
		return false
	}
	segs := pathSegs(u.Path)
	if len(segs) == 0 || (segs[0] != "jobs" && segs[0] != "job") {
		return false
	}
	return strings.Contains(segs[len(segs)-1], "~")
}

func extractUpwork(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	// A challenged fetch surfaces typed: StaticFetcher's retry returns
	// *fetch.ChallengeError and it passes straight through to the caller.
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vertical: upwork: parse HTML: %w", err)
	}

	rec := map[string]any{"url": u.String(), "source": "meta"}
	if v := upworkMeta(doc, "og:title"); v != "" {
		rec["title"] = upworkTrimBrand(v)
	}
	if v := upworkMeta(doc, "og:description"); v != "" {
		rec["description"] = v
	}

	// Best-effort embedded state. __NEXT_DATA__ is the canonical
	// hydration location; walk it shallowly for the first job-shaped map
	// (has a ciphertext). Missing or shape-drifted: meta already covered
	// the record, so this can only add, never fail.
	if nd := doc.Find(`script#__NEXT_DATA__`).Text(); strings.TrimSpace(nd) != "" {
		var state map[string]any
		if json.Unmarshal([]byte(nd), &state) == nil {
			if job := upworkFindJob(state, 0); job != nil {
				rec["source"] = "embedded"
				upworkMergeJob(rec, job)
			}
		}
	}

	if _, has := rec["title"]; !has {
		if _, has = rec["description"]; !has {
			return nil, fmt.Errorf("vertical: upwork: no og/meta content on %s (challenge shell or layout drift)", u.String())
		}
	}
	return rec, nil
}

// upworkMeta reads a property- or name-addressed meta tag.
func upworkMeta(doc *goquery.Document, name string) string {
	v := doc.Find(`meta[property="`+name+`"]`).AttrOr("content", "")
	if v == "" {
		v = doc.Find(`meta[name="`+name+`"]`).AttrOr("content", "")
	}
	return strings.TrimSpace(v)
}

// upworkTrimBrand drops the site suffix from og:title ("Job title |
// Upwork" style) when present.
func upworkTrimBrand(s string) string {
	for _, suffix := range []string{" | Upwork", " | Up Work"} {
		if i := strings.LastIndex(s, suffix); i >= 0 {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// upworkFindJob walks a decoded state tree depth-first for the first map
// that looks like a job record (carries a ciphertext). Depth-capped: a
// runaway structure must not hang the scrape.
func upworkFindJob(v any, depth int) map[string]any {
	if depth > 8 {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if _, has := m["ciphertext"]; has {
		return m
	}
	for _, k := range []string{"job", "jobProfile", "posting", "vacancy", "data"} {
		if child, exists := m[k]; exists {
			if hit := upworkFindJob(child, depth+1); hit != nil {
				return hit
			}
		}
	}
	for _, v := range m {
		if hit := upworkFindJob(v, depth+1); hit != nil {
			return hit
		}
	}
	return nil
}

// upworkMergeJob copies known fields out of an embedded job map. Every
// hop is an ok-assertion: unverified shape, so unknown types are skipped,
// never coerced.
func upworkMergeJob(rec map[string]any, job map[string]any) {
	set := func(key string) {
		if _, exists := rec[key]; !exists {
			if v, ok := job[key].(string); ok && strings.TrimSpace(v) != "" {
				rec[key] = strings.TrimSpace(v)
			}
		}
	}
	set("title")
	set("description")
	set("budget")
	set("category")
	set("postedAt")
	if rec["title"] == nil {
		if v, ok := job["title"].(string); ok && strings.TrimSpace(v) != "" {
			rec["title"] = strings.TrimSpace(v)
		}
	}
	if rec["description"] == nil {
		if v, ok := job["description"].(string); ok && strings.TrimSpace(v) != "" {
			rec["description"] = strings.TrimSpace(v)
		}
	}
	if skills, ok := job["skills"].([]any); ok && len(skills) > 0 {
		out := make([]string, 0, len(skills))
		for _, sk := range skills {
			switch s := sk.(type) {
			case string:
				out = append(out, s)
			case map[string]any:
				if name, ok := s["name"].(string); ok {
					out = append(out, name)
				}
			}
		}
		if len(out) > 0 {
			rec["skills"] = out
		}
	}
	if _, exists := rec["id"]; !exists {
		if v, ok := job["ciphertext"].(string); ok && v != "" {
			rec["id"] = v
		}
	}
}
