package selector_test

// Relocation tests (Phase R): the money path proves the zero-LLM property
// by counter; the decline paths prove guessing never happens. The unit
// tests for the unexported scoring helpers live in
// relocate_internal_test.go (Go keeps the two test packages separate).

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/selector"

	"github.com/PuerkitoBio/goquery"
)

// aabbSamples builds the redesign scenario: two old-template samples (the
// cached selector still works) followed by two new-template ones (it
// nulls). Truth rides in the samples — the pipeline already paid for it.
func aabbSamples(t *testing.T) []selector.SynthSample {
	t.Helper()
	a := loadSelectorFixture(t, "product-A.html")
	b := loadSelectorFixture(t, "product-B.html")
	return []selector.SynthSample{
		{URL: "http://ex.com/a1", HTML: a, Truth: testTruth},
		{URL: "http://ex.com/a2", HTML: a, Truth: testTruth},
		{URL: "http://ex.com/b1", HTML: b, Truth: testTruth},
		{URL: "http://ex.com/b2", HTML: b, Truth: testTruth},
	}
}

func synthADoc(t *testing.T, calls *int) selector.SelectorDoc {
	t.Helper()
	doc, err := selector.Synthesize(context.Background(), threeSamples(t, "product-A.html"), mustParseTestSchema(t),
		cannedPropose(t, nil, calls), nil)
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	return doc
}

func TestRelocate_RenamesIDKeepsStructure(t *testing.T) {
	sch := mustParseTestSchema(t)
	calls := 0

	// 1. Synthesize on product-A (the old template). The free paths must
	//    win with zero proposals — otherwise the counter below is pre-poisoned.
	base := synthADoc(t, &calls)
	if calls != 0 {
		t.Fatalf("synthesize made %d propose calls; fixture must synth for free", calls)
	}
	fp, ok := base.Fingerprints["price"]
	if !ok {
		t.Fatal("no fingerprint for price after synth")
	}
	if base.EngineVersion != 2 {
		t.Errorf("EngineVersion = %d, want 2", base.EngineVersion)
	}

	sel, newFP, ok := selector.Relocate(aabbSamples(t), "price", base.Fields["price"], fp, sch)
	if !ok {
		t.Fatal("Relocate declined the A→B redesign; want success")
	}
	if calls != 0 { // THE assertion: zero LLM through the whole path
		t.Errorf("propose calls = %d, want 0 (relocation must be free)", calls)
	}

	// 2. The winner must extract truth-exact on every new-template sample.
	for _, s := range aabbSamples(t)[2:] {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(s.HTML))
		if err != nil {
			t.Fatal(err)
		}
		got, found := selector.ExtractFieldValue(doc, s.Sidecar, "price", sel, sch.Hints)
		if !found || got == nil {
			t.Fatalf("relocated selector %q null on %s", sel.Expr, s.URL)
		}
		if selector.CanonicalJSON(got) != selector.CanonicalJSON(12.99) {
			t.Errorf("extracted %v, want 12.99", got)
		}
	}

	// 3. The returned fingerprint tracks the element the NEW selector matches.
	if !newFP.NumText {
		t.Error("newFP.NumText = false, want true (12.99 is numeric)")
	}
	if !strings.Contains(newFP.Parent, "buybox") {
		t.Errorf("newFP.Parent = %q, want it to contain buybox", newFP.Parent)
	}
}

func TestRelocate_Declines_NoFingerprint(t *testing.T) {
	var calls int
	base := synthADoc(t, &calls)
	sel, fp, ok := selector.Relocate(aabbSamples(t), "price", base.Fields["price"], selector.ElementFP{}, mustParseTestSchema(t))
	if ok {
		t.Errorf("relocated with a zero-value fingerprint as %q", sel.Expr)
	}
	if sel != (selector.FieldSelector{}) {
		t.Errorf("decline selector = %+v, want zero value", sel)
	}
	if !reflect.DeepEqual(fp, selector.ElementFP{}) {
		t.Errorf("decline fingerprint = %+v, want zero value", fp)
	}
}

func TestRelocate_Declines_NoNewSamples(t *testing.T) {
	var calls int
	base := synthADoc(t, &calls)
	// All-A: the old selector still works everywhere — nothing to relocate.
	if _, _, ok := selector.Relocate(threeSamples(t, "product-A.html"), "price", base.Fields["price"], base.Fingerprints["price"], mustParseTestSchema(t)); ok {
		t.Error("relocated with no broken samples; want decline")
	}
}

func TestRelocate_Declines_Ambiguous(t *testing.T) {
	// Two isomorphic candidates: identical sigs ⇒ margin 0 ⇒ the guard,
	// not luck, must decline.
	html := `<html><body><main><div class="offer"><span class="amt">5.00</span></div><div class="offer"><span class="amt">7.00</span></div></main></body></html>`
	truth := map[string]any{"price": 12.99}
	samples := []selector.SynthSample{
		{URL: "http://ex.com/1", HTML: html, Truth: truth},
		{URL: "http://ex.com/2", HTML: html, Truth: truth},
	}
	fp := selector.ElementFP{Tag: "span", Classes: []string{"amt"}, Parent: "div.offer", Grandparent: "main", NumText: true}
	sel, gotFP, ok := selector.Relocate(samples, "price", selector.FieldSelector{Type: "css", Expr: "#amount"}, fp, mustParseTestSchema(t))
	if ok {
		t.Errorf("accepted an ambiguous page as %q", sel.Expr)
	}
	if sel != (selector.FieldSelector{}) || !reflect.DeepEqual(gotFP, selector.ElementFP{}) {
		t.Errorf("decline values = %+v, %+v; want zero values", sel, gotFP)
	}
}

func TestFingerprint_SynthesizeWritesParity(t *testing.T) {
	var calls int
	doc := synthADoc(t, &calls)
	if doc.EngineVersion != 2 {
		t.Errorf("EngineVersion = %d, want 2", doc.EngineVersion)
	}
	if len(doc.Fingerprints) == 0 {
		t.Fatal("no fingerprints written")
	}
	for f := range doc.Fields {
		if _, ok := doc.Fingerprints[f]; !ok {
			t.Errorf("css field %q has no fingerprint", f)
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back selector.SelectorDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, back) {
		t.Error("JSON round-trip changed the doc")
	}

	// jsonld fields ride the sidecar — no fingerprint, no entry.
	jsonldSch, err := extract.ParseSchema([]byte(`$schema: "https://json-schema.org/draft/2020-12/schema"
type: object
additionalProperties: false
required: [price, ean]
properties:
  price: {type: number, x-magpie: {coerce: "eur_decimal"}}
  ean: {type: string, x-magpie: {jsonld_path: "$.gtin13"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	samples := threeSamples(t, "product-A.html")
	for i := range samples {
		samples[i].Sidecar = json.RawMessage(`{"gtin13":"4001234567890"}`)
	}
	doc2, err := selector.Synthesize(context.Background(), samples, jsonldSch, cannedPropose(t, nil, &calls), nil)
	if err != nil {
		t.Fatalf("Synthesize(jsonld): %v", err)
	}
	if doc2.Fields["ean"].Type != "jsonld" {
		t.Errorf("ean type = %q, want jsonld", doc2.Fields["ean"].Type)
	}
	if _, ok := doc2.Fingerprints["ean"]; ok {
		t.Error("jsonld field must not be fingerprinted")
	}
	if _, ok := doc2.Fingerprints["price"]; !ok {
		t.Error("css price missing fingerprint")
	}
}
