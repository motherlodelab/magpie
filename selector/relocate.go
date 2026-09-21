package selector

import (
	"regexp"
	"strings"

	"github.com/motherlodelab/magpie/extract"

	"github.com/PuerkitoBio/goquery"
)

// ElementFP remembers what the element a cached selector matched looks
// like — structure, not text (the text is exactly what churns in a
// redesign). Persisted per field on SelectorDoc so relocation works in a
// later run, where heals actually fire.
type ElementFP struct {
	Tag         string            `json:"tag"`
	Classes     []string          `json:"classes,omitempty"`
	ID          string            `json:"id,omitempty"`
	Attrs       map[string]string `json:"attrs,omitempty"`
	Parent      string            `json:"parent,omitempty"`
	Grandparent string            `json:"grandparent,omitempty"`
	NumText     bool              `json:"num_text,omitempty"`
}

// fpAttrs is the attribute whitelist shared by fingerprinting and scoring
// (same list emitForSel emits candidate selectors for).
var fpAttrs = []string{"name", "itemprop", "data-testid", "data-test", "data-id"}

var numTextRe = regexp.MustCompile(`^[$€£]?-?\d[\d,.]*%?$`)

// numText reports whether s looks like a bare number (optionally signed,
// currency-prefixed, thousand-separated, percent-suffixed).
func numText(s string) bool { return numTextRe.MatchString(strings.TrimSpace(s)) }

// nodeSig is a compact structural signature: tag#id.class(es), layout
// classes filtered — the same shape emitForSel considers noise.
func nodeSig(s *goquery.Selection) string {
	node := s.Get(0)
	if node == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(node.Data)
	if id, ok := s.Attr("id"); ok && id != "" {
		b.WriteString("#" + id)
	}
	for _, c := range filteredClasses(s) {
		b.WriteString("." + c)
	}
	return b.String()
}

