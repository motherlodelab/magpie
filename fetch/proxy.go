package fetch

// Proxy pool (Phase G): MAGPIE_PROXY_FILE (multi-entry, rotation) wins
// over MAGPIE_PROXY (single entry = 1-entry pool). One concrete struct,
// one implementation — no interface. The trust seam is proxiedForHost:
// non-nil proxyForHost ⇒ operator-chosen egress ⇒ dialPeerAllowed skips
// the peer check. Everything else stays peer-checked.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrProxyConfig marks every proxy pool/config failure (bad file, bad
// line, all entries dead, bad strategy). The CLI maps it to exit 2 —
// proxy misconfiguration is a usage error, never a page outcome.
var ErrProxyConfig = errors.New("fetch: bad proxy config")

// ProxyHelp is the canonical proxy URL scheme list for messages.
const ProxyHelp = "http(s)|socks5(h)://host:port or host:port:user:pass"

// proxyCooldown is how long a failed entry is skipped before it is
// retried (vendor failover semantics).
const proxyCooldown = 60 * time.Second

// poolEntry is one proxy line: the parsed URL plus the raw line (kept
// for redaction and {{session}} re-substitution).
type poolEntry struct {
	u   *url.URL
	raw string
}

// pool is the parse-once proxy pool: rotation, sticky sessions, and
// cooldown failover state. All mutation happens under mu.
type pool struct {
	entries  []poolEntry
	strategy string // round-robin (default) | sticky-host
	cooldown time.Duration
	mu       sync.Mutex
	rr       int               // round-robin cursor
	dead     map[int]time.Time // entry index → cooldown-until
	sessions map[string]string // target host → 8-hex sticky token
}

// parsePool parses pool-file content: one entry per line, '#' comments
// and blank lines skipped, errors name the 1-based line number. Line
// forms: URL (http|https|socks5|socks5h — stdlib treats socks5h the
// same as socks5, no normalization needed) or host:port:user:pass
// (vendor paste → http://user:pass@host:port).
func parsePool(content []byte) ([]poolEntry, error) {
	var out []poolEntry
	for i, line := range strings.Split(string(content), "\n") {
		ln := strings.TrimSpace(line)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		e, err := parsePoolLine(ln)
		if err != nil {
			return nil, fmt.Errorf("fetch: proxy pool line %d: %w", i+1, err)
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("fetch: proxy pool file has no entries (only comments/blank lines): %w", ErrProxyConfig)
	}
	return out, nil
}

// parsePoolLine parses one entry. Errors never embed the raw line —
// vendor pastes carry credentials, and url.Parse error text would leak
// them; the line number plus the form hint is the debugging surface.
func parsePoolLine(ln string) (poolEntry, error) {
	// {{session}} is not URL-legal in userinfo — validate the shape with
	// a placeholder, keep the template in raw.
	probe := strings.ReplaceAll(ln, "{{session}}", "00000000")
	if !strings.Contains(probe, "://") {
		parts := strings.SplitN(probe, ":", 4)
		if len(parts) != 4 || parts[0] == "" || parts[1] == "" {
			return poolEntry{}, fmt.Errorf("%w: want %s", ErrProxyConfig, ProxyHelp)
		}
		u, err := url.Parse("http://" + url.UserPassword(parts[2], parts[3]).String() + "@" + parts[0] + ":" + parts[1])
		if err != nil || u.Host == "" {
			return poolEntry{}, fmt.Errorf("%w: want %s", ErrProxyConfig, ProxyHelp)
		}
		return poolEntry{u: u, raw: ln}, nil
	}
	u, err := url.Parse(probe)
	if err != nil || u.Host == "" {
		return poolEntry{}, fmt.Errorf("%w: want %s", ErrProxyConfig, ProxyHelp)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return poolEntry{}, fmt.Errorf("%w: scheme %q not supported (want http|https|socks5|socks5h)", ErrProxyConfig, u.Scheme)
	}
	return poolEntry{u: u, raw: ln}, nil
}

// get selects one entry for hostname: dead entries are skipped, all-dead
// fails with a redacted endpoint list (never credentials). round-robin
// cycles the alive entries; sticky-host FNV-1a-hashes the hostname so
// the same target always rides the same entry.
func (p *pool) get(hostname string) (*url.URL, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	alive := make([]int, 0, len(p.entries))
	for i := range p.entries {
		if t, dead := p.dead[i]; dead && now.Before(t) {
			continue
		}
		alive = append(alive, i)
	}
	if len(alive) == 0 {
		ends := make([]string, len(p.entries))
		for i, e := range p.entries {
			ends[i] = RedactProxy(e.u)
		}
		return nil, 0, fmt.Errorf("fetch: all %d proxy pool entries in cooldown [%s]: %w", len(p.entries), strings.Join(ends, ", "), ErrProxyConfig)
	}
	var idx int
	if p.strategy == "sticky-host" {
		h := fnv.New32a()
		_, _ = h.Write([]byte(hostname)) //nolint:errcheck // hash.Write cannot fail
		idx = alive[int(h.Sum32())%len(alive)]
	} else { // round-robin (default)
		idx = alive[p.rr%len(alive)]
		p.rr++
	}
	u, err := p.resolve(p.entries[idx], hostname)
	return u, idx, err
}

// resolve substitutes {{session}} with a per-target-host stable token
// (sticky sessions for rotating-gateway vendors) and re-parses. The
// raw line was validated at parse time and substitution only inserts
// hex, so re-parse failure is unreachable — fall back to the parsed
// entry rather than erroring.
func (p *pool) resolve(e poolEntry, hostname string) (*url.URL, error) {
	if !strings.Contains(e.raw, "{{session}}") {
		return e.u, nil
	}
	raw := strings.ReplaceAll(e.raw, "{{session}}", p.sessionToken(hostname))
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return e.u, nil
	}
	return u, nil
}

