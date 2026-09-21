package selector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/motherlodelab/magpie/extract"

	"github.com/PuerkitoBio/goquery"
)

// SynthSample is one validated sample page: raw HTML + sidecar + the
// direct-LLM ground truth record (logged Purpose:"synth" by the caller).
type SynthSample struct {
	URL     string
	HTML    string
	Sidecar json.RawMessage
	Truth   map[string]any
}

// FieldSelector is one cached field extractor (§4.4 doc format).
type FieldSelector struct {
	Type     string  `json:"type"` // css | jsonld
	Expr     string  `json:"expr"`
	Regex    string  `json:"regex,omitempty"`
	Coerce   string  `json:"coerce,omitempty"`
	NullRate float64 `json:"null_rate"`
}

// SelectorDoc is the cached §4.4 document.
type SelectorDoc struct {
	SchemaHash    string                   `json:"schema_hash"`
	Domain        string                   `json:"domain"`
	Fields        map[string]FieldSelector `json:"fields"`
	Fingerprints  map[string]ElementFP     `json:"fingerprints,omitempty"`
	SynthesizedAt string                   `json:"synthesized_at"`
	SamplesUsed   int                      `json:"samples_used"`
	EngineVersion int                      `json:"engine_version"`
}

// SchemaHash is hex(SHA-256(json.Marshal(sch.Raw))). json.Marshal sorts map
// keys, and Raw is already the yamlToJSON round-trip, so this is stable.
func SchemaHash(sch *extract.Schema) string {
	raw, err := json.Marshal(sch.Raw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SchemaFields returns top-level property names, sorted.
func SchemaFields(sch *extract.Schema) []string {
	obj, ok := sch.Raw.(map[string]any)
	if !ok {
		return nil
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for name := range props {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HasXPathHint reports whether the schema mentions xpath_hint anywhere
// (CSS-only engine: warn once, ignore).
func HasXPathHint(sch *extract.Schema) bool {
	raw, err := json.Marshal(sch.Raw)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), "xpath_hint")
}

// ExtractFieldValue applies one field selector to an already-parsed document.
func ExtractFieldValue(doc *goquery.Document, sidecar json.RawMessage, field string, sel FieldSelector, hints extract.FieldHints) (any, bool) {
	if sel.Type == "jsonld" || sel.Type == "" {
		if path, ok := hints.JSONLDPath[field]; ok && path != "" {
			v, found := extract.JSONLDWalk(sidecar, path)
			return v, found
		}
		if sel.Type == "jsonld" {
			if v, found := extract.JSONLDWalk(sidecar, sel.Expr); found {
				return v, true
			}
			return nil, false
		}
	}
	if sel.Expr == "" {
		return nil, false
	}
	if hints.Multiple[field] {
		var vals []any
		doc.Find(sel.Expr).Each(func(_ int, s *goquery.Selection) {
			if v, ok := convertText(field, strings.TrimSpace(s.Text()), hints); ok {
				vals = append(vals, v)
			}
		})
		if len(vals) == 0 {
			return nil, false
		}
		return vals, true
	}
	sel2 := doc.Find(sel.Expr).First()
	if sel2.Length() == 0 {
		return nil, false
	}
	return convertText(field, strings.TrimSpace(sel2.Text()), hints)
}

func convertText(field, text string, hints extract.FieldHints) (any, bool) {
	if text == "" {
		return nil, false
	}
	v := any(text)
	if pat, ok := hints.Regex[field]; ok && pat != "" {
		nv, err := extract.ApplyRegex(pat, text)
		if err != nil {
			return nil, false
		}
		text, v = nv, nv
	}
	if kind, ok := hints.Coerce[field]; ok && kind != "" && kind != "regex" {
		nv, err := extract.Coerce(kind, text)
		if err != nil {
			return nil, false
		}
		v = nv
	} else if hints.Trim[field] {
		v = strings.TrimSpace(text)
	}
	return v, true
}

// CanonicalJSON renders a value for ground-truth comparison.
func CanonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// FieldAgreement runs sel against every sample; returns hits/total where a
// hit means extracted-canonical == truth-canonical (both-null counts).
func FieldAgreement(samples []SynthSample, field string, sel FieldSelector, sch *extract.Schema) (int, int) {
	hits := 0
	for _, s := range samples {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(s.HTML)))
		if err != nil {
			continue
		}
		got, ok := ExtractFieldValue(doc, s.Sidecar, field, sel, sch.Hints)
		want, present := s.Truth[field]
		if !ok || got == nil {
			if !present || want == nil {
				hits++ // both null
			}
			continue
		}
		if present && want != nil && CanonicalJSON(got) == CanonicalJSON(want) {
			hits++
		}
	}
	return hits, len(samples)
}

func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}
