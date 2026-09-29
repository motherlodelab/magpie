package vertical

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

const (
	okTokenJSON  = `{"access_token":"t0k","expires_in":3600,"token_type":"bearer"}`
	badTokenJSON = `{"error":"unauthorized"}` // no access_token → decode-shape error path
)

// Z.1: fetchJSONAuth forwards headers; absent headers → request identical
// to today's tokenless shape (only the absence of a header asserted — the
// default header bundle lives in fetch/, not here).
func TestFetchJSONAuth_ForwardsHeaders(t *testing.T) {
	fx := &fakeAuthFetcher{body: []byte(`{"ok":true}`)}
	if _, err := fetchJSONAuth(context.Background(), fx, "https://api.example.com/x", []string{"Authorization: Bearer z"}); err != nil {
		t.Fatal(err)
	}
	req := fx.lastReq()
	if len(req.Headers) != 1 || !strings.HasPrefix(req.Headers[0], "Authorization: Bearer z") {
		t.Errorf("headers = %v, want the bearer", req.Headers)
	}

	// nil headers → byte-identical tokenless contract.
	if _, err := fetchJSON(context.Background(), fx, "https://api.example.com/x"); err != nil {
		t.Fatal(err)
	}
	if n := len(fx.lastReq().Headers); n != 0 {
		t.Errorf("tokenless headers n = %d, want none", n)
	}
}

func TestFetchBytesAuth_Non2xxIsHardError(t *testing.T) {
	fx := &fakeAuthFetcher{status: 403, body: []byte("nope")}
	if _, err := fetchBytesAuth(context.Background(), fx, "https://api.example.com/x", nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want HTTP 403 named", err)
	}
}

func TestGithubAuthHeaders(t *testing.T) {
	t.Setenv("MAGPIE_GITHUB_TOKEN", "")
	if got := githubAuthHeaders(); got != nil {
		t.Errorf("tokenless = %v, want nil", got)
	}
	t.Setenv("MAGPIE_GITHUB_TOKEN", "gh_pat")
	got := githubAuthHeaders()
	if len(got) != 1 || got[0] != "Authorization: Bearer gh_pat" {
		t.Errorf("got %v", got)
	}
}

func TestGithub_TokenRidesRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   string
		wantHdr bool
	}{
		{"token set", "gh_pat", true},
		{"tokenless", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAGPIE_GITHUB_TOKEN", tc.token)
			fx := &fakeAuthFetcher{body: []byte(`{"full_name":"o/r"}`)}
			if _, err := extractGithub(context.Background(), fx, mustURL(t, "https://github.com/o/r")); err != nil {
				t.Fatal(err)
			}
			hdrs := fx.lastReq().Headers
			if tc.wantHdr != (len(hdrs) > 0) {
				t.Errorf("headers = %v, wantHdr=%v", hdrs, tc.wantHdr)
			}
		})
	}
}

func TestRedditToken(t *testing.T) {
	var gotBasic, gotCT string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBasic, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		body, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			t.Errorf("read body: %v", rerr)
		}
		if !strings.Contains(string(body), "grant_type=client_credentials") {
			t.Errorf("body = %q", body)
		}
		if _, werr := w.Write([]byte(okTokenJSON)); werr != nil {
			t.Error(werr)
		}
	}))
	defer ts.Close()
	defer OverrideRedditEndpoints(ts.URL, "https://oauth.reddit.com")()

	tok, err := redditToken(context.Background(), "cid", "sec")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "t0k" {
		t.Errorf("token = %q", tok)
	}
	if gotBasic != "Basic "+basicAuth("cid", "sec") {
		t.Errorf("basic auth = %q", gotBasic)
	}
	if !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		t.Errorf("content-type = %q", gotCT)
	}

	// 401 → error names the grant step.
	ts401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts401.Close()
	defer OverrideRedditEndpoints(ts401.URL, "https://oauth.reddit.com")()
	if _, err := redditToken(context.Background(), "cid", "sec"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want 401 named", err)
	}

	// 200 but wrong shape → decode error.
	tsBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, werr := w.Write([]byte(badTokenJSON)); werr != nil {
			t.Error(werr)
		}
	}))
	defer tsBad.Close()
	defer OverrideRedditEndpoints(tsBad.URL, "https://oauth.reddit.com")()
	if _, err := redditToken(context.Background(), "cid", "sec"); err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Errorf("err = %v, want missing access_token named", err)
	}
}

func TestRedditCreds_PartialIsHardError(t *testing.T) {
	t.Setenv("MAGPIE_REDDIT_CLIENT_ID", "cid")
	t.Setenv("MAGPIE_REDDIT_CLIENT_SECRET", "")
	t.Setenv("MAGPIE_REDDIT_UA", "")
	_, _, _, err := redditCreds()
	if err == nil {
		t.Fatal("want error for partial creds")
	}
	for _, v := range []string{"MAGPIE_REDDIT_CLIENT_SECRET", "MAGPIE_REDDIT_UA"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("err %q missing %s", err, v)
		}
	}
}

// basicAuth mirrors http's basic-auth header value for assertion clarity.
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// fakeAuthFetcher is the package-internal twin of vertical_test's fake
// (the external fake isn't visible from package vertical tests).
type fakeAuthFetcher struct {
	body   []byte
	status int
	req    fetch.FetchRequest
}

func (f *fakeAuthFetcher) lastReq() fetch.FetchRequest { return f.req }

func (f *fakeAuthFetcher) Fetch(_ context.Context, req fetch.FetchRequest) (*fetch.FetchResponse, error) {
	f.req = req
	st := f.status
	if st == 0 {
		st = 200
	}
	return &fetch.FetchResponse{StatusCode: st, HTML: f.body}, nil
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
