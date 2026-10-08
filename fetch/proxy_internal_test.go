package fetch

// Pool internals (Phase G G.1): parsePool/strategy/cooldown/{{session}}/
// RedactProxy tables. Package fetch — the pool's unexported glue is
// where G.1's security-adjacent decisions live (parsePool redaction,
// cooldown, substitution); same justification as
// ssrf_internal_test.go/utls_internal_test.go.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

func TestParsePool(t *testing.T) {
	good := []struct {
		name  string
		line  string
		check func(t *testing.T, e poolEntry)
	}{
		{"http url", "http://proxy.example:3128", func(t *testing.T, e poolEntry) {
			if e.u.Host != "proxy.example:3128" || e.u.Scheme != "http" {
				t.Errorf("u = %v", e.u)
			}
		}},
		{"https url", "https://proxy.example", func(t *testing.T, e poolEntry) {
			if e.u.Scheme != "https" {
				t.Errorf("scheme = %s", e.u.Scheme)
			}
		}},
		{"socks5 tor", "socks5://127.0.0.1:9050", func(t *testing.T, e poolEntry) {
			if e.u.Scheme != "socks5" || e.u.Host != "127.0.0.1:9050" {
				t.Errorf("u = %v", e.u)
			}
		}},
		// Drift pin: stdlib treats socks5h the same as socks5
		// (net/http/transport.go: "socks5 is treated the same as socks5h"),
		// so both parse verbatim — if the stdlib union ever narrows, this
		// row and the docs change together.
		{"socks5h verbatim", "socks5h://127.0.0.1:9050", func(t *testing.T, e poolEntry) {
			if e.u.Scheme != "socks5h" {
				t.Errorf("scheme = %s, want socks5h preserved (stdlib handles it natively)", e.u.Scheme)
			}
		}},
		{"vendor paste", "1.2.3.4:8000:user:secret", func(t *testing.T, e poolEntry) {
			if e.u.Scheme != "http" || e.u.Host != "1.2.3.4:8000" {
				t.Errorf("u = %v", e.u)
			}
			if p, _ := e.u.User.Password(); e.u.User.Username() != "user" || p != "secret" {
				t.Errorf("userinfo = %v", e.u.User)
			}
		}},
		{"session template", "http://u:{{session}}@gw.example:7000", func(t *testing.T, e poolEntry) {
			if !strings.Contains(e.raw, "{{session}}") {
				t.Errorf("raw = %q, want the template preserved", e.raw)
			}
		}},
	}
	for _, c := range good {
		t.Run(c.name, func(t *testing.T) {
			entries, err := parsePool([]byte("# comment\n\n" + c.line + "\n# trailing\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1 (comments/blanks skipped)", len(entries))
			}
			c.check(t, entries[0])
		})
	}
	bad := []struct{ name, line string }{
		{"ftp scheme", "ftp://p.example:3128"},
		{"no host", "http://"},
		{"too few fields", "1.2.3.4:8000"},
		{"bare host", "proxy.example"},
	}
	for _, c := range bad {
		t.Run("bad/"+c.name, func(t *testing.T) {
			if _, err := parsePool([]byte(c.line)); err == nil {
				t.Fatalf("parsePool(%q) = nil error, want loud failure", c.line)
			}
		})
	}
}

// TestParsePool_LineNumbersAndRedaction: errors name the 1-based line
// and never echo credential-bearing line content.
func TestParsePool_LineNumbersAndRedaction(t *testing.T) {
	const secret = "hunter2password"
	content := "# header comment\n\nhttp://ok.example:3128\nnonsense-line\n"
	_, err := parsePool([]byte(content))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "line 4") {
		t.Errorf("err = %v, want the 1-based line number 4", err)
	}
	credErr := "http://" + secret + "@\x7f:1\n" // control char ⇒ url.Parse error; raw must never echo
	_, err = parsePool([]byte(credErr))
	if err == nil {
		t.Fatal("want error for unparseable URL line")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("credential %q leaked in parse error: %v", secret, err)
	}
}

// TestParsePool_EmptyFileLoud: an all-comment file must fail, not
// silently become a zero-entry pool.
func TestParsePool_EmptyFileLoud(t *testing.T) {
	for _, content := range []string{"", "# only comments\n\n", "\n\n"} {
		if _, err := parsePool([]byte(content)); err == nil {
			t.Errorf("parsePool(%q) = nil error, want loud failure", content)
		}
	}
}

func testPool(t *testing.T, lines ...string) *pool {
	t.Helper()
	p, err := buildPoolFile(t, strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func buildPoolFile(t *testing.T, content string) (*pool, error) {
	t.Helper()
	entries, err := parsePool([]byte(content))
	if err != nil {
		return nil, err
	}
	return &pool{entries: entries, strategy: "round-robin", cooldown: 60 * time.Second, sessions: map[string]string{}}, nil
}

// TestPool_RoundRobin cycles 1→2→3→1 deterministically.
func TestPool_RoundRobin(t *testing.T) {
	p := testPool(t, "http://a:1", "http://b:2", "http://c:3")
	var got []string
	for i := 0; i < 4; i++ {
		u, _, err := p.get("target.example")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, u.Host)
	}
	want := []string{"a:1", "b:2", "c:3", "a:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cycle = %v, want %v", got, want)
		}
	}
}