// sessionToken returns the stable 8-hex token for a target host,
// minting it on first use.
func (p *pool) sessionToken(host string) string {
	if t, ok := p.sessions[host]; ok {
		return t
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// ponytail: crypto/rand failure needs a same-nanosecond host collision to matter.
		t := fmt.Sprintf("%08x", time.Now().UnixNano())[:8]
		p.sessions[host] = t
		return t
	}
	t := hex.EncodeToString(b[:])
	p.sessions[host] = t
	return t
}

// reportFailure puts an entry on cooldown after a dial/CONNECT failure.
func (p *pool) reportFailure(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead == nil {
		p.dead = map[int]time.Time{}
	}
	p.dead[idx] = time.Now().Add(p.cooldown)
}

// requestProxyKey carries a per-request proxy URL (FetchRequest.Proxy)
// through the http.Request context so proxyFunc honors it ahead of the
// env pool — flag-over-env, the house precedence. Context, not a
// transport field: the static client is shared, the override is per run.
type requestProxyKey struct{}

// requestProxyFrom extracts the per-request proxy override from a
// context (nil when absent). Consulted by proxyFunc AND the dial guard:
// when set, the dialed peer IS the operator-chosen proxy — the same
// trusted-egress decision a pool entry gets.
func requestProxyFrom(ctx context.Context) *url.URL {
	u, _ := ctx.Value(requestProxyKey{}).(*url.URL)
	return u
}

// ValidateRequestProxy parses a per-run proxy (FetchRequest.Proxy /
// scrape.Options.Proxy / crawl.Options.Proxy): pool-line grammar
// (http|https|socks5|socks5h URL, or host:port:user:pass). Typed
// ErrProxyConfig so callers map it to exit 2; the message names the
// field, never the raw value (vendor pastes carry credentials).
func ValidateRequestProxy(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	e, err := parsePoolLine(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: per-run proxy: want %s", ErrProxyConfig, ProxyHelp)
	}
	return e.u, nil
}

// RedactProxy renders a proxy URL as host:port — credentials never
// appear in errors, logs, or records.
func RedactProxy(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Host
}

// poolCache memoizes the parsed pool keyed by the full env
// configuration. Keyed per env VALUE (not per process) so tests that
// point MAGPIE_PROXY_FILE at different temp files get isolated pools.
var poolCache struct {
	mu  sync.Mutex
	key string
	p   *pool
	err error
}

