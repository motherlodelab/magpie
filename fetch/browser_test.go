package fetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

// QA §3 (K6): the consent hook. These tests swap package globals
// (lookPath, launcher.DefaultBrowserDir, AskBeforeDownload), so none of
// them runs in parallel. No test downloads Chromium or launches a browser.

// withNoBrowser: no installed browser and an empty rod download dir, so
// ensureBrowser has nothing to launch and nothing already downloaded.
func withNoBrowser(t *testing.T) {
	t.Helper()
	oldLP, oldDir := lookPath, launcher.DefaultBrowserDir
	lookPath = func() (string, bool) { return "", false }
	launcher.DefaultBrowserDir = t.TempDir()
	t.Cleanup(func() { lookPath, launcher.DefaultBrowserDir = oldLP, oldDir })
}

// countingHook installs AskBeforeDownload and counts its calls.
func countingHook(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := AskBeforeDownload
	AskBeforeDownload = func() { n.Add(1) }
	t.Cleanup(func() { AskBeforeDownload = old })
	return &n
}

// failFastLaunch: an unset Bin points at a missing file, so Launch fails
// instead of downloading (the TestRod_PrefersInstalledBrowser stub).
func failFastLaunch(t *testing.T) func(*launcher.Launcher) {
	return func(l *launcher.Launcher) {
		l.Delete(flags.Leakless)
		if l.Get(flags.Bin) == "" {
			l.Bin(filepath.Join(t.TempDir(), "no-chrome"))
		}
	}
}

func skipNoShell(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("the fake browser is a shell script")
	}
}

// With a consent hook set (the desktop), a box with no browser and no
// download asks instead of fetching ~150 MB — and never reaches the launcher.
func TestEnsureBrowser_AsksBeforeDownload(t *testing.T) {
	withNoBrowser(t)
	calls := countingHook(t)
	r := NewRodFetcher()
	r.launch = func(*launcher.Launcher) {
		t.Fatal("launcher reached: the hook must stop before any launch or download")
	}
	defer func() { _ = r.Close() }() //nolint:errcheck // never launched

	err := r.ensureBrowser()
	if !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("err = %v, want ErrNoBrowser", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("hook calls = %d, want 1", n)
	}
	// Not cached: a second run asks again (the user may have said Not now).
	_ = r.ensureBrowser() //nolint:errcheck // asserted above
	if n := calls.Load(); n != 2 {
		t.Fatalf("hook calls after retry = %d, want 2 (ensureBrowser must not cache a refusal)", n)
	}
}

// Vacuity + the CLI contract: with no hook, the same box reaches the
// launcher (rod's download path) exactly as before — the stub stops it.
func TestEnsureBrowser_CLIUnchanged(t *testing.T) {
	withNoBrowser(t)
	var reached atomic.Bool
	r := NewRodFetcher()
	stub := failFastLaunch(t)
	r.launch = func(l *launcher.Launcher) { reached.Store(true); stub(l) }
	defer func() { _ = r.Close() }() //nolint:errcheck // never connected

	err := r.ensureBrowser()
	if errors.Is(err, ErrNoBrowser) {
		t.Fatal("ErrNoBrowser without a hook: the CLI must keep downloading")
	}
	if !reached.Load() {
		t.Fatal("launcher never reached")
	}
}

// An installed browser never consults the hook.
func TestEnsureBrowser_InstalledSkipsHook(t *testing.T) {
	skipNoShell(t)
	bin, _ := fakeBrowser(t) // exits 1: the launch fails, the hook must not run
	withLookPath(t, bin)
	calls := countingHook(t)
	r := NewRodFetcher()
	r.launch = func(l *launcher.Launcher) { l.Delete(flags.Leakless) }
	defer func() { _ = r.Close() }() //nolint:errcheck // the fake never connected
	if err := r.ensureBrowser(); errors.Is(err, ErrNoBrowser) {
		t.Fatalf("err = %v: an installed browser is not 'no browser'", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("hook calls = %d, want 0", n)
	}
}

func TestBrowserReady(t *testing.T) {
	skipNoShell(t)
	withNoBrowser(t)
	if BrowserReady() {
		t.Fatal("ready with no browser and an empty download dir")
	}
	bin, _ := fakeBrowser(t)
	withLookPath(t, bin)
	if !BrowserReady() {
		t.Fatal("not ready with an installed browser")
	}
}

// A cancelled ctx stops the download before any request leaves the box,
// and the error reaches the caller.
func TestDownloadBrowser_Cancelled(t *testing.T) {
	withNoBrowser(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- DownloadBrowser(ctx, func(string) {}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("DownloadBrowser with a cancelled ctx succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DownloadBrowser ignored the cancelled ctx")
	}
}

// rod's progress lines reach the callback as plain text.
func TestLineLogger(t *testing.T) {
	var got []string
	lineLogger(func(s string) { got = append(got, s) }).Println("Progress:", "42%")
	if len(got) != 1 || got[0] != "Progress: 42%" {
		t.Fatalf("lines = %q, want [\"Progress: 42%%\"]", got)
	}
}