// TestPool_StickyHost: the same host twice rides the same entry; five
// fixed hostnames touch more than one entry (FNV is deterministic —
// no statistics).
func TestPool_StickyHost(t *testing.T) {
	p := testPool(t, "http://a:1", "http://b:2", "http://c:3")
	p.strategy = "sticky-host"
	hosts := []string{"one.example", "two.example", "three.example", "four.example", "five.example"}
	touched := map[string]bool{}
	for _, h := range hosts {
		u1, _, err := p.get(h)
		if err != nil {
			t.Fatal(err)
		}
		u2, _, err := p.get(h)
		if err != nil {
			t.Fatal(err)
		}
		if u1.Host != u2.Host {
			t.Errorf("host %s: %s then %s — sticky must be stable", h, u1.Host, u2.Host)
		}
		touched[u1.Host] = true
	}
	if len(touched) < 2 {
		t.Errorf("5 hostnames touched %d entry, want >1 (deterministic spread)", len(touched))
	}
}

// TestPool_CooldownSkipAndRestore: a failed entry is skipped, then
// returns after the cooldown window (field set to 30ms; real clock).
func TestPool_CooldownSkipAndRestore(t *testing.T) {
	p := testPool(t, "http://dead:1", "http://alive:2")
	p.cooldown = 30 * time.Millisecond
	p.reportFailure(0)
	for i := 0; i < 3; i++ {
		u, idx, err := p.get("target.example")
		if err != nil {
			t.Fatal(err)
		}
		if u.Host != "alive:2" || idx != 1 {
			t.Fatalf("got %s (idx %d), want the alive entry only", u.Host, idx)
		}
	}
	time.Sleep(50 * time.Millisecond)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		u, _, err := p.get("target.example")
		if err != nil {
			t.Fatal(err)
		}
		seen[u.Host] = true
	}
	if !seen["dead:1"] {
		t.Errorf("after cooldown the failed entry never returned (got %v)", seen)
	}
}

// TestPool_AllDead_Redacted: the all-dead error lists host:port only —
// the grep belt over the redaction contract.
func TestPool_AllDead_Redacted(t *testing.T) {
	const secret = "hunter2password"
	p := testPool(t, "1.2.3.4:8000:user:"+secret, "http://5.6.7.8:3128")
	for i := range p.entries {
		p.reportFailure(i)
	}
	_, _, err := p.get("target.example")
	if err == nil {
		t.Fatal("all-dead pool must error")
	}
	if !strings.Contains(err.Error(), "1.2.3.4:8000") || !strings.Contains(err.Error(), "5.6.7.8:3128") {
		t.Errorf("err = %v, want redacted host:port endpoint list", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("credential %q leaked in all-dead error: %v", secret, err)
	}
	if !errors.Is(err, ErrProxyConfig) {
		t.Errorf("all-dead error must wrap ErrProxyConfig: %v", err)
	}
}

// TestPool_Session: {{session}} resolves to a stable 8-hex token per
// target host; different hosts get different tokens.
func TestPool_Session(t *testing.T) {
	p := testPool(t, "http://u:{{session}}@gw.example:7000")
	u1, _, err := p.get("target-a.example")
	if err != nil {
		t.Fatal(err)
	}
	u2, _, err := p.get("target-a.example")
	if err != nil {
		t.Fatal(err)
	}
	u3, _, err := p.get("target-b.example")
	if err != nil {
		t.Fatal(err)
	}
	if u1.String() != u2.String() {
		t.Errorf("same host resolved %s then %s — session must be sticky", u1, u2)
	}
	if u3.String() == u1.String() {
		t.Errorf("different host resolved identical URL %s — token must vary per host", u1)
	}
	if u1.User.Username() != "u" {
		t.Fatalf("username = %q, want substituted template", u1.User.Username())
	}
	tok, _ := u1.User.Password()
	if len(tok) != 8 {
		t.Fatalf("token = %q, want 8 chars", tok)
	}
	for _, c := range tok {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("token %q contains non-hex %q, want ^[0-9a-f]{8}$", tok, c)
		}
	}
}

