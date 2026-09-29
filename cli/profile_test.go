package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase Z.5 profile tests — dirs injected via the profilePath var
// (DefaultConfigDir is unreachable in tests); all hermetic.

func withProfileStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := profilePath
	profilePath = func() string { return filepath.Join(dir, "profiles.json") }
	t.Cleanup(func() { profilePath = old })
	return dir
}

func TestProfile_StoreRoundTrip(t *testing.T) {
	dir := withProfileStore(t)

	// save → 0600 file on create
	if err := saveProfiles(map[string]Profile{"gh": {Cookies: "sess=abc", Headers: []string{"X-Token: sekrit"}}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600", fi.Mode().Perm())
	}

	// upsert, not append
	if err := saveProfiles(map[string]Profile{"gh": {Cookies: "sess=xyz"}}); err != nil {
		t.Fatal(err)
	}
	m, err := loadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m["gh"].Cookies != "sess=xyz" || len(m["gh"].Headers) != 0 {
		t.Errorf("got %#v", m)
	}
}

func TestProfile_LoadHostileJSON(t *testing.T) {
	dir := withProfileStore(t)
	if err := os.WriteFile(filepath.Join(dir, "profiles.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProfiles(); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %v, want parse error", err)
	}
}

func TestProfile_RmMissing(t *testing.T) {
	withProfileStore(t)
	err := newProfileRmCmd().RunE(newProfileRmCmd(), []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("err = %v, want unknown profile", err)
	}
}

func TestProfile_NamePattern(t *testing.T) {
	withProfileStore(t)
	for _, bad := range []string{"", "with space", strings.Repeat("x", 33)} {
		if err := newProfileSaveCmd().RunE(newProfileSaveCmd(), []string{bad}); err == nil {
			t.Errorf("name %q: want error", bad)
		}
	}
}

func TestResolveProfile_UnknownNamesKnown(t *testing.T) {
	withProfileStore(t)
	if err := saveProfiles(map[string]Profile{"known": {Cookies: "a=b"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveProfile("missing", "", nil)
	if err == nil || !strings.Contains(err.Error(), "known") {
		t.Errorf("err = %v, want unknown-name error listing known", err)
	}
}

func TestResolveProfile_FlagsWinFieldByField(t *testing.T) {
	withProfileStore(t)
	if err := saveProfiles(map[string]Profile{
		"p": {Cookies: "profile=cookie", Headers: []string{"X-From: profile", "X-Only: profile"}},
	}); err != nil {
		t.Fatal(err)
	}
	cookies, headers, err := resolveProfile("p", "flag=cookie", []string{"X-From: flag"})
	if err != nil {
		t.Fatal(err)
	}
	if cookies != "flag=cookie" {
		t.Errorf("cookies = %q, want flag to win", cookies)
	}
	if len(headers) != 1 || headers[0] != "X-From: flag" {
		t.Errorf("headers = %v, want flag set to win field-by-field", headers)
	}
	// No flags → profile fills everything.
	cookies, headers, err = resolveProfile("p", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookies != "profile=cookie" || len(headers) != 2 {
		t.Errorf("got %q %v, want profile values", cookies, headers)
	}
}

func TestProfile_MaskedLs(t *testing.T) {
	withProfileStore(t)
	if err := saveProfiles(map[string]Profile{
		"gh": {Cookies: "sess=abcdefghijklmn", Headers: []string{"Authorization: Bearer supersecrettoken"}},
	}); err != nil {
		t.Fatal(err)
	}
	cmd := newProfileLsCmd()
	cmd.SetOut(&strings.Builder{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := cmd.OutOrStdout().(*strings.Builder).String()
	for _, secret := range []string{"abcdefghijklmn", "supersecrettoken"} {
		if strings.Contains(out, secret) {
			t.Errorf("ls leaked secret %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "…klmn") || !strings.Contains(out, "Authorization: ") || !strings.Contains(out, "…oken") {
		t.Errorf("ls = %q, want masked previews", out)
	}
}
