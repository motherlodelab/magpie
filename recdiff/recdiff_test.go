package recdiff

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func load(t *testing.T, s string, o Options) Set {
	t.Helper()
	set, err := Load(strings.NewReader(s), o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return set
}

// Added, changed and gone with the default url key.
func TestDiffURLKeys(t *testing.T) {
	prev := load(t, `
{"url":"https://a/1","price":"19.99","title":"One"}
{"url":"https://a/2","price":"19.99","title":"Two"}
{"url":"https://a/3","price":"9.99","title":"Three"}
`, Options{})
	cur := load(t, `
{"url":"https://a/1","price":"19.99","title":"One"}
{"url":"https://a/2","price":"17.99","title":"Two"}
{"url":"https://a/4","price":"5.99","title":"Four"}
`, Options{})
	res := Diff(prev, cur, Options{})
	if res.Added != 1 || res.Changed != 1 || res.Gone != 1 {
		t.Fatalf("counts: added=%d changed=%d gone=%d", res.Added, res.Changed, res.Gone)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("rows: got %d, want 3", len(res.Rows))
	}
	if res.Rows[0].Key != "https://a/4" || res.Rows[0].Change != "added" {
		t.Fatalf("row 0: %+v", res.Rows[0])
	}
	chg := res.Rows[1]
	if chg.Key != "https://a/2" || chg.Change != "changed" {
		t.Fatalf("row 1: %+v", chg)
	}
	if len(chg.Fields) != 1 || chg.Fields[0].Path != "price" ||
		chg.Fields[0].Old != "19.99" || chg.Fields[0].New != "17.99" {
		t.Fatalf("changed fields: %+v", chg.Fields)
	}
	if res.Rows[2].Key != "https://a/3" || res.Rows[2].Change != "gone" {
		t.Fatalf("row 2: %+v", res.Rows[2])
	}
	if res.Truncated || res.DupKeys != 0 || res.NoKey != 0 {
		t.Fatalf("meta: %+v", res)
	}
}

// The plan's fixture pair: +1 · ~1 · -1 with sku keys and a price change.
func TestFixturePair(t *testing.T) {
	prev := load(t, `
{"sku":"sku-1","price":"19.99","name":"Widget"}
{"sku":"sku-2","price":"19.99","name":"Gadget"}
{"sku":"sku-3","price":"9.99","name":"Mousetrap"}
`, Options{Key: "sku"})
	cur := load(t, `
{"sku":"sku-1","price":"19.99","name":"Widget"}
{"sku":"sku-2","price":"17.99","name":"Gadget"}
{"sku":"sku-4","price":"5.99","name":"Sprocket"}
`, Options{Key: "sku"})
	res := Diff(prev, cur, Options{})
	if res.Added != 1 || res.Changed != 1 || res.Gone != 1 {
		t.Fatalf("counts: %+v", res)
	}
	want := Row{Key: "sku-2", Change: "changed", Fields: []FieldChange{
		{Path: "price", Old: "19.99", New: "17.99"},
	}}
	b, merr := json.Marshal(res.Rows[1])
	if merr != nil {
		t.Fatal(merr)
	}
	var got Row
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != want.Key || got.Change != want.Change ||
		len(got.Fields) != 1 || got.Fields[0] != want.Fields[0] {
		t.Fatalf("changed row JSON: %s", b)
	}
}

// Items explodes an array; each element is a record keyed inside the element.
func TestItemsExplode(t *testing.T) {
	mk := func(items ...string) string {
		var b strings.Builder
		b.WriteString(`{"url":"https://a/catalog","extracted":{"products":[`)
		for i, it := range items {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(it)
		}
		b.WriteString(`]}}`)
		return b.String()
	}
	prev := load(t, mk(
		`{"sku":"sku-0","price":"5.00"}`,
		`{"sku":"sku-1","price":"10.00"}`,
		`{"sku":"sku-2","price":"20.00"}`), Options{Key: "sku", Items: "extracted.products"})
	cur := load(t, mk(
		`{"sku":"sku-1","price":"10.00"}`,
		`{"sku":"sku-2","price":"21.00"}`,
		`{"sku":"sku-3","price":"30.00"}`), Options{Key: "sku", Items: "extracted.products"})
	res := Diff(prev, cur, Options{})
	if res.Added != 1 || res.Changed != 1 || res.Gone != 1 {
		t.Fatalf("counts: added=%d changed=%d gone=%d", res.Added, res.Changed, res.Gone)
	}
	changed := res.Rows[1]
	if changed.Key != "sku-2" || changed.Fields[0].Path != "price" ||
		changed.Fields[0].Old != "20.00" || changed.Fields[0].New != "21.00" {
		t.Fatalf("changed: %+v", changed)
	}
}

// Duplicate keys are counted, never silently merged; keyless records counted.
func TestDupAndNoKey(t *testing.T) {
	prev := load(t, `
{"url":"https://a/1","x":"1"}
{"url":"https://a/1","x":"2"}
{"x":"3"}
"not an object"
`, Options{})
	if prev.DupKeys != 1 || prev.NoKey != 2 {
		t.Fatalf("dup=%d nokey=%d", prev.DupKeys, prev.NoKey)
	}
	cur := load(t, `{"url":"https://a/1","x":"2"}`, Options{})
	res := Diff(prev, cur, Options{})
	if res.DupKeys != 1 || res.NoKey != 2 {
		t.Fatalf("result meta: %+v", res)
	}
	if res.Added != 0 || res.Changed != 0 || res.Gone != 0 {
		t.Fatalf("counts should be clean: %+v", res)
	}
}

// Unseen keys and a partial run suppress "gone".
func TestUnseenAndNoGone(t *testing.T) {
	prev := load(t, `
{"url":"https://a/1"}
{"url":"https://a/2"}
{"url":"https://a/3"}
`, Options{})
	cur := load(t, `{"url":"https://a/1"}`, Options{})

	res := Diff(prev, cur, Options{Unseen: []string{"https://a/2"}})
	if res.Gone != 1 || res.Rows[0].Key != "https://a/3" {
		t.Fatalf("unseen should hide only a/2: %+v", res)
	}

	res = Diff(prev, cur, Options{NoGone: true})
	if res.Gone != 0 || len(res.Rows) != 0 {
		t.Fatalf("NoGone should hide all gone: %+v", res)
	}
}

// Fields over 1 KiB compare by sha256 and report Long with empty values.
func TestLongFields(t *testing.T) {
	long := strings.Repeat("word ", 300) // > 1 KiB
	prev := load(t, `{"url":"u","md":"`+long+` other"}`, Options{})
	cur := load(t, `{"url":"u","md":"`+long+` changed"}`, Options{})
	res := Diff(prev, cur, Options{})
	if res.Changed != 1 {
		t.Fatalf("want changed: %+v", res)
	}
	fc := res.Rows[0].Fields[0]
	if !fc.Long || fc.Old != "" || fc.New != "" || fc.Path != "md" {
		t.Fatalf("long field: %+v", fc)
	}
	// Same long text on both sides: unchanged despite size.
	same1 := load(t, `{"url":"u","md":"`+long+`"}`, Options{})
	same2 := load(t, `{"url":"u","md":"`+long+`"}`, Options{})
	if res := Diff(same1, same2, Options{}); res.Changed != 0 {
		t.Fatalf("identical long fields changed: %+v", res)
	}
}

// Ignore paths (and everything under them) never compare — honoured at
// Load (fields never stored) and again at Diff (belt and braces).
func TestIgnore(t *testing.T) {
	prev := load(t, `{"url":"u","depth":1,"meta":{"ts":"a","keep":"x"}}`, Options{})
	cur := load(t, `{"url":"u","depth":9,"meta":{"ts":"a","keep":"x"}}`, Options{})
	if res := Diff(prev, cur, Options{Ignore: []string{"depth"}}); res.Changed != 0 {
		t.Fatalf("depth alone should not change: %+v", res)
	}
	if res := Diff(prev, cur, Options{}); res.Changed != 1 {
		t.Fatalf("without Ignore the run must change: %+v", res)
	}
	res := Diff(prev, cur, Options{Ignore: []string{"depth", "meta.ts"}})
	if res.Changed != 0 {
		t.Fatalf("nested ignore failed: %+v", res)
	}
	// Load-time filtering keeps ignored fields out of the set entirely.
	prevL := load(t, `{"url":"u","depth":1,"keep":"x"}`, Options{Ignore: []string{"depth"}})
	curL := load(t, `{"url":"u","depth":9,"keep":"x"}`, Options{Ignore: []string{"depth"}})
	if res := Diff(prevL, curL, Options{}); res.Changed != 0 || len(res.Rows) != 0 {
		t.Fatalf("load-time ignore failed: %+v", res)
	}
}

// Strings collapse whitespace; numbers compare as numbers; types stay apart.
func TestCanonicalisation(t *testing.T) {
	prev := load(t, `{"url":"u","s":"  hello\n\tworld  ","p":19.9}`, Options{})
	cur := load(t, `{"url":"u","s":"hello world","p":19.90}`, Options{})
	if res := Diff(prev, cur, Options{}); res.Changed != 0 {
		t.Fatalf("canonical equal must be unchanged: %+v", res)
	}
	// String "19.99" is not number 19.99.
	prev = load(t, `{"url":"u","p":"19.99"}`, Options{})
	cur = load(t, `{"url":"u","p":19.99}`, Options{})
	if res := Diff(prev, cur, Options{}); res.Changed != 1 {
		t.Fatalf("type change must be detected: %+v", res)
	}
	// Big integer ids beyond float64 exact range stay literal.
	prev = load(t, `{"url":"u","id":9007199254749935}`, Options{})
	cur = load(t, `{"url":"u","id":9007199254749936}`, Options{})
	if res := Diff(prev, cur, Options{}); res.Changed != 1 {
		t.Fatalf("big int ids must not collide: %+v", res)
	}
}

// A JSON array loads like JSONL.
func TestLoadJSONArray(t *testing.T) {
	arr := `[{"url":"a","x":1},{"url":"b","x":2}]`
	set, err := Load(strings.NewReader(arr), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.recs) != 2 {
		t.Fatalf("recs: %d", len(set.recs))
	}
}

// Rows cap at MaxRows, added first, then changed, then gone; counts stay exact.
func TestMaxRowsAndOrder(t *testing.T) {
	var prevB, curB strings.Builder
	for i := 0; i < 10; i++ {
		prevB.WriteString(`{"url":"u` + itoa(i) + `"}`) // all 10 will be changed
		curB.WriteString(`{"url":"u` + itoa(i) + `","v":1}`)
	}
	prevB.WriteString(`{"url":"gone-1"}`)
	curB.WriteString(`{"url":"added-1"}`)
	prev := load(t, prevB.String(), Options{})
	cur := load(t, curB.String(), Options{})
	res := Diff(prev, cur, Options{MaxRows: 5})
	if res.Added != 1 || res.Changed != 10 || res.Gone != 1 {
		t.Fatalf("counts must stay exact: %+v", res)
	}
	if len(res.Rows) != 5 || !res.Truncated {
		t.Fatalf("rows capped: %d truncated=%v", len(res.Rows), res.Truncated)
	}
	// added first, then changed sorted by key, then gone.
	if res.Rows[0].Key != "added-1" || res.Rows[0].Change != "added" {
		t.Fatalf("first row: %+v", res.Rows[0])
	}
	if res.Rows[1].Key != "u0" || res.Rows[1].Change != "changed" {
		t.Fatalf("second row: %+v", res.Rows[1])
	}
}

// Nested objects flatten to dotted paths; whole arrays canonicalize.
func TestNestedAndArrays(t *testing.T) {
	prev := load(t, `{"url":"u","extracted":{"price":"1.00","tags":["a","b"]}}`, Options{})
	cur := load(t, `{"url":"u","extracted":{"price":"2.00","tags":["a","b"]}}`, Options{})
	res := Diff(prev, cur, Options{})
	if res.Changed != 1 || res.Rows[0].Fields[0].Path != "extracted.price" {
		t.Fatalf("nested path: %+v", res)
	}
	if res.Rows[0].Fields[0].Old != "1.00" || res.Rows[0].Fields[0].New != "2.00" {
		t.Fatalf("string leaf as-is: %+v", res.Rows[0].Fields[0])
	}
	cur = load(t, `{"url":"u","extracted":{"price":"1.00","tags":["a", "c"]}}`, Options{})
	res = Diff(prev, cur, Options{})
	if res.Changed != 1 || res.Rows[0].Fields[0].Path != "extracted.tags" {
		t.Fatalf("array compare: %+v", res)
	}
	if res.Rows[0].Fields[0].Old != `["a","b"]` || res.Rows[0].Fields[0].New != `["a","c"]` {
		t.Fatalf("array canonical JSON: %+v", res.Rows[0].Fields[0])
	}
}

// Result marshals with the Go field names the plan's condition pins.
func TestResultJSONShape(t *testing.T) {
	prev := load(t, `{"sku":"sku-2","price":"19.99"}`, Options{Key: "sku"})
	cur := load(t, `{"sku":"sku-2","price":"17.99"}`, Options{Key: "sku"})
	b, err := json.Marshal(Diff(prev, cur, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"Added":0`, `"Changed":1`, `"Gone":0`, `"Truncated":false`, `"DupKeys":0`, `"NoKey":0`,
		`"Key":"sku-2"`, `"Change":"changed"`, `"Path":"price"`, `"Old":"19.99"`, `"New":"17.99"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
}

// FieldChange values cap at 256 bytes without splitting a rune.
func TestValueCap(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes, 200 runes
	prev := load(t, `{"url":"u","v":"a`+long+`"}`, Options{})
	cur := load(t, `{"url":"u","v":"b`+long+`"}`, Options{})
	res := Diff(prev, cur, Options{})
	fc := res.Rows[0].Fields[0]
	if len(fc.Old) > 256 || len(fc.New) > 256 {
		t.Fatalf("cap: %d %d", len(fc.Old), len(fc.New))
	}
	if !strings.HasPrefix(fc.Old, "a") || !strings.HasPrefix(fc.New, "b") || !utf8.ValidString(fc.Old) {
		t.Fatalf("rune cut: %q %q", fc.Old, fc.New)
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func BenchmarkLoad10k(b *testing.B) {
	var sb strings.Builder
	for i := 0; i < 10000; i++ {
		sb.WriteString(`{"url":"https://example.com/page/` + itoa(i) + `","title":"Page ` + itoa(i) + `","depth":` + itoa(i%5) + `,"price":"` + itoa(i) + `.99"}` + "\n")
	}
	src := sb.String()
	o := Options{Ignore: []string{"depth"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Load(strings.NewReader(src), o); err != nil {
			b.Fatal(err)
		}
	}
}
