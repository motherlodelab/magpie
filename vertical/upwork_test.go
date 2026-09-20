package vertical_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
	"github.com/motherlodelab/magpie/vertical"
)

// Fixture upwork_job.html: a synthetic shape fixture - og/meta unfurl tags
// plus a __NEXT_DATA__ hydration block with a job-shaped map. Mirrors the
// documented surfaces, not a live capture: Upwork challenges this network
// (403 challenge shell), so the embedded field map stays unverified.

func TestUpworkMatch_Table(t *testing.T) {
	ex, ok := vertical.Lookup("upwork_job")
	if !ok {
		t.Fatal("upwork_job not registered")
	}
	yes := []string{
		"https://www.upwork.com/jobs/~01a2b3c4",
		"https://www.upwork.com/jobs/senior-go-scraper~01a2b3c4",
		"https://www.upwork.com/job/automation-engineer~abcd1234",
		"https://upwork.com/jobs/~xyz",
	}
	no := []string{
		"https://www.upwork.com/nx/search/jobs/?q=web+scraping",
		"https://www.upwork.com/ab/applied-jobs/",
		"https://www.upwork.com/developer/documentation/graphql/api/docs/index.html",
		"https://www.upwork.com/freelancers/~somespecialist",
		"https://example.com/jobs/~01a2b3c4",
		"https://www.upwork.com/",
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

func TestUpworkExtract_MetaPlusEmbedded(t *testing.T) {
	ex, _ := vertical.Lookup("upwork_job")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"upwork.com": {body: verticalFixture(t, "upwork_job.html")},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.upwork.com/jobs/senior-go-scraper~01a2b3c4"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["source"] != "embedded" {
		t.Errorf("source = %v, want embedded (fixture carries __NEXT_DATA__)", rec["source"])
	}
	if title, _ := rec["title"].(string); title != "Senior Go Scraper (Cloudflare hardening)" {
		t.Errorf("title = %q", title)
	}
	if desc, _ := rec["description"].(string); !strings.Contains(desc, "production scraper") {
		t.Errorf("description = %q, want og:description content", desc)
	}
	if budget, _ := rec["budget"].(string); budget != "$500-1,000" {
		t.Errorf("budget = %q, want embedded value", budget)
	}
	skills, _ := rec["skills"].([]string)
	if len(skills) != 3 || skills[0] != "Go" {
		t.Errorf("skills = %v, want [Go ...] from embedded", skills)
	}
	if id, _ := rec["id"].(string); id != "~01a2b3c4" {
		t.Errorf("id = %q, want ciphertext", id)
	}
}

func TestUpworkExtract_MetaOnly(t *testing.T) {
	ex, _ := vertical.Lookup("upwork_job")
	html := []byte(`<html><head>
		<meta property="og:title" content="Data pipeline fix | Upwork">
		<meta property="og:description" content="Fix a broken pipeline.">
	</head><body>shell</body></html>`)
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"upwork.com": {body: html},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.upwork.com/jobs/~deadbeef"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["source"] != "meta" {
		t.Errorf("source = %v, want meta (no __NEXT_DATA__)", rec["source"])
	}
	if title, _ := rec["title"].(string); title != "Data pipeline fix" {
		t.Errorf("title = %q, want brand suffix trimmed", title)
	}
}

func TestUpworkExtract_ChallengePropagates(t *testing.T) {
	ex, _ := vertical.Lookup("upwork_job")
	cerr := &fetch.ChallengeError{Vendor: "cloudflare", StatusCode: 403, URL: "https://www.upwork.com/jobs/~x"}
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"upwork.com": {err: cerr},
	}}
	_, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.upwork.com/jobs/~x"))
	var ce *fetch.ChallengeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *fetch.ChallengeError", err)
	}
	if ce.Vendor != "cloudflare" {
		t.Errorf("vendor = %q, want cloudflare", ce.Vendor)
	}
}

func TestUpworkExtract_ChallengeShell403(t *testing.T) {
	ex, _ := vertical.Lookup("upwork_job")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"upwork.com": {status: 403, body: []byte("<html><title>Challenge - Upwork</title></html>")},
	}}
	_, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.upwork.com/jobs/~x"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v, want hard 403 error", err)
	}
}

func TestUpworkExtract_NoMeta(t *testing.T) {
	ex, _ := vertical.Lookup("upwork_job")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"upwork.com": {status: 200, body: []byte("<html><body>no metadata here</body></html>")},
	}}
	_, err := ex.Extract(t.Context(), fx, mustURL(t, "https://www.upwork.com/jobs/~x"))
	if err == nil || !strings.Contains(err.Error(), "no og/meta content") {
		t.Fatalf("err = %v, want no og/meta content error", err)
	}
}
