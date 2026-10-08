package crawl

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/motherlodelab/magpie/core"
	"github.com/motherlodelab/magpie/extract"
	"github.com/motherlodelab/magpie/store"
)

// writer serializes PageResults. jsonl streams; json buffers the full array
// (ponytail: a 100k-page --format json crawl holds all records in RAM —
// ceiling = large crawls must use jsonl).
type writer struct {
	format   string
	corpus   bool
	out      *os.File
	enc      *json.Encoder
	csvW     *csv.Writer
	cols     []string
	header   bool
	buf      []any
	db       *store.DB
	runID    string
	n        int
	closeOut bool
}

func newWriter(outPath, format string, sch *extract.Schema, db *store.DB, runID string, corpus bool) (*writer, error) {
	w := &writer{format: format, db: db, runID: runID, corpus: corpus}
	if outPath == "" {
		w.out = os.Stdout
	} else {
		f, err := os.Create(outPath)
		if err != nil {
			return nil, fmt.Errorf("crawl: create out: %w", err)
		}
		w.out = f
		w.closeOut = true
	}
	switch format {
	case "jsonl", "json":
		w.enc = json.NewEncoder(w.out)
	case "csv":
		w.cols = csvColumns(sch)
		w.csvW = csv.NewWriter(w.out)
	case "sqlite":
		if err := db.ExecRaw(`CREATE TABLE IF NOT EXISTS records(run_id TEXT NOT NULL, url TEXT NOT NULL, record_json TEXT NOT NULL, ts TEXT NOT NULL)`); err != nil {
			w.closeQuiet()
			return nil, err
		}
	default:
		w.closeQuiet()
		return nil, fmt.Errorf("crawl: format %q must be jsonl|json|csv|sqlite", format)
	}
	return w, nil
}

// csvCell defuses spreadsheet formulas in scraped text (QA C7): a leading
// = + - @ TAB CR gets a ' prefix, unless the cell is a plain number
// ("-5.00" stays a number).
func csvCell(s string) string {
	if s == "" || !strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	return "'" + s
}

// csvColumns = schema required[] then remaining sorted (deterministic).
func csvColumns(sch *extract.Schema) []string {
	obj, ok := sch.Raw.(map[string]any)
	if !ok {
		return nil
	}
	props, _ := obj["properties"].(map[string]any)
	var required []string
	if req, ok := obj["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
	}
	rest := []string{}
	inReq := map[string]bool{}
	for _, r := range required {
		inReq[r] = true
	}
	for name := range props {
		if !inReq[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(required, rest...)
}

// record is the single home for the JSON envelope: the writer encodes
// exactly this, and the sink hands the same object to OnRecord (exporter
// tee), so the two streams can never drift apart in shape. Corpus mode
// emits {url,title,depth,markdown}; extracted mode {url,extracted}.
func (w *writer) record(r core.PageResult) map[string]any {
	if w.corpus {
		return map[string]any{"url": r.Task.URL, "title": r.Title, "depth": r.Task.Depth, "markdown": r.Text}
	}
	return map[string]any{"url": r.Task.URL, "extracted": r.Record}
}

func (w *writer) write(r core.PageResult) error {
	url := r.Task.URL
	switch w.format {
	case "jsonl":
		if err := w.enc.Encode(w.record(r)); err != nil {
			return fmt.Errorf("crawl: jsonl encode: %w", err)
		}
		w.n++
		return nil
	case "json":
		w.buf = append(w.buf, w.record(r))
		w.n++
		return nil
	case "csv":
		if !w.header {
			if err := w.csvW.Write(w.cols); err != nil {
				return fmt.Errorf("crawl: csv header: %w", err)
			}
			w.header = true
		}
		row := make([]string, len(w.cols))
		for i, c := range w.cols {
			v, ok := r.Record[c]
			if !ok || v == nil {
				continue
			}
			switch t := v.(type) {
			case string:
				row[i] = csvCell(t)
			case float64, float32, int, int64, bool:
				row[i] = fmt.Sprintf("%v", t)
			default:
				// Covers json.Number and any decoded scalar the switch
				// missed without rejecting nested maps/slices below.
				if _, isMap := v.(map[string]any); isMap {
					return fmt.Errorf("crawl: csv: nested value in field %q (flat schemas only)", c)
				}
				if _, isSlice := v.([]any); isSlice {
					return fmt.Errorf("crawl: csv: nested value in field %q (flat schemas only)", c)
				}
				row[i] = strings.TrimSpace(fmt.Sprintf("%v", t))
			}
		}
		if err := w.csvW.Write(row); err != nil {
			return fmt.Errorf("crawl: csv write: %w", err)
		}
		w.n++
		return nil
	case "sqlite":
		raw, err := json.Marshal(r.Record)
		if err != nil {
			return fmt.Errorf("crawl: marshal record: %w", err)
		}
		if err := w.db.InsertRecord(w.runID, url, string(raw)); err != nil {
			return err
		}
		w.n++
		return nil
	}
	return fmt.Errorf("crawl: unknown format %q", w.format)
}

func (w *writer) close() error {
	var err error
	switch w.format {
	case "jsonl":
		// streamed; nothing to flush
	case "json":
		buf := w.buf
		if buf == nil {
			buf = []any{}
		}
		err = w.enc.Encode(buf)
	case "csv":
		w.csvW.Flush()
		err = w.csvW.Error()
	}
	if cerr := w.closeFile(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

func (w *writer) count() int {
	if w.format == "json" {
		return len(w.buf)
	}
	return w.n
}

func (w *writer) closeQuiet() {
	if w.closeOut && w.out != nil {
		_ = w.out.Close() //nolint:errcheck // error-path cleanup; close error unactionable
	}
}

func (w *writer) closeFile() error {
	if w.closeOut && w.out != nil {
		return w.out.Close()
	}
	return nil
}
