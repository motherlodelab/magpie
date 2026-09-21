package vertical_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestArticleExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("article")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"news.example": {body: verticalFixture(t, "article.html")}, // carries OG/Twitter meta tags on purpose
	}}
	const page = "https://news.example/post"
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]any{
		"headline":      "Scrapers Eat the Web",
		"authors":       []string{"Ada Lovelace", "Grace Hopper"},
		"datePublished": "2026-09-18T08:00:00Z",
		"dateModified":  "2026-09-19T09:30:00Z",
		"publisher":     "The Daily Example",
		"image":         "https://cdn.example/hero.jpg",
		"url":           page, // decoy url inside the block must NOT win
	}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("got %#v\nwant %#v", rec, want)
	}
	// Non-duplication pin: og/meta fields belong to the `og` extractor.
	for _, k := range []string{"og_title", "og_image", "twitter_card", "canonical"} {
		if _, ok := rec[k]; ok {
			t.Errorf("%s present — article must never read meta tags", k)
		}
	}
}

func TestArticle_Variants(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("article")
	t.Run("author scalar string", func(t *testing.T) {
		t.Parallel()
		rec := extractInline(t, ex, "news.example", `{"@type":"Article","headline":"H","author":"Solo Author"}`)
		if !reflect.DeepEqual(rec["authors"], []string{"Solo Author"}) {
			t.Errorf("authors = %#v", rec["authors"])
		}
	})
	t.Run("author single object", func(t *testing.T) {
		t.Parallel()
		rec := extractInline(t, ex, "news.example", `{"@type":"Article","headline":"H","author":{"@type":"Person","name":"Obj Author"}}`)
		if !reflect.DeepEqual(rec["authors"], []string{"Obj Author"}) {
			t.Errorf("authors = %#v", rec["authors"])
		}
	})
	for _, typ := range []string{"BlogPosting", "NewsArticle"} {
		t.Run("type "+typ, func(t *testing.T) {
			t.Parallel()
			rec := extractInline(t, ex, "news.example", `{"@type":"`+typ+`","headline":"Typed"}`)
			if rec["headline"] != "Typed" {
				t.Errorf("@type %s: headline = %#v", typ, rec["headline"])
			}
		})
	}
	t.Run("no dateModified", func(t *testing.T) {
		t.Parallel()
		rec := extractInline(t, ex, "news.example", `{"@type":"BlogPosting","headline":"H","datePublished":"2026-01-01"}`)
		if _, ok := rec["dateModified"]; ok {
			t.Error("dateModified present with no source value — omission broken")
		}
	})
}

func TestArticleExtract_NoBlock(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("article")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"news.example": {body: []byte(`<html><body>plain text only</body></html>`)},
	}}
	const page = "https://news.example/none"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: article: no Article data at "+page) {
		t.Fatalf("err = %v, want typed no-data error naming the URL", err)
	}
}
