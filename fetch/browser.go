package fetch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-rod/rod/lib/launcher"
)

// ErrNoBrowser: no installed Chrome/Edge, no downloaded Chromium, and the
// caller asked to be consulted first (AskBeforeDownload).
var ErrNoBrowser = errors.New("fetch: no browser installed — this page needs Chrome, Edge or a one-time Chromium download (about 150 MB)")

// AskBeforeDownload, when non-nil, replaces rod's silent first-use download
// (QA §3): ensureBrowser calls it and fails with ErrNoBrowser, and the
// embedder offers DownloadBrowser. The CLI leaves it nil and downloads.
// ponytail: one consent policy per process (a package hook); upgrade: a
// fetch.Options field if a process ever needs two.
var AskBeforeDownload func()

// BrowserReady: an installed browser rod can drive, or rod's downloaded copy.
func BrowserReady() bool {
	if installedBrowser() != "" {
		return true
	}
	return launcher.NewBrowser().Validate() == nil
}

// DownloadBrowser fetches rod's pinned Chromium, passing rod's own progress
// lines ("Progress: 42%", at most one a second) to progress.
func DownloadBrowser(ctx context.Context, progress func(string)) error {
	b := launcher.NewBrowser()
	b.Context, b.Logger = ctx, lineLogger(progress)
	if _, err := b.Get(); err != nil {
		return fmt.Errorf("fetch: download browser: %w", err)
	}
	return nil
}

// installedBrowser is an installed Chrome/Edge rod can drive, or "".
// Snap Chromium can't read rod's /tmp profile dir under confinement.
func installedBrowser() string {
	if p, ok := lookPath(); ok && !strings.HasPrefix(p, "/snap/") {
		return p
	}
	return ""
}

// lineLogger adapts a line callback to rod's utils.Logger.
type lineLogger func(string)

func (f lineLogger) Println(vs ...interface{}) { f(strings.TrimSpace(fmt.Sprintln(vs...))) }
