package fetch

// Parser pins for M0b cookie injection (pure — the browser delivery
// test lives in cookies_browser_test.go under the browser tag).
import "testing"

func TestCookieParams(t *testing.T) {
	got := cookieParams("https://Example.com:8080/page", "sid=abc; theme=dark mode=1; broken, novalue; empty=")
	if len(got) != 3 {
		t.Fatalf("params = %+v, want 3 pairs", got)
	}
	if got[0].Name != "sid" || got[0].Value != "abc" || got[0].Domain != "example.com" || got[0].Path != "/" {
		t.Errorf("pair 0 = %+v, want sid=abc scoped to example.com/", got[0])
	}
	if got[1].Name != "theme" || got[1].Value != "dark mode=1" {
		t.Errorf("pair 1 = %+v, want value split on the FIRST = only", got[1])
	}
	if got[2].Name != "empty" || got[2].Value != "" {
		t.Errorf("pair 2 = %+v, want empty value kept", got[2])
	}
	if params := cookieParams("not a url", "a=b"); len(params) != 1 || params[0].Domain != "" {
		t.Errorf("unparsable URL: %+v, want pair with empty domain", params)
	}
	if got := cookieParams("https://x.test/", "  ;  , ;"); got != nil {
		t.Errorf("junk-only input = %+v, want nil", got)
	}
}
