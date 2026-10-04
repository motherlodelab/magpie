package vertical

import "slices"

// OverrideRedditEndpoints points the token POST and the oauth .json GETs
// at test servers. Returns a restore func — call as: defer restore().
func OverrideRedditEndpoints(tokenURL, oauthHost string) func() {
	oldT, oldH := redditTokenURL, redditOAuthHost
	redditTokenURL, redditOAuthHost = tokenURL, oauthHost
	return func() { redditTokenURL, redditOAuthHost = oldT, oldH }
}

// SaveRegistry snapshots the package-global registry and returns its
// restore — call as: t.Cleanup(SaveRegistry()), so a test that Registers
// leaves only the built-ins behind and -count=N reruns stay clean.
func SaveRegistry() func() {
	saved := slices.Clone(registry)
	return func() { registry = saved }
}
