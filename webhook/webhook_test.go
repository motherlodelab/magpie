package webhook

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hit is one request a sink saw.
type hit struct {
	id, ts, sig, ctype string
	body               []byte
}

// sink answers statuses in order (the last one repeats forever) and
// records every request. hdr, when non-nil, sets response headers per
// attempt index.
func sink(t *testing.T, hdr func(i int, h http.Header), statuses ...int) (*httptest.Server, func() []hit) {
	t.Helper()
	var mu sync.Mutex
	var seen []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // test sink
		mu.Lock()
		i := len(seen)
		seen = append(seen, hit{id: r.Header.Get("webhook-id"), ts: r.Header.Get("webhook-timestamp"),
			sig: r.Header.Get("webhook-signature"), ctype: r.Header.Get("Content-Type"), body: body})
		mu.Unlock()
		if hdr != nil {
			hdr(i, w.Header())
		}
		w.WriteHeader(statuses[min(i, len(statuses)-1)])
	}))
	t.Cleanup(srv.Close)
	return srv, func() []hit { mu.Lock(); defer mu.Unlock(); return append([]hit(nil), seen...) }
}

var fast = Options{Backoff: time.Millisecond}

// The published Standard Webhooks reference tuple (recomputed with
// Python hmac on 2026-10-03) — the one external anchor for the format.
func TestSign_StandardVector(t *testing.T) {
	got, err := Sign("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek",
		time.Unix(1614265330, 0), []byte(`{"test": 2432232314}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Errorf("Sign = %q, want %q", got, want)
	}
}

func TestSign_BadSecret(t *testing.T) {
	for _, s := range []string{"", "abc", "whsec_!!!"} {
		if _, err := Sign(s, "msg_1", time.Now(), []byte("{}")); err == nil {
			t.Errorf("Sign(%q) = nil error, want one", s)
		}
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "whsec_") {
		t.Fatalf("secret %q lacks whsec_", a)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "whsec_"))
	if err != nil || len(key) != 32 {
		t.Errorf("key = %d bytes (%v), want 32", len(key), err)
	}
	if a == b {
		t.Error("two NewSecret calls returned the same secret")
	}
}

func TestSend_SignedHeaders(t *testing.T) {
	secret, _ := NewSecret() //nolint:errcheck // crypto/rand never fails here
	srv, hits := sink(t, nil, 200)
	body := []byte(`{"type":"x"}`)
	r := Send(context.Background(), srv.URL, secret, "msg_abc", body, fast)
	if r.Err != nil || r.Status != 200 || r.Attempts != 1 {
		t.Fatalf("Send = %+v, want 200 / 1 attempt / nil", r)
	}
	h := hits()[0]
	if h.ctype != "application/json" || h.id != "msg_abc" || string(h.body) != string(body) {
		t.Errorf("hit = %+v", h)
	}
	ts, err := strconv.ParseInt(h.ts, 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Second {
		t.Errorf("webhook-timestamp %q not within 5 s of now", h.ts)
	}
	want, _ := Sign(secret, h.id, time.Unix(ts, 0), h.body) //nolint:errcheck // secret is valid
	if h.sig != want {
		t.Errorf("signature %q, want %q", h.sig, want)
	}
}

func TestSend_RetryThenOK(t *testing.T) {
	secret, _ := NewSecret() //nolint:errcheck // crypto/rand never fails here
	srv, hits := sink(t, func(i int, h http.Header) {
		if i == 1 {
			h.Set("Retry-After", "0")
		}
	}, 500, 429, 200)
	r := Send(context.Background(), srv.URL, secret, "msg_retry", []byte(`{}`), fast)
	if r.Err != nil || r.Status != 200 || r.Attempts != 3 {
		t.Fatalf("Send = %+v, want 200 after 3 attempts", r)
	}
	for i, h := range hits() {
		if h.id != "msg_retry" {
			t.Errorf("attempt %d id %q", i+1, h.id)
		}
		ts, _ := strconv.ParseInt(h.ts, 10, 64)                                     //nolint:errcheck // checked by the signature compare
		if want, _ := Sign(secret, h.id, time.Unix(ts, 0), h.body); h.sig != want { //nolint:errcheck // valid secret
			t.Errorf("attempt %d not re-signed against its own timestamp", i+1)
		}
	}
}

func TestSend_FinalStatuses(t *testing.T) {
	for _, code := range []int{302, 400, 410} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			other, otherHits := sink(t, nil, 200)
			srv, hits := sink(t, func(_ int, h http.Header) { h.Set("Location", other.URL) }, code)
			r := Send(context.Background(), srv.URL, "", "msg_f", []byte(`{}`), fast)
			if r.Attempts != 1 || r.Status != code || r.Err == nil || !strings.Contains(r.Err.Error(), "HTTP "+strconv.Itoa(code)) {
				t.Errorf("Send = %+v, want 1 attempt, HTTP %d", r, code)
			}
			if len(hits()) != 1 || len(otherHits()) != 0 {
				t.Errorf("hits = %d, redirect target hits = %d, want 1 and 0", len(hits()), len(otherHits()))
			}
		})
	}
}

func TestSend_GivesUp(t *testing.T) {
	srv, _ := sink(t, nil, 503)
	r := Send(context.Background(), srv.URL, "", "msg_g", []byte(`{}`), Options{Tries: 4, Backoff: time.Millisecond})
	if r.Attempts != 4 || r.Err == nil || r.Err.Error() != "HTTP 503" {
		t.Errorf("always-503 = %+v, want 4 attempts and HTTP 503", r)
	}
	// backoff v5 returns the raw *RetryAfterError on the try cap — the
	// status must still win.
	srv429, _ := sink(t, func(_ int, h http.Header) { h.Set("Retry-After", "0") }, 429)
	r = Send(context.Background(), srv429.URL, "", "msg_g", []byte(`{}`), Options{Tries: 2, Backoff: time.Millisecond})
	if r.Attempts != 2 || r.Err == nil || r.Err.Error() != "HTTP 429" {
		t.Errorf("always-429 = %+v (err %v), want exactly HTTP 429", r, r.Err)
	}
}

func TestSend_Unsigned(t *testing.T) {
	srv, hits := sink(t, nil, 200)
	if r := Send(context.Background(), srv.URL, "", "msg_u", []byte(`{}`), fast); r.Err != nil {
		t.Fatal(r.Err)
	}
	if h := hits()[0]; h.sig != "" || h.id != "msg_u" {
		t.Errorf("unsigned hit = %+v, want no signature and an id", h)
	}
	r := Send(context.Background(), "http://127.0.0.1:1/x", "", "msg_u", []byte(`{}`), Options{Tries: 2, Backoff: time.Millisecond})
	if r.Err == nil || strings.Contains(r.Err.Error(), "127.0.0.1:1/x") {
		t.Errorf("dead port err = %v, want non-nil and URL-free", r.Err)
	}
	if r.Attempts != 2 || r.Status != 0 {
		t.Errorf("dead port = %+v, want 2 attempts, status 0", r)
	}
}

func TestSend_BadSecretNoIO(t *testing.T) {
	srv, hits := sink(t, nil, 200)
	r := Send(context.Background(), srv.URL, "whsec_!!!", "msg_b", []byte(`{}`), fast)
	if r.Err == nil || r.Attempts != 0 || len(hits()) != 0 {
		t.Errorf("bad secret = %+v, hits %d — want error before any I/O", r, len(hits()))
	}
}

func TestSend_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the first 500 has been read and Send sits in its hour-long backoff wait.
	srv, _ := sink(t, func(int, http.Header) { time.AfterFunc(100*time.Millisecond, cancel) }, 500)
	start := time.Now()
	r := Send(ctx, srv.URL, "", "msg_c", []byte(`{}`), Options{Backoff: time.Hour})
	if time.Since(start) > time.Second {
		t.Errorf("Send took %v after cancel, want < 1 s", time.Since(start))
	}
	if !errors.Is(r.Err, context.Canceled) || r.Status != 500 {
		t.Errorf("Send = %+v (err %v), want context canceled with Status 500 kept", r, r.Err)
	}
}
