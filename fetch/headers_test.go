package fetch_test

// M0a: run-supplied request headers on the static path. The merge is
// one directional rule — user headers apply AFTER the profile bundle,
// Lang, and Cookies, so the caller wins conflicts. One conflict test
// pins the direction; a matrix would test the stdlib.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

// headerEcho reflects the named request headers into the body as
// Name=value pairs (echoOrigin pins UA|CH|CK; this one takes names).
func headerEcho(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, n+"="+r.Header.Get(n))
		}
		fmt.Fprint(w, strings.Join(parts, "|")) //nolint:errcheck // httptest local
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHeaders_BareDelivery(t *testing.T) {
	srv := headerEcho(t, "X-Magpie-Probe", "Authorization")
	body := fetchBody(t, srv.URL, fetch.FetchRequest{
		Headers: []string{"X-Magpie-Probe: hello", "Authorization: Bearer tok"},
	})
	for _, want := range []string{"X-Magpie-Probe=hello", "Authorization=Bearer tok"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
}

// TestHeaders_UserWinsConflict: the single directional pin — a user
// Accept-Language replaces the profile bundle's default of the same
// name, and the other defaults stay untouched.
func TestHeaders_UserWinsConflict(t *testing.T) {
	srv := headerEcho(t, "Accept-Language", "User-Agent")
	body := fetchBody(t, srv.URL, fetch.FetchRequest{
		Headers: []string{"Accept-Language: de-DE,de;q=0.9"},
	})
	if !strings.Contains(body, "Accept-Language=de-DE,de;q=0.9") {
		t.Errorf("user header did not beat the profile default: %q", body)
	}
	if !strings.Contains(body, "User-Agent=github.com/motherlodelab/magpie/1.0") {
		t.Errorf("unrelated default header lost: %q", body)
	}
}

// TestHeaders_MalformedLineSkipped: a colon-less line is skipped on the
// fetch path (the options boundary rejects it with the typed error) —
// delivery of the valid remainder must not be affected.
func TestHeaders_MalformedLineSkipped(t *testing.T) {
	srv := headerEcho(t, "X-Magpie-Probe")
	body := fetchBody(t, srv.URL, fetch.FetchRequest{
		Headers: []string{"no-colon-shape", "X-Magpie-Probe: still-here"},
	})
	if !strings.Contains(body, "X-Magpie-Probe=still-here") {
		t.Errorf("valid header lost next to a malformed line: %q", body)
	}
}
