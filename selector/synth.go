package selector

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/extract"

	"github.com/PuerkitoBio/goquery"
)

// ProposeFunc proposes CSS selectors for fields the free paths missed.
type ProposeFunc func(ctx context.Context, fields []string, trimmedHTML string) (map[string]string, error)

// Synthesize builds a SelectorDoc from validated samples. Candidate order:
// css_hint → heuristic → LLM propose (only for fields both missed).
// Ground truth arrives IN the samples; Synthesize makes no LLM calls itself.
func Synthesize(ctx context.Context, samples []SynthSample, sch *extract.Schema, propose ProposeFunc, only []string) (SelectorDoc, error) {
	if len(samples) == 0 {
		return SelectorDoc{}, fmt.Errorf("selector: no samples (never cache nothing)")
	}
	if len(samples) == 1 {
		warnf("selector: synthesizing from 1 sample; agreement is vacuous")
	}
	if HasXPathHint(sch) {
		warnf("selector: xpath_hint ignored (CSS-only engine)")
	}
	n := len(samples)
	doc := SelectorDoc{
		SchemaHash:    SchemaHash(sch),
		Fields:        map[string]FieldSelector{},
		SynthesizedAt: time.Now().UTC().Format(time.RFC3339),
		SamplesUsed:   n,
		EngineVersion: 2,
	}
	want := map[string]bool{}
	for _, f := range SchemaFields(sch) {
		want[f] = true
	}
	if only != nil {
		keep := map[string]bool{}
		for _, f := range only {
			keep[f] = true
		}
		want = keep
	}

	var missing []string
	for _, field := range sortedKeys(want) {
		if path, ok := sch.Hints.JSONLDPath[field]; ok && path != "" {
			sel := FieldSelector{Type: "jsonld", Expr: path}
			if hits, _ := FieldAgreement(samples, field, sel, sch); hits >= n-1 {
				doc.Fields[field] = sel
			} else {
				missing = append(missing, field)
			}
			continue
		}
		// 1. css_hint first — zero work when valid.
		if hint, ok := sch.Hints.CSSHint[field]; ok && hint != "" {
			sel := FieldSelector{Type: "css", Expr: hint,
				Regex: sch.Hints.Regex[field], Coerce: sch.Hints.Coerce[field]}
			if hits, _ := FieldAgreement(samples, field, sel, sch); hits >= need(n) {
				if hits < n {
					warnf("selector: field %q agrees on %d/%d via css_hint; caching anyway", field, hits, n)
				}
				doc.Fields[field] = sel
				continue
			}
			warnf("css_hint %q for field %q failed validation; falling through to heuristic", hint, field)
		}
		// 2. Heuristic generator.
		if sel, hits, ok := heuristicField(samples, field, sch); ok && hits >= need(n) {
			if hits < n {
				warnf("selector: field %q agrees on %d/%d via heuristic; caching anyway", field, hits, n)
			}
			doc.Fields[field] = sel
			continue
		}
		missing = append(missing, field)
	}

	// 3. LLM proposal last, only for fields the free paths missed.
	if len(missing) > 0 {
		if propose == nil {
			warnf("selector: fields %v have no validating selector and no proposer; non-cacheable", missing)
		} else {
			got, err := propose(ctx, missing, trimmedHTML(samples, sch))
			if err != nil {
				return SelectorDoc{}, fmt.Errorf("selector: propose: %w", err)
			}
			for _, field := range missing {
				expr, ok := got[field]
				if !ok || expr == "" {
					continue
				}
				sel := FieldSelector{Type: "css", Expr: expr,
					Regex: sch.Hints.Regex[field], Coerce: sch.Hints.Coerce[field]}
				if hits, _ := FieldAgreement(samples, field, sel, sch); hits >= need(n) {
					if hits < n {
						warnf("selector: field %q agrees on %d/%d via proposal; caching anyway", field, hits, n)
					}
					doc.Fields[field] = sel
				}
			}
		}
	}

	// Final validation: ≥(N-1)/N per field; 2/3 caches with a named warning,
	// the rest degrade to per-page LLM (absent from the doc, never silently cached).
	var nonCacheable []string
	for _, field := range sortedKeys(want) {
		sel, ok := doc.Fields[field]
		if !ok {
			nonCacheable = append(nonCacheable, field)
			continue
		}
		if sel.Type == "jsonld" {
			continue
		}
		hits, total := FieldAgreement(samples, field, sel, sch)
		switch {
		case hits == total:
			// clean
		case total > 1 && hits == total-1:
			warnf("selector: field %q agrees on %d/%d; caching anyway", field, hits, total)
		default:
			delete(doc.Fields, field)
			nonCacheable = append(nonCacheable, field)
		}
	}
	if len(nonCacheable) > 0 {
		warnf("selector: fields %v below agreement threshold; per-page LLM extraction (not cached)", nonCacheable)
	}
	// Fingerprints for the relocation pass (zero-LLM heal middle step):
	// remember what the matched element looks like on the newest sample
	// where the cached selector actually hits. css fields only.
	for _, field := range sortedKeys(want) {
		sel, ok := doc.Fields[field]
		if !ok || sel.Type != "css" {
			continue
		}
		for i := len(samples) - 1; i >= 0; i-- {
			s := samples[i]
			d, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(s.HTML)))
			if err != nil {
				continue
			}
			if _, hit := ExtractFieldValue(d, s.Sidecar, field, sel, sch.Hints); !hit {
				continue
			}
			if fp, ok := fingerprintSel(d, sel, sch.Hints); ok {
				if doc.Fingerprints == nil {
					doc.Fingerprints = map[string]ElementFP{}
				}
				doc.Fingerprints[field] = fp
			}
			break
		}
	}
	return doc, nil
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// truthNeedle renders a truth value as searchable page text.
func truthNeedle(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64, float32, int, int64, bool:
		return CanonicalJSON(v), true
	default:
		return "", false
	}
}

