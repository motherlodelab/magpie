package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/recdiff"
)

// The plan's fixture pair: +1 · ~1 · -1, no changes prints nothing (exit 0).
func diffFixtures(t *testing.T) (prevPath, curPath string) {
	t.Helper()
	dir := t.TempDir()
	prev := filepath.Join(dir, "prev.jsonl")
	cur := filepath.Join(dir, "cur.jsonl")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(prev, `{"sku":"sku-1","price":"19.99","name":"Widget"}
{"sku":"sku-2","price":"19.99","name":"Gadget"}
{"sku":"sku-3","price":"9.99","name":"Mousetrap"}
`)
	write(cur, `{"sku":"sku-1","price":"19.99","name":"Widget"}
{"sku":"sku-2","price":"17.99","name":"Gadget"}
{"sku":"sku-4","price":"5.99","name":"Sprocket"}
`)
	return prev, cur
}

func TestDiffRecords_NoChangeSilent(t *testing.T) {
	prev, _ := diffFixtures(t)
	var out, errW bytes.Buffer
	err := runRecordDiff(prev, prev, recdiff.Options{Key: "sku"}, "text", &out, &errW)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	if out.Len() != 0 || errW.Len() != 0 {
		t.Fatalf("no changes must print nothing: out=%q err=%q", out.String(), errW.String())
	}
}

func TestDiffRecords_JSONShape(t *testing.T) {
	prev, cur := diffFixtures(t)
	var out, errW bytes.Buffer
	err := runRecordDiff(prev, cur, recdiff.Options{Key: "sku"}, "json", &out, &errW)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	var res recdiff.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("json: %v (%s)", err, out.String())
	}
	if res.Added != 1 || res.Changed != 1 || res.Gone != 1 {
		t.Fatalf("counts: +(%d)~(%d)-(%d)", res.Added, res.Changed, res.Gone)
	}
	var changed *recdiff.Row
	for i := range res.Rows {
		if res.Rows[i].Key == "sku-2" {
			changed = &res.Rows[i]
		}
	}
	if changed == nil || changed.Change != "changed" || len(changed.Fields) != 1 ||
		changed.Fields[0].Path != "price" || changed.Fields[0].Old != "19.99" ||
		changed.Fields[0].New != "17.99" {
		b, _ := json.Marshal(res.Rows)
		t.Fatalf("changed row: %s", b)
	}
}

func TestDiffRecords_Text(t *testing.T) {
	prev, cur := diffFixtures(t)
	var out, errW bytes.Buffer
	err := runRecordDiff(prev, cur, recdiff.Options{Key: "sku"}, "text", &out, &errW)
	if err != nil {
		t.Fatalf("exit: %v", err)
	}
	for _, want := range []string{"+ sku-4\n", "- sku-3\n", "~ sku-2 price: 19.99 → 17.99\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
}

func TestDiffRecords_BadInput(t *testing.T) {
	var out, errW bytes.Buffer
	if err := runRecordDiff("-", "x.jsonl", recdiff.Options{}, "text", &out, &errW); err == nil {
		t.Fatal("stdin must be refused in records mode")
	}
	if err := runRecordDiff(filepath.Join(t.TempDir(), "gone.jsonl"), "x.jsonl", recdiff.Options{}, "text", &out, &errW); err == nil {
		t.Fatal("missing prev file must error")
	}
	if err := runRecordDiff("/dev/null", "/dev/null", recdiff.Options{}, "yaml", &out, &errW); err == nil {
		t.Fatal("bad format must error")
	}
}
