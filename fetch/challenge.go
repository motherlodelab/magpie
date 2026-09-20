package fetch

// Typed bot-challenge detection (Phase G): a 200/403 challenge page
// errors loudly with the vendor name instead of flowing into cleaning
// as garbage. Header signatures are authoritative (no size gate); body
// signatures only count under the same thin-page gate IsChallengePage
// uses, so a rich article mentioning "Just a moment" stays clean.
// clean.Classify remains the final backstop for untyped challenges.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/motherlodelab/magpie/clean"
)

// ChallengeError is the typed challenge failure. Edges match it with
// errors.As to recover the vendor; the message crosses the MCP boundary
// — change the format only with a declared message change.
type ChallengeError struct {
	Vendor     string
	StatusCode int
	URL        string
}

func (e *ChallengeError) Error() string {
	return fmt.Sprintf("fetch: bot challenge (%s) on %s [status %d]", e.Vendor, e.URL, e.StatusCode)
}

// challengeBodySignatures maps body markers to vendor names, first
// match wins (all markers of a signature must be present). All checks
// run against the lower-cased body.
var challengeBodySignatures = []struct {
	vendor  string
	markers []string
}{
	{vendor: "cloudflare", markers: []string{"_cf_chl_opt"}},
	{vendor: "cloudflare", markers: []string{"challenge-platform/h/b/orchestrate/"}},
	{vendor: "cloudflare", markers: []string{"/h/g/orchestrate/"}},
	{vendor: "turnstile", markers: []string{"challenges.cloudflare.com/turnstile"}},
	{vendor: "datadome", markers: []string{"datadome", "captcha"}},
	{vendor: "datadome", markers: []string{"datadome", "geo"}},
	{vendor: "awswaf", markers: []string{"awswaf"}},
	{vendor: "hcaptcha", markers: []string{"h-captcha", "verify"}},
	{vendor: "hcaptcha", markers: []string{"h-captcha", "blocked"}},
}

// DetectChallenge returns the bot-protection vendor for a response, or
// "". The cf-mitigated header is authoritative regardless of body size;
// body signatures only fire under the thin-page gate (same contract as
// IsChallengePage - status classification stays there, not here).
func DetectChallenge(body []byte, headers http.Header, status int) string {
	if headers.Get("cf-mitigated") != "" {
		return "cloudflare"
	}
	if len(body) == 0 || len(body) >= 15*1024 || clean.WordCount(string(body)) >= clean.ThinPageWords {
		return ""
	}
	lower := strings.ToLower(string(body))
	for _, sig := range challengeBodySignatures {
		ok := true
		for _, m := range sig.markers {
			if !strings.Contains(lower, m) {
				ok = false
				break
			}
		}
		if ok {
			return sig.vendor
		}
	}
	return ""
}

// renderedChallengeSignatures classifies browser-rendered interstitials.
// No thin-page gate here: a challenge that survives rod escalation is a
// full 200 DOM (Upwork's serialized shell is ~345KB), so the static
// path's size gate would skip every marker. The pairs are stricter than
// the static signatures instead - a live interstitial embeds its own
// script identifiers alongside vendor UI strings, and an article ABOUT a
// vendor almost never carries both at once.
var renderedChallengeSignatures = []struct {
	vendor  string
	markers []string
}{
	{vendor: "cloudflare", markers: []string{"cf_chl", "turnstile"}},
	{vendor: "cloudflare", markers: []string{"_cf_chl_opt", "ray id"}},
}

// DetectChallengeRendered classifies a browser-rendered DOM (rod output):
// same typed-vendor contract as DetectChallenge, minus the thin-page
// gate, plus stricter marker pairs.
func DetectChallengeRendered(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	lower := strings.ToLower(string(body))
	for _, sig := range renderedChallengeSignatures {
		ok := true
		for _, m := range sig.markers {
			if !strings.Contains(lower, m) {
				ok = false
				break
			}
		}
		if ok {
			return sig.vendor
		}
	}
	return ""
}
