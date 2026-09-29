package vertical

// OverrideRedditEndpoints points the token POST and the oauth .json GETs
// at test servers. Returns a restore func — call as: defer restore().
func OverrideRedditEndpoints(tokenURL, oauthHost string) func() {
	oldT, oldH := redditTokenURL, redditOAuthHost
	redditTokenURL, redditOAuthHost = tokenURL, oauthHost
	return func() { redditTokenURL, redditOAuthHost = oldT, oldH }
}
