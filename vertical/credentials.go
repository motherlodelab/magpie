package vertical

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/motherlodelab/magpie/fetch"
)

// Credential injection (Phase Z): env-var credentials ride the existing
// fetch.FetchRequest.Headers seam — absent vars mean nil headers and
// byte-identical tokenless behavior. Credentials are read per-call so
// tests can t.Setenv them.

// Unexported so _test.go can point the token POST and oauth GETs at
// httptest servers (the token hop bypasses the Fetcher, so a fake
// fetcher can never intercept it).
var (
	redditTokenURL  = "https://www.reddit.com/api/v1/access_token"
	redditOAuthHost = "https://oauth.reddit.com"
)

// fetchBytesAuth is fetchBytes with raw request headers attached.
func fetchBytesAuth(ctx context.Context, f Fetcher, rawURL string, headers []string) ([]byte, error) {
	resp, err := f.Fetch(ctx, fetch.FetchRequest{URL: rawURL, Headers: headers})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("vertical: GET %s: HTTP %d", rawURL, resp.StatusCode)
	}
	return resp.HTML, nil
}

// fetchJSONAuth is fetchJSON with raw request headers attached.
func fetchJSONAuth(ctx context.Context, f Fetcher, rawURL string, headers []string) (map[string]any, error) {
	body, err := fetchBytesAuth(ctx, f, rawURL, headers)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("vertical: GET %s: decode: %w", rawURL, err)
	}
	return m, nil
}

// githubAuthHeaders returns the bearer header when MAGPIE_GITHUB_TOKEN is
// set, else nil.
func githubAuthHeaders() []string {
	if tok := os.Getenv("MAGPIE_GITHUB_TOKEN"); tok != "" {
		return []string{"Authorization: Bearer " + tok}
	}
	return nil
}

// redditCreds reports the three env vars; partial configuration is a hard
// error naming the missing vars (fail loudly at a trust boundary — a
// silent anonymous fallback would hide a misconfigured env).
func redditCreds() (id, secret, ua string, err error) {
	id = os.Getenv("MAGPIE_REDDIT_CLIENT_ID")
	secret = os.Getenv("MAGPIE_REDDIT_CLIENT_SECRET")
	ua = os.Getenv("MAGPIE_REDDIT_UA")
	var missing []string
	if id == "" {
		missing = append(missing, "MAGPIE_REDDIT_CLIENT_ID")
	}
	if secret == "" {
		missing = append(missing, "MAGPIE_REDDIT_CLIENT_SECRET")
	}
	if ua == "" {
		missing = append(missing, "MAGPIE_REDDIT_UA")
	}
	if len(missing) > 0 {
		return "", "", "", fmt.Errorf("vertical: reddit credentials partially configured, missing: %s", strings.Join(missing, ", "))
	}
	return id, secret, ua, nil
}

// redditToken exchanges client credentials for an app-only bearer token.
// ponytail: deliberately bypasses the Fetcher/proxy pool (FetchRequest is
// GET-only; adding a Method field for one POST is fetch/ creep) — one
// extra RTT per extract call, upgrade path = package-level token cache.
func redditToken(ctx context.Context, id, secret string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, redditTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("vertical: reddit token: %w", err)
	}
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("vertical: reddit token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body fully read above; close error unactionable
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("vertical: reddit token: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vertical: reddit token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var m struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &m); err != nil || m.AccessToken == "" {
		return "", fmt.Errorf("vertical: reddit token: no access_token in response")
	}
	return m.AccessToken, nil
}

// redditAuth fetches a token and returns the bearer + namespaced UA
// headers for oauth.reddit.com requests.
func redditAuth(ctx context.Context) ([]string, error) {
	id, secret, ua, err := redditCreds()
	if err != nil {
		return nil, err
	}
	tok, err := redditToken(ctx, id, secret)
	if err != nil {
		return nil, err
	}
	return []string{
		"Authorization: Bearer " + tok,
		"User-Agent: " + ua,
	}, nil
}
