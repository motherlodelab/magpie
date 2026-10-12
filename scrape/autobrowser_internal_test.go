package scrape

import (
	"context"
	"errors"
	"testing"

	"github.com/motherlodelab/magpie/fetch"
)

// TestFetchURL_AutoWithoutBrowserKeepsStatic (S4-core K8): with the
// embedder's AskBeforeDownload set and no browser, render=auto escalating a
// thin page (example.com scores NeedsBrowser) failed the whole scrape with
// ErrNoBrowser although the static page was in hand. Auto falls back to it;
// render=browser still fails loudly.
func TestFetchURL_AutoWithoutBrowserKeepsStatic(t *testing.T) {
	old := autoBrowser
	autoBrowser = func(context.Context, string, Options) (*fetch.FetchResponse, error) { return nil, fetch.ErrNoBrowser }
	t.Cleanup(func() { autoBrowser = old })
	thin := []byte(`<html><body><h1>Example Domain</h1><p>Short.</p></body></html>`)
	if s, _ := fetch.ScoreJSRequired(thin, nil); !fetch.NeedsBrowser(s) {
		t.Fatalf("fixture scores %d: it must escalate under auto, or the test proves nothing", s)
	}
	vf := capturingFetcher{fn: func(context.Context, fetch.FetchRequest) (*fetch.FetchResponse, error) {
		return &fetch.FetchResponse{URL: "https://example.com/", StatusCode: 200, HTML: thin}, nil
	}}
	resp, err := fetchURL(t.Context(), vf, "https://example.com/", "auto", Options{})
	if err != nil || resp == nil || string(resp.HTML) != string(thin) {
		t.Fatalf("auto without a browser = %v, %v; want the static page", resp, err)
	}
	other := errors.New("launch: boom")
	autoBrowser = func(context.Context, string, Options) (*fetch.FetchResponse, error) { return nil, other }
	if _, err := fetchURL(t.Context(), vf, "https://example.com/", "auto", Options{}); !errors.Is(err, other) {
		t.Errorf("auto with another browser error = %v, want it returned (only ErrNoBrowser falls back)", err)
	}
}