func TestRedactProxy(t *testing.T) {
	mustURL := func(s string) *url.URL {
		t.Helper()
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	cases := []struct {
		in   *url.URL
		want string
	}{
		{mustURL("http://user:pass@1.2.3.4:8080"), "1.2.3.4:8080"},
		{mustURL("socks5://u:p@127.0.0.1:9050"), "127.0.0.1:9050"},
		{mustURL("http://plain.example:3128"), "plain.example:3128"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := RedactProxy(c.in); got != c.want {
			t.Errorf("RedactProxy(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestProxiedForHost pins the trust seam: pool/env presence ⇒ proxied;
// NO_PROXY naming the host ⇒ not proxied (peer check applies — safe
// default). The nothing-set case is deliberately unasserted: it falls
// through to ProxyFromEnvironment, whose env snapshot is process-cached.
func TestProxiedForHost(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pool.txt")
	if err := os.WriteFile(file, []byte("socks5://127.0.0.1:9050\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", file)
	t.Setenv("NO_PROXY", "")
	if !proxiedForHost("target.example") {
		t.Error("pool set ⇒ proxiedForHost must be true (operator-chosen egress)")
	}
	t.Setenv("NO_PROXY", "target.example")
	if proxiedForHost("target.example") {
		t.Error("NO_PROXY-exempt host must NOT be proxied — the peer check applies")
	}
	t.Setenv("MAGPIE_PROXY_FILE", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("MAGPIE_PROXY", "http://127.0.0.1:3128")
	if !proxiedForHost("target.example") {
		t.Error("env-single proxy ⇒ proxiedForHost must be true")
	}
}

// TestPool_CacheKeyedByEnv: changing MAGPIE_PROXY_FILE re-parses —
// a process-once singleton would poison every t.Setenv test.
func TestPool_CacheKeyedByEnv(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("http://10.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("http://10.0.0.2:2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_STRATEGY", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", a)
	pa, err := currentPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY_FILE", b)
	pb, err := currentPool()
	if err != nil {
		t.Fatal(err)
	}
	if pa.entries[0].u.Host != "10.0.0.1:1" || pb.entries[0].u.Host != "10.0.0.2:2" {
		t.Errorf("cache not keyed by env value: %v vs %v", pa.entries[0].u.Host, pb.entries[0].u.Host)
	}
}

// TestBuildPool_StrategyValidation: unknown strategies fail loud.
func TestBuildPool_StrategyValidation(t *testing.T) {
	if _, err := buildPool("", "http://1.2.3.4:1", "random-walk"); err == nil {
		t.Error("unknown strategy must fail loud")
	} else if !errors.Is(err, ErrProxyConfig) {
		t.Errorf("err = %v, want ErrProxyConfig wrap", err)
	}
}

// TestPool_FailoverMarksCooldown pins the END-TO-END wiring: a failed
// pooled fetch must put every dead entry on cooldown through
// doWithFailover → reportFailure. Regression for the pickRef-shadowing
// bug where do() overwrote the caller's ref and reportFailure was
// unreachable (failover only worked by round-robin coincidence).
func TestPool_FailoverMarksCooldown(t *testing.T) {
	deadPort := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a := l.Addr().String()
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		return a
	}
	poolPath := filepath.Join(t.TempDir(), "pool.txt")
	content := "http://" + deadPort() + "\nhttp://" + deadPort() + "\n"
	if err := os.WriteFile(poolPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_PROXY", "")
	t.Setenv("MAGPIE_PROXY_FILE", poolPath)
	t.Setenv("MAGPIE_PROXY_STRATEGY", "")
	t.Setenv("NO_PROXY", "")
	s, err := NewStaticFetcherWithOptions(SSRFOptions{AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := currentPool()
	if err != nil {
		t.Fatal(err)
	}
	_, ferr := s.doWithFailover(t.Context(), FetchRequest{URL: "http://" + deadPort() + "/x"})
	if ferr == nil {
		t.Fatal("all-dead pool must error")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dead) != 2 {
		t.Fatalf("cooldown map = %v, want BOTH entries marked by the real fetch path", p.dead)
	}
}

// TestRod_AuthProxyRefused — QA S8 + S2b N4: --proxy-server has no
// credential grammar, so the browser path refuses an authenticated proxy
// (instead of a 407 later), and refuses a per-run proxy over CDP (instead
// of silently dropping it). Chrome never starts: the launch seam points at
// a missing binary, so a pre-fix run fails fast with fork/exec, and the
// refusal must come before any launch. The error never echoes the password.
func TestRod_AuthProxyRefused(t *testing.T) {
	noChrome := func(l *launcher.Launcher) { l.Delete(flags.Leakless).Bin(filepath.Join(t.TempDir(), "no-chrome")) }
	for _, c := range []struct{ name, proxy, cdp string }{
		{"http userinfo", "http://alice:s3cret@127.0.0.1:1", ""},
		{"pool line", "127.0.0.1:1:alice:s3cret", ""},
		{"socks5 userinfo", "socks5://alice:s3cret@127.0.0.1:1", ""},
		{"proxy over CDP", "http://127.0.0.1:2", "ws://127.0.0.1:1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := NewRodFetcher()
			r.Proxy, r.CDP, r.launch = c.proxy, c.cdp, noChrome
			defer func() { _ = r.Close() }() //nolint:errcheck // nothing launched on the refused path
			_, err := r.Fetch(context.Background(), FetchRequest{URL: "http://127.0.0.1:1/"})
			if !errors.Is(err, ErrProxyConfig) {
				t.Fatalf("err = %v, want ErrProxyConfig before any launch", err)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error echoes the proxy password: %v", err)
			}
		})
	}
}