var layoutClassRe = regexp.MustCompile(`^(col|row|container|wrapper)-`)

// need is the acceptance threshold: unanimous, except N>=3 allows one
// miss (the spec (N-1)/N rule). N=2 with one miss is evidence of nothing.
func need(n int) int {
	if n >= 3 {
		return n - 1
	}
	return n
}

// heuristicField finds the candidate selector with the best agreement across
// samples (first-max wins). ok=false when no candidate matches any sample.
func heuristicField(samples []SynthSample, field string, sch *extract.Schema) (FieldSelector, int, bool) {
	n := len(samples)
	for _, s := range samples {
		v, present := s.Truth[field]
		if !present || v == nil {
			return FieldSelector{}, 0, false
		}
		if _, ok := truthNeedle(v); !ok {
			return FieldSelector{}, 0, false // non-scalar truth: LLM path only
		}
	}
	sel := FieldSelector{Type: "css", Regex: sch.Hints.Regex[field], Coerce: sch.Hints.Coerce[field]}
	seen := map[string]bool{}
	var cands []string
	for _, s := range samples {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(s.HTML)))
		if err != nil {
			return FieldSelector{}, 0, false
		}
		// ok proven above for every sample; nd=="" would match nothing.
		nd, _ := truthNeedle(s.Truth[field])
		for _, c := range emitForDoc(doc, nd) {
			if !seen[c] {
				seen[c] = true
				cands = append(cands, c)
			}
		}
	}
	best, bestHits := FieldSelector{}, 0
	for _, c := range cands {
		sel.Expr = c
		if hits, _ := FieldAgreement(samples, field, sel, sch); hits > bestHits {
			bestHits = hits
			best = sel
			if bestHits == n {
				break
			}
		}
	}
	if bestHits == 0 {
		return FieldSelector{}, 0, false
	}
	return best, bestHits, true
}

type candNode struct {
	sel   *goquery.Selection
	depth int
}

