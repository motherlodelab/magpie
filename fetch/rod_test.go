package fetch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

// fakeBrowser is an executable that records it was launched, then fails:
// enough to prove ensureBrowser chose it, with no Chrome and no download.
func fakeBrowser(t *testing.T) (bin, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "marker")
	bin = filepath.Join(dir, "chrome")
	script := "#!/bin/sh\necho launched >> '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, marker
}

func withLookPath(t *testing.T, p string) {
	t.Helper()
	old := lookPath
	lookPath = func() (string, bool) { return p, true }
	t.Cleanup(func() { lookPath = old })
}

// QA §4: an installed Chrome/Edge is used before rod's unannounced ~150 MB
// download; a snap Chromium is skipped (it can't read rod's /tmp profile
// under confinement). The fake browser's marker proves which binary ran.
func TestRod_PrefersInstalledBrowser(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("the fake browser is a shell script")
	}
	t.Run("installed", func(t *testing.T) {
		bin, marker := fakeBrowser(t)
		withLookPath(t, bin)
		r := NewRodFetcher()
		r.launch = func(l *launcher.Launcher) {
			l.Delete(flags.Leakless)
			if l.Get(flags.Bin) == "" { // nothing chosen: fail fast, never download in a test
				l.Bin(filepath.Join(t.TempDir(), "no-chrome"))
			}
		}
		defer func() { _ = r.Close() }() //nolint:errcheck // the fake never connected
		if err := r.ensureBrowser(); err == nil {
			t.Fatal("ensureBrowser succeeded with a fake that exits 1")
		}
		// Vacuity: the error alone would also come from a failed download.
		if b, err := os.ReadFile(marker); err != nil || !strings.Contains(string(b), "launched") {
			t.Fatalf("marker = %q (%v): the installed browser %s never ran", b, err, bin)
		}
	})
	t.Run("snap skipped", func(t *testing.T) {
		withLookPath(t, "/snap/bin/chromium")
		var chosen string
		r := NewRodFetcher()
		r.launch = func(l *launcher.Launcher) {
			chosen = l.Get(flags.Bin)
			l.Delete(flags.Leakless).Bin(filepath.Join(t.TempDir(), "no-chrome")) // never download in a test
		}
		defer func() { _ = r.Close() }() //nolint:errcheck // nothing launched
		if err := r.ensureBrowser(); err == nil {
			t.Fatal("ensureBrowser launched a missing binary")
		}
		if chosen == "/snap/bin/chromium" {
			t.Fatal("ensureBrowser chose the snap Chromium")
		}
	})
}
