package vertical

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/motherlodelab/magpie/clean"

	"github.com/PuerkitoBio/goquery"
)

// JSON-LD block harvesting shared by the standards extractors
// (ecommerce_product, job_posting, event, local_business, article).

// blocksOfType returns candidate JSON-LD blocks mentioning typ. See
// blocksOfTypes (the plural form is the real entry point).
func blocksOfType(html []byte, typ string) []json.RawMessage {
	return blocksOfTypes(html, typ)
}

// blocksOfTypes harvests JSON-LD blocks two-stage: clean.HarvestSidecar
// first; a raw script brace-scan only when the sidecar is absent.
// Inherited commerce quirk (keep until a real page misses): a page whose
// sidecar exists but lacks the type is a miss — the fallback never runs.
func blocksOfTypes(html []byte, types ...string) []json.RawMessage {
	sidecar := clean.HarvestSidecar(html)
	if len(sidecar) != 0 {
		return sidecarBlocks(sidecar)
	}
	return scriptScanBlocks(html, types...)
}

// sidecarBlocks normalizes HarvestSidecar's array-or-single shape into a
// plain list.
func sidecarBlocks(sidecar []byte) []json.RawMessage {
	var arr []json.RawMessage
	if err := json.Unmarshal(sidecar, &arr); err == nil {
		return arr
	}
	var single json.RawMessage
	if err := json.Unmarshal(sidecar, &single); err == nil {
		return []json.RawMessage{single}
	}
	return nil
}

// scriptScanBlocks is the fallback for JSON-LD the harvest missed
// (nonstandard type-attribute spellings): balanced-brace objects whose
// text mentions any of types.
func scriptScanBlocks(html []byte, types ...string) []json.RawMessage {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
	if err != nil {
		return nil
	}
	var blocks []json.RawMessage
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t == "" || !containsAny(t, types) {
			return
		}
		for _, frag := range clean.BraceObjects(t) {
			if json.Valid([]byte(frag)) && containsAny(frag, types) {
				blocks = append(blocks, json.RawMessage(frag))
			}
		}
	})
	return blocks
}

// firstTypedBlock returns the first block whose @type contains one of
// types (string or array spellings), decoded.
func firstTypedBlock(html []byte, types ...string) (map[string]any, bool) {
	for _, block := range blocksOfTypes(html, types...) {
		var m map[string]any
		if err := json.Unmarshal(block, &m); err != nil {
			continue
		}
		if typedAny(m["@type"], types) {
			return m, true
		}
	}
	return nil, false
}

// typedAny matches a JSON-LD @type value (string or array) against type
// substrings — the isProductType shape, parameterized.
func typedAny(t any, types []string) bool {
	switch v := t.(type) {
	case string:
		return containsAny(v, types)
	case []any:
		for _, e := range v {
			if s, _ := e.(string); containsAny(s, types) {
				return true
			}
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// matchHTTP is the maximally permissive OptIn matcher shared by the
// JSON-LD standards extractors: any http(s) URL might carry the block.
func matchHTTP(u *url.URL) bool {
	return u.Scheme == "http" || u.Scheme == "https"
}

// blockArray normalizes a JSON-LD value that may be a single object or an
// array of objects into []any (nil when absent or scalar).
func blockArray(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		return []any{t}
	}
	return nil
}

// nameList normalizes a scalar-or-array JSON-LD value whose elements are
// plain strings or {name} objects into []string (order preserved).
func nameList(v any) []string {
	var out []string
	add := func(e any) {
		switch ev := e.(type) {
		case string:
			out = append(out, ev)
		case map[string]any:
			if n := str(ev, "name"); n != "" {
				out = append(out, n)
			}
		}
	}
	if arr, _ := v.([]any); arr != nil {
		for _, e := range arr {
			add(e)
		}
		return out
	}
	add(v)
	return out
}

// flatAddress flattens a schema.org PostalAddress into the fixed key set;
// empty parts are omitted entirely (omission over guessing).
func flatAddress(addr map[string]any) map[string]any {
	out := map[string]any{}
	if v := str(addr, "streetAddress"); v != "" {
		out["street"] = v
	}
	if v := str(addr, "addressLocality"); v != "" {
		out["locality"] = v
	}
	if v := str(addr, "addressRegion"); v != "" {
		out["region"] = v
	}
	if v := str(addr, "postalCode"); v != "" {
		out["postalCode"] = v
	}
	if c := addressCountry(addr); c != "" {
		out["country"] = c
	}
	return out
}

// addressCountry handles addressCountry as string or {name} object.
func addressCountry(addr map[string]any) string {
	if s := str(addr, "addressCountry"); s != "" {
		return s
	}
	return str(anyMap(addr["addressCountry"]), "name")
}
