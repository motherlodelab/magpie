package vertical

import (
	"net/url"
	"testing"
)

func parseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

func ldJSON(t *testing.T, inner string) []byte {
	t.Helper()
	return []byte(`<html><head><script type="application/ld+json">` + inner + `</script></head><body>x</body></html>`)
}

func TestBlocksOfType_SidecarPath(t *testing.T) {
	t.Parallel()
	html := ldJSON(t, `{"@context":"https://schema.org","@type":"JobPosting","title":"A"}`)
	blocks := blocksOfTypes(html, "JobPosting")
	if len(blocks) != 1 {
		t.Fatalf("blocksOfType = %d blocks, want 1", len(blocks))
	}
	m, ok := firstTypedBlock(html, "JobPosting")
	if !ok || m["title"] != "A" {
		t.Errorf("firstTypedBlock = (%v, %v), want title A", m, ok)
	}
}

func TestBlocksOfType_BraceScanFallback(t *testing.T) {
	t.Parallel()
	// Nonstandard type spelling blinds HarvestSidecar; the raw script
	// brace-scan must still find the block (two-stage behavior preserved).
	html := []byte(`<html><head><script type="application/ld+json; charset=utf-8">` +
		`{"@context":"https://schema.org","@type":"JobPosting","title":"Fallback"}` +
		`</script></head></html>`)
	m, ok := firstTypedBlock(html, "JobPosting")
	if !ok || m["title"] != "Fallback" {
		t.Errorf("firstTypedBlock = (%v, %v), want fallback title", m, ok)
	}
}

func TestBlocksOfType_SidecarQuirkMiss(t *testing.T) {
	t.Parallel()
	// ponytail: inherited ceiling — the brace-scan fallback fires only
	// when the sidecar is ABSENT, so a sidecar lacking the type is a miss
	// even with a fallback-visible block right below it. Widening the
	// fallback later must flip this test deliberately, not silently.
	html := []byte(`<html><head>` +
		`<script type="application/ld+json">{"@context":"https://schema.org","@type":"WebSite","name":"S"}</script>` +
		`<script type="application/ld+json; charset=utf-8">{"@type":"JobPosting","title":"Hidden"}</script>` +
		`</head></html>`)
	// The sidecar candidates ARE returned (raw, type-unfiltered — same as
	// commerce's productBlocks), but the extractor's typed lookup misses:
	if _, ok := firstTypedBlock(html, "JobPosting"); ok {
		t.Error("firstTypedBlock = ok, want miss (sidecar-without-type never falls back)")
	}
}

func TestFirstTypedBlock_MultiType(t *testing.T) {
	t.Parallel()
	html := ldJSON(t, `{"@type":"BlogPosting","headline":"H"}`)
	m, ok := firstTypedBlock(html, "NewsArticle", "BlogPosting")
	if !ok || m["headline"] != "H" {
		t.Errorf("firstTypedBlock = (%v, %v), want BlogPosting hit", m, ok)
	}
	if _, ok := firstTypedBlock(html, "NewsArticle", "Article"); ok {
		t.Error("firstTypedBlock = ok, want miss when neither type present")
	}
}

func TestBlocksOfType_MalformedSkipped(t *testing.T) {
	t.Parallel()
	// Balanced-but-invalid fragment is skipped; the good block survives
	// (scan continues — never fatal on malformed JSON-LD).
	html := ldJSON(t, `{"a" 1},{"@type":"JobPosting","title":"Good"}`)
	blocks := blocksOfTypes(html, "JobPosting")
	if len(blocks) != 1 {
		t.Fatalf("blocksOfType = %d blocks, want 1 (malformed skipped)", len(blocks))
	}
	m, ok := firstTypedBlock(html, "JobPosting")
	if !ok || m["title"] != "Good" {
		t.Errorf("firstTypedBlock = (%v, %v), want Good", m, ok)
	}
}

func TestNewVerticals_NeverAutoFires(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"job_posting", "event", "local_business", "article", "rss"} {
		ex, ok := Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if !ex.OptIn {
			t.Errorf("%s OptIn = false, want true (permissive matchers never auto-fire)", name)
		}
	}
	// Extends TestMatchURL_StrictOnly/TestOptInNeverSteals with the five
	// new matchers' URL classes — rss must not fire even on obvious feeds.
	for _, raw := range []string{
		"https://careers.example/x",
		"https://events.example/x",
		"https://biz.example/",
		"https://news.example/post",
		"https://blog.example/feed",
		"https://blog.example/feed.xml",
	} {
		if ex, ok := MatchURL(raw); ok {
			t.Errorf("MatchURL(%s) auto-fired %s — OptIn must never auto-match", raw, ex.Info.Name)
		}
	}
	// Permissive explicit matches stay alive.
	if !matchHTTP(parseURL(t, "https://careers.example/x")) {
		t.Error("matchHTTP(careers URL) = false, want true")
	}
	rss, _ := Lookup("rss")
	if !rss.Match(parseURL(t, "https://blog.example/feed.xml")) {
		t.Error("rss.Match(feed URL) = false, want true (explicit path)")
	}
}
