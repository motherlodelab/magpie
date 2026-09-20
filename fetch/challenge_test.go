package fetch_test

// Challenge detection tests (Phase G G.2): fixture-file vendor table,
// header-authoritative row, rich-negative, PDF-negative, and the
// DetectChallenge/IsChallengePage status split. Fixtures under
// testdata/challenge/ are plan deliverables — a missing file fails.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/clean"
	"github.com/motherlodelab/magpie/fetch"
)

func loadChallengeFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "challenge", name+".html"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestDetectChallenge_Vendors: every vendor fixture classifies exactly.
func TestDetectChallenge_Vendors(t *testing.T) {
	cases := []struct{ file, vendor string }{
		{"cloudflare", "cloudflare"},
		{"turnstile", "turnstile"},
		{"datadome", "datadome"},
		{"awswaf", "awswaf"},
		{"hcaptcha", "hcaptcha"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			body := loadChallengeFixture(t, c.file)
			if got := fetch.DetectChallenge(body, nil, 403); got != c.vendor {
				t.Errorf("DetectChallenge(%s) = %q, want %q", c.file, got, c.vendor)
			}
			if got := fetch.DetectChallenge(body, nil, 200); got != c.vendor {
				t.Errorf("DetectChallenge(%s, 200) = %q, want %q (body signatures are status-independent)", c.file, got, c.vendor)
			}
		})
	}
}

// TestDetectChallenge_HeaderAuthoritative: cf-mitigated wins even on a
// rich body (no size gate on headers).
func TestDetectChallenge_HeaderAuthoritative(t *testing.T) {
	rich := "<html><body>" + strings.Repeat("honest prose about many things ", 300) + "</body></html>"
	h := http.Header{"Cf-Mitigated": {"challenge"}}
	if got := fetch.DetectChallenge([]byte(rich), h, 200); got != "cloudflare" {
		t.Errorf("header on rich body = %q, want cloudflare", got)
	}
	if got := fetch.DetectChallenge([]byte(rich), nil, 200); got != "" {
		t.Errorf("rich body without header = %q, want \"\"", got)
	}
}

// TestDetectChallenge_RichNegative: an article that merely mentions
// "Just a moment" is not a challenge — detection returns "" AND clean
// still produces markdown.
func TestDetectChallenge_RichNegative(t *testing.T) {
	rich := "<html><head><title>Just a moment — our story</title></head><body><p>" +
		strings.Repeat("Our editorial team published honest descriptive prose every single week. ", 40) +
		"We wrote Just a moment ago about verification systems and captcha blocking. " +
		strings.Repeat("More paragraphs of genuine article content follow here for scoring. ", 20) +
		"</p></body></html>"
	if got := fetch.DetectChallenge([]byte(rich), nil, 200); got != "" {
		t.Fatalf("rich negative = %q, want \"\"", got)
	}
	cleaned, err := clean.Clean(t.Context(), clean.RawPage{HTML: []byte(rich), FinalURL: "https://example.com/story"})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Markdown == "" {
		t.Error("rich negative must still clean to markdown")
	}
}

// TestDetectChallenge_Negatives: PDF bytes and empty bodies never
// classify; a challenge STATUS alone stays IsChallengePage's job (the
// split — DetectChallenge is body/header signatures only).
func TestDetectChallenge_Negatives(t *testing.T) {
	if got := fetch.DetectChallenge([]byte("%PDF-1.7 binary-ish bytes"), nil, 403); got != "" {
		t.Errorf("PDF bytes = %q, want \"\"", got)
	}
	if got := fetch.DetectChallenge(nil, nil, 403); got != "" {
		t.Errorf("empty body = %q, want \"\"", got)
	}
	tiny := []byte("<html><body>plain tiny page, no signatures</body></html>")
	if got := fetch.DetectChallenge(tiny, nil, 403); got != "" {
		t.Errorf("403 tiny no-sig = %q, want \"\" (status stays IsChallengePage's job)", got)
	}
	if !fetch.IsChallengePage(tiny, 403) {
		t.Error("IsChallengePage must classify the 403 tiny page — assert the split, both halves")
	}
}

// TestChallengeError_Message pins the cross-boundary message shape.
func TestChallengeError_Message(t *testing.T) {
	e := &fetch.ChallengeError{Vendor: "cloudflare", StatusCode: 403, URL: "https://example.com/x"}
	want := "fetch: bot challenge (cloudflare) on https://example.com/x [status 403]"
	if e.Error() != want {
		t.Errorf("message = %q, want %q", e.Error(), want)
	}
}

func TestDetectChallengeRendered_UpworkInterstitial(t *testing.T) {
	// Probe-derived shape (2026-09-20): Upwork's shell survives rod as a
	// 200 DOM, ~345KB, carrying the live script identifiers and vendor UI.
	body := []byte(`<html><title>Just a moment...</title><script>var cf_chl_opt={"r":"a3"}</script>` +
		`<script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script>` +
		strings.Repeat("<p>filler</p>", 6000) + `<div>Cloudflare Ray ID: a3df</div></html>`)
	if v := fetch.DetectChallengeRendered(body, 200); v != "cloudflare" {
		t.Errorf("DetectChallengeRendered = %q, want cloudflare", v)
	}
}

func TestDetectChallengeRendered_ArticleNoFalsePositive(t *testing.T) {
	// A large article ABOUT anti-bot vendors mentions the words but never
	// embeds the live script identifiers next to the vendor UI string.
	var b strings.Builder
	b.WriteString("<html><title>Solving captchas with cloudflare tools</title><body>")
	for b.Len() < 20*1024 {
		b.WriteString("<p>Discusses turnstile widgets and the ray id header in theory.</p>")
	}
	b.WriteString("</body></html>")
	if v := fetch.DetectChallengeRendered([]byte(b.String()), 200); v != "" {
		t.Errorf("DetectChallengeRendered = %q, want empty for legit article", v)
	}
}

func TestDetectChallengeRendered_Empty(t *testing.T) {
	if v := fetch.DetectChallengeRendered(nil, 200); v != "" {
		t.Errorf("empty body = %q, want empty", v)
	}
}
