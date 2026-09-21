// Package vertical holds zero-LLM typed extractors: registry JSON APIs +
// HTML/API fast paths. Leaf package: it may import fetch (types) and clean
// (HarvestSidecar, PlayerResponseHTML); nothing in vertical imports
// scrape/cli/mcp, and clean must never import vertical (cycle).
package vertical

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/motherlodelab/magpie/fetch"
)

// ErrNoMatch marks a URL no strict extractor handles (auto-dispatch miss).
var ErrNoMatch = errors.New("no vertical extractor matches")

// ErrURLMismatch marks an explicit --vertical name that does not handle the URL.
var ErrURLMismatch = errors.New("vertical extractor does not handle URL")

// Fetcher is the single-method fetch surface extractors need. Satisfied by
// *fetch.StaticFetcher directly (one-way import: fetch never imports vertical).
type Fetcher interface {
	Fetch(ctx context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error)
}

var _ Fetcher = (*fetch.StaticFetcher)(nil)

// Info describes one extractor for List() output (Phase C renders these).
type Info struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Desc     string   `json:"desc"`
	Patterns []string `json:"patterns"` // example URL shapes, human-readable only — never regexes
}

// Extractor is one zero-LLM typed extractor.
type Extractor struct {
	Info    Info
	Match   func(u *url.URL) bool
	Extract func(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error)
	OptIn   bool // true = explicit-only, skipped by MatchURL
}

// registry is the extractor set: built-ins self-register via init();
// embedders add more via Register. Writes happen at startup, before any
// serving — no mutex.
var registry []Extractor

func register(e Extractor) {
	registry = append(registry, e)
}

// Register adds a custom extractor to the registry (exported for
// embedders — the desktop app registers proprietary verticals at
// startup). Duplicate names are rejected: Lookup is name-addressed.
func Register(e Extractor) error {
	if e.Info.Name == "" || e.Match == nil || e.Extract == nil {
		return fmt.Errorf("vertical: register: Name, Match and Extract are required")
	}
	if _, exists := Lookup(e.Info.Name); exists {
		return fmt.Errorf("vertical: extractor %q already registered", e.Info.Name)
	}
	registry = append(registry, e)
	return nil
}

// List returns every extractor's Info, registry order.
func List() []Info {
	out := make([]Info, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.Info)
	}
	return out
}

// Lookup resolves an extractor by name.
func Lookup(name string) (Extractor, bool) {
	for _, e := range registry {
		if e.Info.Name == name {
			return e, true
		}
	}
	return Extractor{}, false
}

// MatchURL returns the first strict (non-OptIn) extractor handling rawURL.
// Permissive OptIn extractors never auto-fire — explicit Lookup only.
func MatchURL(rawURL string) (Extractor, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return Extractor{}, false
	}
	for _, e := range registry {
		if e.OptIn {
			continue
		}
		if e.Match(u) {
			return e, true
		}
	}
	return Extractor{}, false
}

// fetchBytes GETs rawURL and returns the body; non-2xx is a hard error
// naming the status (never a silent fallback — fail loudly).
func fetchBytes(ctx context.Context, f Fetcher, rawURL string) ([]byte, error) {
	resp, err := f.Fetch(ctx, fetch.FetchRequest{URL: rawURL})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("vertical: GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return resp.HTML, nil
}

// fetchJSON GETs rawURL and unmarshals the body into a generic map.
func fetchJSON(ctx context.Context, f Fetcher, rawURL string) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, rawURL)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("vertical: GET %s: decode: %w", rawURL, err)
	}
	return m, nil
}

// child walks nested maps by key path; nil when any hop misses.
func child(m map[string]any, keys ...string) map[string]any {
	cur := m
	for _, k := range keys {
		next, _ := cur[k].(map[string]any)
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// str picks the first present non-empty string along the key paths.
func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return ""
}

// num picks a numeric field as float64; strings parse, anything else is 0.
func num(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if n := numVal(m[k]); n != 0 {
			return n
		}
	}
	return 0
}

// numVal coerces a decoded JSON value to float64 (0 when not numeric).
func numVal(v any) float64 {
	switch v := v.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return 0
}

// firstJSONArray unmarshals a top-level JSON array body and returns its
// first object element (GitHub releases list, Reddit .json listing).
func firstJSONArray(body []byte) (map[string]any, error) {
	var arr []any
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("vertical: decode array: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("vertical: empty array response")
	}
	m, _ := arr[0].(map[string]any)
	if m == nil {
		return nil, fmt.Errorf("vertical: array element not an object")
	}
	return m, nil
}

// anyMap type-asserts a decoded JSON value to an object (nil when not).
func anyMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// matchHTTP is the maximally permissive OptIn matcher shared by the
// JSON-LD standards extractors: any http(s) URL might carry the block.
func matchHTTP(u *url.URL) bool {
	return u.Scheme == "http" || u.Scheme == "https"
}

// hostIs folds the URL host for canonical-host checks.
func hostIs(u *url.URL, hosts ...string) bool {
	h := strings.ToLower(u.Hostname())
	for _, want := range hosts {
		if h == want {
			return true
		}
	}
	return false
}

// pathSegs splits a URL path into non-empty segments. Single home for
// path-shape matching (github kinds, reddit permalinks, arxiv, handles).
func pathSegs(p string) []string {
	raw := strings.Split(strings.Trim(p, "/"), "/")
	out := raw[:0]
	for _, s := range raw {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// lastSegment returns the last non-empty path segment (registry names,
// handles, IDs).
func lastSegment(p string) string {
	segs := pathSegs(p)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}