// currentPool resolves the pool from the environment: MAGPIE_PROXY_FILE
// wins over MAGPIE_PROXY (which becomes a 1-entry pool). Neither set →
// nil pool (standard HTTP(S)_PROXY environment semantics apply).
func currentPool() (*pool, error) {
	file := strings.TrimSpace(os.Getenv("MAGPIE_PROXY_FILE"))
	single := strings.TrimSpace(os.Getenv("MAGPIE_PROXY"))
	strategy := strings.TrimSpace(os.Getenv("MAGPIE_PROXY_STRATEGY"))
	if file == "" && single == "" {
		return nil, nil
	}
	key := file + "\x00" + single + "\x00" + strategy
	poolCache.mu.Lock()
	defer poolCache.mu.Unlock()
	if poolCache.key == key {
		return poolCache.p, poolCache.err
	}
	p, err := buildPool(file, single, strategy)
	poolCache.key, poolCache.p, poolCache.err = key, p, err
	return p, err
}

func buildPool(file, single, strategy string) (*pool, error) {
	var entries []poolEntry
	if file != "" {
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("fetch: proxy pool file %s: %w", file, err)
		}
		if entries, err = parsePool(content); err != nil {
			return nil, err
		}
	} else {
		e, err := parsePoolLine(single)
		if err != nil {
			// Preserve the historical env-single message (tests pin it).
			return nil, fmt.Errorf("fetch: bad MAGPIE_PROXY %q: want %s: %w", single, ProxyHelp, ErrProxyConfig)
		}
		entries = []poolEntry{e}
	}
	if strategy == "" {
		strategy = "round-robin"
	}
	if strategy != "round-robin" && strategy != "sticky-host" {
		return nil, fmt.Errorf("fetch: MAGPIE_PROXY_STRATEGY %q must be round-robin|sticky-host: %w", strategy, ErrProxyConfig)
	}
	return &pool{
		entries:  entries,
		strategy: strategy,
		cooldown: proxyCooldown,
		sessions: map[string]string{},
	}, nil
}

// proxyForHostPick resolves the proxy for a target hostname, returning
// the owning pool + entry index so callers can report failures. Nil pool
// (nothing configured) falls through to the standard environment.
func proxyForHostPick(hostname string) (*url.URL, *pool, int, error) {
	p, err := currentPool()
	if err != nil {
		return nil, nil, 0, err
	}
	if p == nil {
		req := &http.Request{URL: &url.URL{Scheme: "https", Host: hostname}}
		u, err := http.ProxyFromEnvironment(req)
		return u, nil, 0, err
	}
	if noProxyMatch(hostname) {
		return nil, p, 0, nil
	}
	u, idx, err := p.get(hostname)
	return u, p, idx, err
}

// proxyForHost is the per-request seam: the stock transport's Proxy func
// AND the browser dial both consult it. MAGPIE_PROXY(_FILE) wins over
// the standard environment; NO_PROXY bypasses (peer check then applies —
// safe default).
func proxyForHost(hostname string) (*url.URL, error) {
	u, _, _, err := proxyForHostPick(hostname)
	return u, err
}

// proxiedForHost is the dial-guard trust decision: a non-nil proxy for
// this host means the connection's peer is operator-chosen egress (often
// loopback, e.g. Tor) and the SSRF peer check is skipped. One source of
// truth — derived from proxyForHost, never consulted separately.
func proxiedForHost(hostname string) bool {
	u, err := proxyForHost(hostname)
	return err == nil && u != nil
}

// pickRef records which pool entry the transport chose for one request —
// the failover loop reports failures against it and success surfaces the
// redacted endpoint on the response.
type pickRef struct {
	mu  sync.Mutex
	p   *pool
	idx int
	u   *url.URL
}

type pickKey struct{}

func pickRefFrom(ctx context.Context) *pickRef {
	v, _ := ctx.Value(pickKey{}).(*pickRef)
	return v
}

func (r *pickRef) set(p *pool, idx int, u *url.URL) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.p, r.idx, r.u = p, idx, u
}

func (r *pickRef) chosen() (*pool, int, *url.URL) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.p, r.idx, r.u
}
