// Package webhook delivers one JSON message to one receiver: Standard
// Webhooks v1 signing (HMAC-SHA256 over "id.timestamp.body") and bounded
// retry with backoff. It owns delivery only — who receives which event,
// registries and delivery logs belong to the caller.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/motherlodelab/magpie/fetch"
)

const secretPrefix = "whsec_"

// Options bounds one Send; zero values are the defaults.
type Options struct {
	Tries   int           // total attempts; 0 = 4 (1 + 3 retries)
	Backoff time.Duration // first retry wait; 0 = 2s (×3 per retry, ±50% jitter, ≤30s apart, ≤2m total)
}

// Result is one Send's outcome. Err is nil only on a 2xx.
type Result struct {
	Status   int           // last HTTP status; 0 = no response
	Attempts int           // 0 only when the secret was malformed
	Latency  time.Duration // the last attempt's round trip
	Err      error
}

// NewSecret returns a fresh signing secret: "whsec_" + base64 of 32
// crypto/rand bytes (the Standard Webhooks shape).
func NewSecret() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("webhook: new secret: %w", err)
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(key), nil
}

// Sign returns the webhook-signature value "v1,<base64 HMAC-SHA256>" of
// "<id>.<unix ts>.<body>" under secret ("whsec_<base64 key>").
func Sign(secret, id string, ts time.Time, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if !strings.HasPrefix(secret, secretPrefix) || err != nil || len(key) == 0 {
		return "", fmt.Errorf("webhook: secret must be %s followed by base64", secretPrefix)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts.Unix(), 10) + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil)), nil
}

// Send POSTs body to rawURL as one message: the same webhook-id on every
// attempt, a fresh webhook-timestamp (and signature, when secret != "")
// per attempt. 2xx = delivered. Transport errors, 408, 429 and 5xx retry
// with backoff (429/503 honour an integer Retry-After); 3xx is never
// followed and, like every other status, is final. ctx cancels during
// attempts and backoff. Errors never carry the URL: for Slack/Discord the
// URL is the secret.
//
// ponytail: delivery is in-process — a quit during backoff loses the
// message. Upgrade path: the caller persists the pending delivery and
// resumes it at startup.
func Send(ctx context.Context, rawURL, secret, id string, body []byte, o Options) Result {
	var res Result
	if secret != "" {
		if _, err := Sign(secret, id, time.Now(), body); err != nil {
			res.Err = err
			return res
		}
	}
	tries := o.Tries
	if tries <= 0 {
		tries = 4
	}
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 3
	b.RandomizationFactor = 0.5
	b.MaxInterval = 30 * time.Second
	// Capped: backoff never caps the FIRST interval, and a first wait past
	// the 2m elapsed budget would silently mean "give up after one try".
	b.InitialInterval = min(o.Backoff, b.MaxInterval)
	if b.InitialInterval <= 0 {
		b.InitialInterval = 2 * time.Second
	}

	// Built INLINE with AllowPrivate: a webhook URL is operator-chosen (a
	// localhost n8n is the main use case) — the searxng precedent. Never
	// hoist this transport into a shared var: a future caller would
	// inherit the private-net allowance. Redirects are never followed: Go
	// would turn the POST into a GET, and the spec calls 3xx a failure.
	client := &http.Client{
		Transport:     fetch.GuardedTransportWithOptions(fetch.SSRFOptions{AllowPrivate: true}),
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	op := func() (struct{}, error) {
		res.Attempts++
		res.Status = 0
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
		if err != nil {
			return struct{}{}, backoff.Permanent(errors.New("webhook: invalid URL"))
		}
		now := time.Now()
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "magpie-webhooks")
		req.Header.Set("webhook-id", id)
		req.Header.Set("webhook-timestamp", strconv.FormatInt(now.Unix(), 10))
		if secret != "" {
			sig, _ := Sign(secret, id, now, body) //nolint:errcheck // validated before the first attempt
			req.Header.Set("webhook-signature", sig)
		}
		resp, err := client.Do(req)
		res.Latency = time.Since(now)
		if err != nil {
			return struct{}{}, classify(ctx, nil, err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) //nolint:errcheck // bounded drain; a hostile receiver can't stall us
		_ = resp.Body.Close()                                         //nolint:errcheck // drain-close; failure unactionable
		res.Status = resp.StatusCode
		return struct{}{}, classify(ctx, resp, nil)
	}
	_, err := backoff.Retry(ctx, op,
		backoff.WithBackOff(b),
		backoff.WithMaxTries(uint(tries)),
		backoff.WithMaxElapsedTime(2*time.Minute),
	)
	if err != nil {
		// Never Retry's error as-is: on the try cap it is the raw last
		// error (a *RetryAfterError reads "retry after 1s" and loses the
		// status), and a cancel during the backoff wait must read
		// canceled, not the previous attempt's status.
		var pe *backoff.PermanentError
		switch {
		case context.Cause(ctx) != nil:
			res.Err = context.Cause(ctx)
		case res.Status != 0:
			res.Err = fmt.Errorf("HTTP %d", res.Status)
		case errors.As(err, &pe):
			res.Err = pe.Err
		default:
			res.Err = err
		}
	}
	return res
}

// classify maps one attempt to nil (2xx), a retryable error (transport,
// 408, 429, 5xx; RetryAfter when 429/503 carry an integer Retry-After),
// or backoff.Permanent (every other status, a canceled ctx). Transport
// errors lose their *url.Error wrapper, which embeds the URL.
func classify(ctx context.Context, resp *http.Response, err error) error {
	if err != nil {
		if ctx.Err() != nil {
			return backoff.Permanent(ctx.Err())
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	code := resp.StatusCode
	switch {
	case code/100 == 2:
		return nil
	case code == 429 || code == 503:
		if n, aerr := strconv.Atoi(resp.Header.Get("Retry-After")); aerr == nil && n >= 0 {
			return backoff.RetryAfter(n)
		}
		return fmt.Errorf("HTTP %d", code)
	case code == 408 || code/100 == 5:
		return fmt.Errorf("HTTP %d", code)
	}
	return backoff.Permanent(fmt.Errorf("HTTP %d", code))
}
