package crawl

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"

	"github.com/motherlodelab/magpie/core"
)

// TestCSV_FormulaCellsDefused — QA C7: scraped text that a spreadsheet
// would run as a formula gets a ' prefix; numbers (and number-shaped
// strings) stay as they are.
func TestCSV_FormulaCellsDefused(t *testing.T) {
	out := filepath.Join(t.TempDir(), "r.csv")
	w, err := newWriter(out, "csv", mustTestSchema(t), nil, "r", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []map[string]any{
		{"title": `=HYPERLINK("http://x","y")`, "ean": "-5.00", "price": -3.5},
		{"title": `+cmd|' /C calc'!A0`, "ean": "@SUM(1)", "price": 1.0},
	} {
		if err := w.write(core.PageResult{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, c := range rows[0] {
		col[c] = i
	}
	for i, want := range []map[string]string{
		{"title": `'=HYPERLINK("http://x","y")`, "ean": "-5.00", "price": "-3.5"},
		{"title": `'+cmd|' /C calc'!A0`, "ean": "'@SUM(1)", "price": "1"},
	} {
		for c, v := range want {
			if got := rows[i+1][col[c]]; got != v {
				t.Errorf("row %d %s = %q, want %q", i+1, c, got, v)
			}
		}
	}
}
