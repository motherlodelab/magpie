package selector

// Unit tests for the unexported relocation scoring helpers. Internal test
// package (the external selector_test.go helpers are invisible here) and
// fully self-contained: inline literals only, no fixtures.

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/extract"

	"github.com/PuerkitoBio/goquery"
)

const weightSchemaYAML = `$schema: "https://json-schema.org/draft/2020-12/schema"
type: object
additionalProperties: false
required: [price]
properties:
  price: {type: number, x-magpie: {coerce: "eur_decimal"}}
`

func weightHints(t *testing.T) extract.FieldHints {
	t.Helper()
	sch, err := extract.ParseSchema([]byte(weightSchemaYAML))
	if err != nil {
		t.Fatal(err)
	}
	return sch.Hints
}

func parsePage(t *testing.T, html string) *goquery.Document {
	t.Helper()
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestScoreNode_Weights(t *testing.T) {
	hints := weightHints(t)

	// (a) A node scored against its own fingerprint is a perfect match.
	page := parsePage(t, `<html><body><main><div class="buybox"><span id="p" class="amt" data-testid="price">12.99</span></div></main></body></html>`)
	node := page.Find("#p")
	if got := scoreNode(node, fingerprintNode(node), "price", hints); math.Abs(got-1) > 1e-9 {
		t.Errorf("identical node score = %v, want 1.0", got)
	}

	// (b) The true node outscores its "Price:" label by a wide margin —
	//     numeric agreement + convert penalty, the NumText decision.
	fp := fingerprintNode(parsePage(t, `<html><body><main><div class="buybox"><span>Price:</span> <span id="price">12.99</span></div></main></body></html>`).Find("#price"))
	newPage := parsePage(t, `<html><body><main><div class="buybox"><span>Price:</span> <span id="cost-now">12.99</span></div></main></body></html>`)
	trueScore := scoreNode(newPage.Find("#cost-now"), fp, "price", hints)
	labelScore := scoreNode(newPage.Find("div.buybox span").First(), fp, "price", hints)
	if gap := trueScore - labelScore; gap < 0.3 {
		t.Errorf("label gap = %v, want ≥ 0.3 (true=%v label=%v)", gap, trueScore, labelScore)
	}
	if labelScore >= 0.4 {
		t.Errorf("label score = %v, want < 0.4", labelScore)
	}

	// (c) Partial class overlap outscores no overlap (Jaccard).
	fp = fingerprintNode(parsePage(t, `<html><body><main><div class="buybox"><span id="price" class="price-tag bold">12.99</span></div></main></body></html>`).Find("#price"))
	partial := scoreNode(parsePage(t, `<html><body><main><div class="buybox"><span id="price" class="price-tag big">12.99</span></div></main></body></html>`).Find("#price"), fp, "price", hints)
	unrelated := scoreNode(parsePage(t, `<html><body><main><div class="buybox"><span id="price" class="unrelated">12.99</span></div></main></body></html>`).Find("#price"), fp, "price", hints)
	if partial <= unrelated {
		t.Errorf("partial class overlap %v should outscore none %v", partial, unrelated)
	}

	// (d) Layout classes never distinguish a parent.
	fp = fingerprintNode(parsePage(t, `<html><body><main><div class="buybox"><span id="price">12.99</span></div></main></body></html>`).Find("#price"))
	withLayout := scoreNode(parsePage(t, `<html><body><main><div class="row-2 buybox"><span id="price">12.99</span></div></main></body></html>`).Find("#price"), fp, "price", hints)
	withoutLayout := scoreNode(parsePage(t, `<html><body><main><div class="buybox"><span id="price">12.99</span></div></main></body></html>`).Find("#price"), fp, "price", hints)
	if withLayout != withoutLayout {
		t.Errorf("layout class changed the score: %v vs %v", withLayout, withoutLayout)
	}

	// (e) An adversarial node (numeric text vs non-numeric fp) clamps in [0,1].
	fp = fingerprintNode(parsePage(t, `<html><body><span id="t">Widget</span></body></html>`).Find("#t"))
	if adv := scoreNode(parsePage(t, `<html><body><span id="t">12.99</span></body></html>`).Find("#t"), fp, "title", hints); adv < 0 || adv > 1 {
		t.Errorf("adversarial score = %v, want within [0,1]", adv)
	}
}

func TestNumTextAndNodeSig(t *testing.T) {
	for _, s := range []string{"12.99", "$1,299.00", "€12,99", "12%", "-3", "4001234567890"} {
		if !numText(s) {
			t.Errorf("numText(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "Price:", "12 dollars", "N/A"} {
		if numText(s) {
			t.Errorf("numText(%q) = true, want false", s)
		}
	}
	doc := parsePage(t, `<html><body><div class="row-2 buybox"><div class="col-12"><span id="cost-now">12.99</span></div></div></body></html>`)
	for _, tc := range []struct{ sel, want string }{
		{"div.buybox", "div.buybox"}, // row-2 is layout noise
		{"div.col-12", "div"},        // col-12 is layout noise
		{"#cost-now", "span#cost-now"},
	} {
		if got := nodeSig(doc.Find(tc.sel)); got != tc.want {
			t.Errorf("nodeSig(%s) = %q, want %q", tc.sel, got, tc.want)
		}
	}
	fp, ok := fingerprintSel(doc, FieldSelector{Type: "css", Expr: "#cost-now"}, extract.FieldHints{})
	if !ok || fp.Tag != "span" || fp.ID != "cost-now" || !fp.NumText || fp.Parent != "div" || fp.Grandparent != "div.buybox" {
		t.Errorf("fingerprintSel hit = %+v ok=%v", fp, ok)
	}
	if _, ok := fingerprintSel(doc, FieldSelector{Type: "css", Expr: "#nope"}, extract.FieldHints{}); ok {
		t.Error("fingerprintSel(#nope) = true, want false")
	}
}
