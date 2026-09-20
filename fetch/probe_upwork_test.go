package fetch_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

func TestProbeUpworkBrowserDOM(t *testing.T) {
	if testing.Short() {
		t.Skip("probe")
	}
	rod := fetch.NewRodFetcher()
	defer rod.Close() //nolint:errcheck
	resp, err := rod.Fetch(t.Context(), fetch.FetchRequest{URL: "https://www.upwork.com/jobs/~01a2b3c4"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	vendor := fetch.DetectChallenge(resp.HTML, resp.Headers, resp.StatusCode)
	fmt.Printf("PROBE status=%d len=%d vendor=%q\n", resp.StatusCode, len(resp.HTML), vendor)
	low := strings.ToLower(string(resp.HTML))
	for _, m := range []string{"verifying", "ray id", "challenge - upwork", "cf_chl", "turnstile", "cloudflare"} {
		fmt.Printf("  marker %q: %v\n", m, strings.Contains(low, m))
	}
	head := string(resp.HTML)
	if len(head) > 300 {
		head = head[:300]
	}
	fmt.Printf("  head: %s\n", strings.ReplaceAll(head, "\n", " "))
}