// emitForDoc collects candidate selectors for elements containing needle,
// deepest (most specific) elements first.
func emitForDoc(doc *goquery.Document, needle string) []string {
	var nodes []candNode
	doc.Find("*").Each(func(_ int, s *goquery.Selection) {
		if !strings.Contains(s.Text(), needle) {
			return
		}
		nodes = append(nodes, candNode{sel: s, depth: s.Parents().Length()})
	})
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].depth > nodes[j].depth })
	var out []string
	seen := map[string]bool{}
	for _, nd := range nodes {
		s := nd.sel
		for up := 0; up <= 4 && s.Length() > 0; up++ {
			for _, c := range emitForSel(s) {
				if !seen[c] {
					seen[c] = true
					out = append(out, c)
				}
			}
			s = s.Parent()
		}
	}
	return out
}

// emitForSel emits candidate selectors for one element, cheapest first.
func emitForSel(s *goquery.Selection) []string {
	var out []string
	node := s.Get(0)
	if node == nil {
		return nil
	}
	tag := node.Data
	id, hasID := s.Attr("id")
	if hasID && id != "" {
		out = append(out, "#"+cssEscape(id), tag+"#"+cssEscape(id))
	}
	var classes []string
	if cls, ok := s.Attr("class"); ok {
		for _, c := range strings.Fields(cls) {
			if layoutClassRe.MatchString(c) || c == "clearfix" {
				continue
			}
			classes = append(classes, "."+cssEscape(c))
		}
	}
	if len(classes) > 0 {
		joined := strings.Join(classes, "")
		if len(joined) < 160 {
			out = append(out, joined, tag+joined)
		}
	}
	for _, attr := range []string{"name", "itemprop", "data-testid", "data-test", "data-id"} {
		if v, ok := s.Attr(attr); ok && v != "" && len(v) < 64 && !strings.Contains(v, " ") {
			out = append(out, "["+attr+"=\""+v+"\"]", tag+"["+attr+"=\""+v+"\"]")
		}
	}
	// Positional fallback: tag + class + nth-of-type.
	idx := s.PrevAllFiltered(tag).Length() + 1
	if len(classes) > 0 {
		out = append(out, fmt.Sprintf("%s%s:nth-of-type(%d)", tag, strings.Join(classes, ""), idx))
	} else {
		out = append(out, fmt.Sprintf("%s:nth-of-type(%d)", tag, idx))
	}
	return out
}

func cssEscape(s string) string {
	// Minimal escape for ids/classes; common cases need none.
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-' || r == '_':
			return r
		default:
			return -1
		}
	}, s)
}

// trimmedHTML builds the LLM-proposal payload: script/style stripped, only
// subtrees ≤4 ancestors above truth-containing nodes, capped at MaxTokens.
func trimmedHTML(samples []SynthSample, sch *extract.Schema) string {
	var needles []string
	for _, f := range SchemaFields(sch) {
		for _, s := range samples {
			if v, present := s.Truth[f]; present && v != nil {
				if nd, ok := truthNeedle(v); ok {
					needles = append(needles, nd)
				}
			}
		}
	}
	seen := map[string]bool{}
	var parts []string
	for _, s := range samples {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(s.HTML)))
		if err != nil {
			continue
		}
		doc.Find("script, style, noscript").Remove()
		doc.Find("*").Each(func(_ int, sel *goquery.Selection) {
			text := sel.Text()
			hit := false
			for _, nd := range needles {
				if nd != "" && strings.Contains(text, nd) {
					hit = true
					break
				}
			}
			if !hit {
				return
			}
			anc := sel
			for i := 0; i < 4; i++ {
				if p := anc.Parent(); p.Length() > 0 {
					anc = p
				}
			}
			html, err := goquery.OuterHtml(anc)
			if err != nil || seen[html] {
				return
			}
			seen[html] = true
			parts = append(parts, html)
		})
	}
	joined := strings.Join(parts, "\n")
	if max := clean.MaxTokens * 4; len(joined) > max {
		if idx := strings.LastIndex(joined[:max], "\n"); idx > max/2 {
			return joined[:idx]
		}
		return joined[:max]
	}
	return joined
}
