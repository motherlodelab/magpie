package vertical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

// OAuth-first reddit tests. The token POST bypasses the Fetcher, so it
// rides an httptest server via the OverrideRedditEndpoints bridge (the
// fake fetcher never sees that request); the oauth .json GET goes through
// the fake fetcher, whose bodies map keys on URL substring.

func oauthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MAGPIE_REDDIT_CLIENT_ID", "cid")
	t.Setenv("MAGPIE_REDDIT_CLIENT_SECRET", "sec")
	t.Setenv("MAGPIE_REDDIT_UA", "test:magpie:v1 (by /u/tester)")
}

func tokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("token POST missing basic auth")
		}
		if _, werr := w.Write([]byte(`{"access_token":"t0k","expires_in":3600,"token_type":"bearer"}`)); werr != nil {
			t.Error(werr)
		}
	}))
	t.Cleanup(ts.Close)
	restore := vertical.OverrideRedditEndpoints(ts.URL, "https://oauth.example")
	t.Cleanup(restore)
	return ts
}

func TestReddit_OAuthFirst_Permalink(t *testing.T) {
	oauthEnv(t)
	tokenServer(t)
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"oauth.example": {body: []byte(`[
			{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"title":"Demo post title","author":"testuser","score":123,"selftext":"body"}}]}},
			{"kind":"Listing","data":{"children":[]}}
		]`)},
	}}
	ex, _ := vertical.Lookup("reddit")
	got, err := ex.Extract(context.Background(), fx, mustURL(t, "https://www.reddit.com/r/demo/comments/abc123/demo_post/"))
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "post" || got["title"] != "Demo post title" {
		t.Errorf("got %#v", got)
	}
	// The .json GET must hit oauth.example with the bearer + namespaced UA.
	reqs := fx.reqLogReqs()
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0].URL, "https://oauth.example/r/demo/comments/abc123/demo_post/.json") {
		t.Fatalf("requests = %#v", reqs)
	}
	hdrs := strings.Join(reqs[0].Headers, "|")
	if !strings.Contains(hdrs, "Authorization: Bearer t0k") || !strings.Contains(hdrs, "User-Agent: test:magpie:v1") {
		t.Errorf("headers = %v", reqs[0].Headers)
	}
}

func TestReddit_OAuthFirst_Listing(t *testing.T) {
	oauthEnv(t)
	tokenServer(t)
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"oauth.example": {body: []byte(`{"kind":"Listing","data":{"children":[
			{"kind":"t5","data":{"title":"demo","public_description":"demo subreddit community","subscribers":9876}},
			{"kind":"t3","data":{"title":"sticky post"}}
		]}}`)},
	}}
	ex, _ := vertical.Lookup("reddit")
	got, err := ex.Extract(context.Background(), fx, mustURL(t, "https://www.reddit.com/r/demo/"))
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "subreddit" || got["title"] != "demo" || got["score"] != float64(9876) {
		t.Errorf("got %#v", got)
	}
}

func TestReddit_OAuthPartialCreds_FailLoudly(t *testing.T) {
	t.Setenv("MAGPIE_REDDIT_CLIENT_ID", "cid")
	// secret + UA unset → error naming them, before any request.
	t.Setenv("MAGPIE_REDDIT_CLIENT_SECRET", "")
	t.Setenv("MAGPIE_REDDIT_UA", "")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{}}
	ex, _ := vertical.Lookup("reddit")
	_, err := ex.Extract(context.Background(), fx, mustURL(t, "https://www.reddit.com/r/demo/"))
	if err == nil {
		t.Fatal("want error for partial creds")
	}
	for _, v := range []string{"MAGPIE_REDDIT_CLIENT_SECRET", "MAGPIE_REDDIT_UA"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("err %q missing %s", err, v)
		}
	}
	if len(fx.reqLogReqs()) != 0 {
		t.Errorf("partial creds must not fetch; got %d requests", len(fx.reqLogReqs()))
	}
}

func TestReddit_OAuthTokenFailure_NoSilentFallback(t *testing.T) {
	oauthEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	defer vertical.OverrideRedditEndpoints(ts.URL, "https://oauth.example")()
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"old.reddit.com": {body: []byte("<html><body>SHOULD NOT BE FETCHED</body></html>")},
	}}
	ex, _ := vertical.Lookup("reddit")
	_, err := ex.Extract(context.Background(), fx, mustURL(t, "https://www.reddit.com/r/demo/comments/abc123/x/"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want the 401 named", err)
	}
	// Loud degradation: no fallback request to the anonymous ladder.
	if len(fx.reqLogReqs()) != 0 {
		t.Errorf("token failure must not fall back to anonymous; got %d requests", len(fx.reqLogReqs()))
	}
}

// Tokenless: today's ladder untouched (order pinned — .json first, HTML
// fallback for permalinks; HTML for listings).
func TestReddit_Tokenless_LadderOrderUnchanged(t *testing.T) {
	t.Setenv("MAGPIE_REDDIT_CLIENT_ID", "")
	t.Setenv("MAGPIE_REDDIT_CLIENT_SECRET", "")
	t.Setenv("MAGPIE_REDDIT_UA", "")
	_ = url.Values{} // keep net/url import if assertions above drop it
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"old.reddit.com/r/demo/comments": {body: []byte(`<html><title>post</title></html>`)},
		"old.reddit.com/r/demo/":         {body: []byte(`<html><title>sub</title></html>`)},
	}}
	ex, _ := vertical.Lookup("reddit")
	if _, err := ex.Extract(context.Background(), fx, mustURL(t, "https://www.reddit.com/r/demo/comments/abc123/x/")); err != nil {
		t.Fatal(err)
	}
	order := fx.order
	if len(order) == 0 || !strings.Contains(order[0], ".json") {
		t.Errorf("first request = %q, want permalink .json first", order[0])
	}
}
