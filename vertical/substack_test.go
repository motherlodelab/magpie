package vertical_test

import (
	"math"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestSubstackMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("substack_post")
	if !ok {
		t.Fatal("substack_post not registered")
	}
	yes := []string{
		"https://demo.substack.com/p/hello-world",
		"https://a.b.substack.com/p/deep-nesting",
		"https://substack.com/p/x",
	}
	no := []string{
		"https://notsubstack.com/p/x",       // suffix must be a dot boundary
		"https://demo.substack.com/about",   // not a post path
		"https://demo.substack.com/archive", // not a post path
		"https://demo.substack.com/p/",      // no slug
		"file:///tmp/p/x",
	}
	for _, raw := range yes {
		if !ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = false, want true", raw)
		}
	}
	for _, raw := range no {
		if ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = true, want false", raw)
		}
	}
}

func TestSubstackExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("substack_post")
	const page = "https://demo.substack.com/p/hello-world"
	// Keyed on the REWRITTEN API URL — the fake only succeeds if Extract
	// actually hits the endpoint.
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"demo.substack.com/api/v1/posts/": {body: verticalFixture(t, "substack-post.json")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Hello, world" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["subtitle"] != "A demo post" {
		t.Errorf("subtitle = %v", rec["subtitle"])
	}
	if rec["author"] != "Jane Q. Writer" {
		t.Errorf("author = %v", rec["author"])
	}
	if rec["published"] != "2026-08-14T09:00:00Z" { // raw passthrough
		t.Errorf("published = %v", rec["published"])
	}
	if n, _ := rec["likes"].(float64); math.Abs(n-823) > 1e-9 {
		t.Errorf("likes = %#v, want 823 (float64)", rec["likes"])
	}
	if n, _ := rec["comments"].(float64); math.Abs(n-47) > 1e-9 {
		t.Errorf("comments = %#v, want 47 (float64)", rec["comments"])
	}
	if rec["cover_image"] != "https://substackcdn.com/cover.png" {
		t.Errorf("cover_image = %v", rec["cover_image"])
	}
	// stripTags glue ceiling pinned: block tags glue, no breaks inserted.
	if rec["text"] != "Hello world.Second graph." {
		t.Errorf("text = %q, want %q", rec["text"], "Hello world.Second graph.")
	}
	if rec["url"] != page { // the page URL, never the API endpoint
		t.Errorf("url = %v, want page URL", rec["url"])
	}
}

func TestSubstackExtract_AuthorShapes(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("substack_post")
	const page = "https://demo.substack.com/p/x"
	cases := []struct {
		name   string
		api    string
		author string // "" = key must be absent
	}{
		{"legacy bylines shape", `{"title":"T","bylines":[{"name":"By Line"}]}`, "By Line"},
		{"legacy author shape", `{"title":"T","author":[{"name":"Auth Or"}]}`, "Auth Or"},
		{"neither", `{"title":"T"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
				"demo.substack.com/api/v1/posts/": {body: []byte(tc.api)},
			}}
			rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if tc.author == "" {
				if _, ok := rec["author"]; ok {
					t.Error("author present with no source field — omission broken")
				}
				return
			}
			if rec["author"] != tc.author {
				t.Errorf("author = %v, want %s", rec["author"], tc.author)
			}
		})
	}
}

// The current API reports reaction_count; the older like_count shape must
// still decode (same record key).
func TestSubstackExtract_LegacyLikes(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("substack_post")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"demo.substack.com/api/v1/posts/": {body: []byte(`{"title":"T","like_count":5}`)},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://demo.substack.com/p/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["likes"] != 5.0 {
		t.Errorf("likes = %#v, want 5 from legacy like_count", rec["likes"])
	}
}

func TestSubstackExtract_APIError(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("substack_post")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"demo.substack.com/api/v1/posts/": {status: 500, body: []byte(`{"error":"boom"}`)},
	}}
	_, err := ex.Extract(t.Context(), fx, mustURL(t, "https://demo.substack.com/p/x"))
	if err == nil || !strings.Contains(err.Error(), "vertical: GET https://demo.substack.com/api/v1/posts/x: HTTP 500") {
		t.Fatalf("err = %v, want typed HTTP 500 naming the API URL", err)
	}
}

// Custom-domain substacks never Match (host ceiling) but Extract rewrites
// ANY host's /p/{slug} to the same-origin public API. The fake's request
// log is the only honest witness of which endpoint was hit.
func TestSubstackExtract_CustomDomainRewrite(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("substack_post")
	if ex.Match(mustURL(t, "https://custom.blog/p/x")) {
		t.Fatal("Match(custom.blog) = true — custom domains are a Match ceiling")
	}
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"custom.blog/api/v1/posts/": {body: verticalFixture(t, "substack-post.json")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://custom.blog/p/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := fx.requests(); len(got) != 1 || got[0] != "https://custom.blog/api/v1/posts/x" {
		t.Errorf("requests = %v, want exactly the rewritten API URL", got)
	}
	if rec["url"] != "https://custom.blog/p/x" {
		t.Errorf("url = %v, want the page URL", rec["url"])
	}
}
