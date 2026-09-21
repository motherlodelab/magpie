package fetch

import (
	"context"
	"net/http"
	"time"
)

// FetchRequest is a single fetch job.
type FetchRequest struct {
	URL     string
	Timeout time.Duration // per-attempt budget; 0 = 20s default
	Profile string        // header profile: default|chrome|firefox (unknown = default)
	Browser string        // TLS fingerprint: chrome|firefox|random ("" = stock TLS)
	Cookies string        // raw Cookie header value, passed through verbatim
	// Lang overrides the profile's Accept-Language verbatim (no BCP47
	// policing — validation rejects control characters at the options
	// boundary). Rod sets it as a page-level extra header.
	Lang string
	// Headers are raw "Name: value" request headers applied on the
	// static path AFTER the profile bundle, Lang, and cookies — the
	// caller wins conflicts. Shape/control-character policing lives at
	// the options boundary (scrape.ValidateOptions); malformed lines
	// (no colon) are skipped here.
	Headers []string
	// Proxy is a per-run egress override (http|https|socks5(h)://host:port
	// or host:port:user:pass — pool-line grammar) that beats the env
	// pool for THIS request (flag-over-env precedence). Validated
	// pre-I/O: fetch.ValidateRequestProxy → ErrProxyConfig family. Rod
	// path: launcher --proxy-server (credentials unsupported there —
	// Chromium's flag grammar has no inline auth).
	Proxy string
	// CaptureXHR lists Go regexps; matching XHR/fetch response bodies are
	// captured into FetchResponse.XHR. Rod-only: static fetchers ignore
	// this (CLI/MCP reject the combination at the options boundary).
	CaptureXHR []string
}

// FetchResponse is the fetched page.
type FetchResponse struct {
	URL        string
	FinalURL   string
	StatusCode int
	HTML       []byte
	Headers    http.Header
	// Proxy is the redacted host:port of the pool/MAGPIE_PROXY entry
	// that served this response; "" when direct. Pools rotate per
	// request (and per redirect hop), so this is the entry that served
	// the final hop — a rotation audit trail it is not.
	Proxy string
	// XHR carries captured XHR/fetch response bodies (rod-only; nil on
	// the static path). Additive + omitempty: existing envelopes and
	// goldens never drift.
	XHR []XHRCapture
}

// Fetcher fetches one URL. Rod lives behind this interface.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error)
	CanHandle(req FetchRequest) bool
	Close() error
}

func budget(req FetchRequest) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return 20 * time.Second
}