// filteredClasses returns the node's classes minus layout noise.
func filteredClasses(s *goquery.Selection) []string {
	cls, ok := s.Attr("class")
	if !ok {
		return nil
	}
	var out []string
	for _, c := range strings.Fields(cls) {
		if layoutClassRe.MatchString(c) || c == "clearfix" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// fingerprintSel captures the element sel matches as a fingerprint.
func fingerprintSel(doc *goquery.Document, sel FieldSelector, _ extract.FieldHints) (ElementFP, bool) {
	s := doc.Find(sel.Expr).First()
	if s.Length() == 0 {
		return ElementFP{}, false
	}
	return fingerprintNode(s), true
}

func fingerprintNode(s *goquery.Selection) ElementFP {
	fp := ElementFP{Tag: s.Get(0).Data}
	if id, ok := s.Attr("id"); ok && id != "" {
		fp.ID = id
	}
	fp.Classes = filteredClasses(s)
	for _, a := range fpAttrs {
		if v, ok := s.Attr(a); ok && v != "" {
			if fp.Attrs == nil {
				fp.Attrs = map[string]string{}
			}
			fp.Attrs[a] = v
		}
	}
	if p := s.Parent(); p.Length() > 0 {
		fp.Parent = nodeSig(p)
		if gp := p.Parent(); gp.Length() > 0 {
			fp.Grandparent = nodeSig(gp)
		}
	}
	fp.NumText = numText(s.Text())
	return fp
}

// Relocation scoring (max 14.5, normalized). ponytail: weights calibrated
// by hand against the product-A/B fixtures only — the ceiling is
// synthetic-redesign coverage; retune when a real-world mis-relocation
// shows up (upgrade path: per-component tuning corpus, healthy-field
// proximity anchor for list pages).
const (
	fpMaxScore  = 14.5
	fpThreshold = 0.65
	fpMargin    = 0.08
)

func jaccard(a, b []string) float64 {
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	inter := 0
	for _, s := range b {
		if set[s] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// scoreNode scores one candidate node against a field fingerprint,
// normalized to [0,1]. Pure function.
func scoreNode(n *goquery.Selection, fp ElementFP, field string, hints extract.FieldHints) float64 {
	node := n.Get(0)
	if node == nil {
		return 0
	}
	var score float64
	if node.Data == fp.Tag {
		score += 2
	}
	classes := filteredClasses(n)
	if len(classes) == 0 && len(fp.Classes) == 0 {
		score += 1.5
	} else {
		score += jaccard(classes, fp.Classes) * 3
	}
	if id, ok := n.Attr("id"); ok && id != "" && id == fp.ID {
		score += 1.5
	}
	attrs := map[string]bool{}
	for _, a := range fpAttrs {
		if v, ok := n.Attr(a); ok && v != "" {
			attrs[a] = true
		}
	}
	if len(attrs) == 0 && len(fp.Attrs) == 0 {
		score += 1
	} else if len(attrs) > 0 && len(fp.Attrs) > 0 {
		matched := 0
		for k, v := range fp.Attrs {
			if attrs[k] && n.AttrOr(k, "") == v {
				matched++
			}
		}
		score += float64(matched) / float64(len(fp.Attrs)) * 2
	}
	if p := n.Parent(); p.Length() > 0 && nodeSig(p) == fp.Parent {
		score += 2
	}
	if p := n.Parent(); p.Length() > 0 {
		if gp := p.Parent(); gp.Length() > 0 && nodeSig(gp) == fp.Grandparent {
			score += 1
		}
	}
	text := strings.TrimSpace(n.Text())
	if numText(text) == fp.NumText {
		score += 2
	} else {
		score -= 2
	}
	if _, ok := convertText(field, text, hints); ok {
		score += 1
	} else {
		score -= 1
	}
	if score < 0 {
		score = 0
	}
	return score / fpMaxScore
}

// Relocate re-finds field's element on new-template samples by fingerprint
// similarity and validates the replacement selector against ground truth
// already retained in the samples — zero LLM calls. Declines (false)
// whenever anything is missing or ambiguous; declining is normal control
// flow, never an error.
func Relocate(samples []SynthSample, field string, old FieldSelector, fp ElementFP, sch *extract.Schema) (FieldSelector, ElementFP, bool) {
	if old.Type != "css" || old.Expr == "" || fp.Tag == "" {
		return FieldSelector{}, ElementFP{}, false
	}
	type usable struct {
		sample SynthSample
		doc    *goquery.Document
		node   *goquery.Selection
	}
	var fresh []usable
	for _, s := range samples {
		if v, present := s.Truth[field]; !present || v == nil {
			continue
		}
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(s.HTML))
		if err != nil {
			continue
		}
		if _, hit := ExtractFieldValue(doc, s.Sidecar, field, old, sch.Hints); hit {
			continue // old template still serves here — nothing to relocate
		}
		best, second := -1.0, -1.0
		var bestNode *goquery.Selection
		doc.Find("*").Each(func(_ int, n *goquery.Selection) {
			sc := scoreNode(n, fp, field, sch.Hints)
			switch {
			case sc > best:
				second = best
				best = sc
				bestNode = n
			case sc > second:
				second = sc
			}
		})
		if bestNode == nil || best < fpThreshold || best-second < fpMargin {
			continue // no confident match on this sample
		}
		fresh = append(fresh, usable{sample: s, doc: doc, node: bestNode})
	}
	if len(fresh) < 2 {
		return FieldSelector{}, ElementFP{}, false
	}
	freshSamples := make([]SynthSample, len(fresh))
	for i, u := range fresh {
		freshSamples[i] = u.sample
	}
	seen := map[string]bool{}
	var cands []string
	for _, u := range fresh {
		for _, c := range emitForSel(u.node) {
			if !seen[c] {
				seen[c] = true
				cands = append(cands, c)
			}
		}
	}
	for _, c := range cands {
		sel := FieldSelector{Type: "css", Expr: c,
			Regex: sch.Hints.Regex[field], Coerce: sch.Hints.Coerce[field]}
		if hits, _ := FieldAgreement(freshSamples, field, sel, sch); hits < need(len(freshSamples)) {
			continue
		}
		agrees := true
		for _, u := range fresh { // FieldAgreement counts both-null as a hit — demand real values
			if v, hit := ExtractFieldValue(u.doc, u.sample.Sidecar, field, sel, sch.Hints); !hit || v == nil {
				agrees = false
				break
			}
		}
		if !agrees {
			continue
		}
		newFP, ok := fingerprintSel(fresh[len(fresh)-1].doc, sel, sch.Hints)
		if !ok {
			continue
		}
		return sel, newFP, true
	}
	return FieldSelector{}, ElementFP{}, false
}
