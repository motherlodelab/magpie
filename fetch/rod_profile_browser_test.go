//go:build browser

package fetch // internal: the launcher is unexported state

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher/flags"
)

// TestRod_CloseRemovesProfile: a local browser's temp profile — its cookie
// jar — is gone after Close.
func TestRod_CloseRemovesProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body><p>profile check</p></body></html>")) //nolint:errcheck // test server
	}))
	defer srv.Close()
	r := NewRodFetcher()
	if _, err := r.Fetch(t.Context(), FetchRequest{URL: srv.URL}); err != nil {
		if strings.Contains(err.Error(), "launch browser") || strings.Contains(err.Error(), "connect browser") {
			t.Skipf("no browser available: %v", err)
		}
		t.Fatalf("Fetch: %v", err)
	}
	dir := r.launcher.Get(flags.UserDataDir)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("profile dir %q missing while the browser runs: %v", dir, err) // vacuity guard
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("profile dir %q survives Close (stat err %v): a session jar would stay on disk", dir, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	cdp := &RodFetcher{CDP: "ws://127.0.0.1:1"} // never connected: no launcher, nothing to remove
	if err := cdp.Close(); err != nil {
		t.Fatalf("CDP Close: %v", err)
	}
}
