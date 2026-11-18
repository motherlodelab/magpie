// Package recdiff compares two record sets (crawls, scrapes, search hits)
// and reports rows added, changed and gone, with field-level before/after
// values. Records match by a key path; everything else is a field.
//
// Load holds every record's key plus its compared fields in memory, with
// fields over 1 KiB reduced to their sha256 — roughly 200 B × fields per
// record, so a 100k-record crawl is tens of MB.
// ponytail: O(records × fields) memory. Upgrade path: sort both files by
// key on disk and merge.
package recdiff

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxRows is the detailed-row cap when Options.MaxRows is unset.
	DefaultMaxRows = 200
	longField      = 1 << 10 // leaves over 1 KiB store their sha256, not their text
	maxValue       = 256     // FieldChange Old/New cap in bytes
)

// Options says how two record sets line up.
type Options struct {
	Key     string   // dotted path that identifies a record; "" = "url"
	Items   string   // dotted path to an array to explode: each element becomes a record, keyed by Key inside the element ("" = no explode)
	Ignore  []string // dotted paths never compared (e.g. "depth"); matches the path and everything under it
	Unseen  []string // keys this run didn't reach (failed pages): never "gone"
	NoGone  bool     // the run was partial (max pages hit): report no "gone" at all
	MaxRows int      // detailed rows kept; counts stay exact; 0 = DefaultMaxRows
}

// Result is the change report. Counts are always exact; Rows holds at most
// MaxRows entries, added first, then changed, then gone, sorted by key
// within each group.
type Result struct {
	Added     int
	Changed   int
	Gone      int
	Rows      []Row
	Truncated bool
	DupKeys   int // keys seen twice in one side: counted, never silently merged
	NoKey     int // records without Key: counted and skipped
}

// Row is one added, changed or gone record.
type Row struct {
	Key    string
	Change string // added | changed | gone
	Fields []FieldChange
}

// FieldChange is one field of a changed record. Old/New hold a string leaf
// as-is ("19.99", not "\"19.99\""), anything else as canonical JSON; each
// capped at 256 bytes. When either side is over 1 KiB the field was
// hash-compared and Old/New are empty.
type FieldChange struct {
	Path string
	Old  string
	New  string
	Long bool
}

// Set is a loaded record set. The zero value is empty and usable in Diff.
type Set struct {
	recs    map[string]map[string]leaf
	DupKeys int
	NoKey   int
}

// leaf is one comparable field value. kind keeps strings, numbers and
// booleans apart ("19.99" the string is not 19.99 the number); text is the
// canonical comparable form, or the sha256 of it when long.
type leaf struct {
	kind byte // s string, n number, t bool, z null, a array (canonical JSON)
	text string
	long bool
}

// Load streams JSONL (or a JSON array) into a comparable set: it keeps the
// key plus each compared field, with fields over 1 KiB reduced to their
// sha256. Records without the key path are counted in NoKey and skipped;
// duplicate keys are counted in DupKeys and the last occurrence wins.
func Load(r io.Reader, o Options) (Set, error) {
	set := Set{recs: map[string]map[string]leaf{}}
	keyPath := o.Key
	if keyPath == "" {
		keyPath = "url"
	}
	dec := json.NewDecoder(bufio.NewReader(r))
	dec.UseNumber()
	addRecord := func(v any) {
		obj, ok := v.(map[string]any)
		if !ok {
			set.NoKey++
			return
		}
		if o.Items != "" {
			arr, ok := lookup(obj, o.Items).([]any)
			if !ok {
				set.NoKey++
				return
			}
			for _, el := range arr {
				elem, ok := el.(map[string]any)
				if !ok {
					set.NoKey++
					continue
				}
				set.add(elem, keyPath, o)
			}
			return
		}
		set.add(obj, keyPath, o)
	}
	for {
		var v any
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			return Set{}, fmt.Errorf("recdiff: load: %w", err)
		}
		if arr, ok := v.([]any); ok {
			for _, el := range arr {
				addRecord(el)
			}
			continue
		}
		addRecord(v)
	}
	return set, nil
}

func (s *Set) add(obj map[string]any, keyPath string, o Options) {
	key := ""
	switch kv := lookup(obj, keyPath).(type) {
	case string:
		key = kv
	case json.Number:
		key = kv.String()
	}
	if strings.TrimSpace(key) == "" {
		s.NoKey++
		return
	}
	fields := map[string]leaf{}
	flatten(obj, "", keyPath, o.Ignore, fields)
	if _, dup := s.recs[key]; dup {
		s.DupKeys++
	}
	s.recs[key] = fields
}

// flatten turns an object into dotted-path leaves. Nested objects recurse;
// arrays compare whole; the key path itself is identity, not a field.
func flatten(obj map[string]any, prefix, keyPath string, ignore []string, out map[string]leaf) {
	for k, v := range obj {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if p == keyPath || isIgnored(p, ignore) {
			continue
		}
		if t, ok := v.(map[string]any); ok {
			flatten(t, p, keyPath, ignore, out)
			continue
		}
		out[p] = toLeaf(v)
	}
}

func isIgnored(path string, ignore []string) bool {
	for _, ig := range ignore {
		if path == ig || strings.HasPrefix(path, ig+".") {
			return true
		}
	}
	return false
}

func lookup(obj map[string]any, path string) any {
	var cur any = obj
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

func toLeaf(v any) leaf {
	switch t := v.(type) {
	case string:
		return mkLeaf('s', collapse(t))
	case json.Number:
		return mkLeaf('n', canonNum(t.String()))
	case bool:
		return mkLeaf('t', strconv.FormatBool(t))
	case nil:
		return mkLeaf('z', "null")
	default: // arrays; nested maps inside them are canonicalized in place
		b, err := json.Marshal(canonAny(v))
		if err != nil {
			return mkLeaf('s', fmt.Sprintf("%v", v))
		}
		return mkLeaf('a', string(b))
	}
}

func mkLeaf(kind byte, text string) leaf {
	if len(text) > longField {
		sum := sha256.Sum256([]byte(text))
		return leaf{kind: kind, text: hex.EncodeToString(sum[:]), long: true}
	}
	return leaf{kind: kind, text: text}
}

// canonAny canonicalizes a value for whole-comparison (arrays): strings are
// whitespace-collapsed, numbers normalized, map keys sorted by Marshal.
func canonAny(v any) any {
	switch t := v.(type) {
	case string:
		return collapse(t)
	case json.Number:
		return json.Number(canonNum(t.String()))
	case bool, nil:
		return t
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = canonAny(el)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, el := range t {
			out[k] = canonAny(el)
		}
		return out
	default:
		return t
	}
}

// collapse trims and collapses whitespace runs, which cuts layout noise.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// canonNum makes 19.9 and 19.90 equal. Whole numbers (ids, timestamps)
// compare literally so values beyond float64's exact range can't collide;
// floats and exponents go through float64 round-trip.
func canonNum(s string) string {
	if !strings.ContainsAny(s, ".eE") {
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Diff compares two loaded sets and returns the change report. Counts are
// exact regardless of MaxRows.
func Diff(prev, cur Set, o Options) Result {
	res := Result{DupKeys: prev.DupKeys + cur.DupKeys, NoKey: prev.NoKey + cur.NoKey}
	max := o.MaxRows
	if max <= 0 {
		max = DefaultMaxRows
	}
	unseen := make(map[string]bool, len(o.Unseen))
	for _, k := range o.Unseen {
		unseen[k] = true
	}

	var addedRows, changedRows, goneRows []Row
	for key := range cur.recs {
		prevFields, ok := prev.recs[key]
		if !ok {
			res.Added++
			addedRows = append(addedRows, Row{Key: key, Change: "added"})
			continue
		}
		if fields := diffFields(prevFields, cur.recs[key], o.Ignore); len(fields) > 0 {
			res.Changed++
			changedRows = append(changedRows, Row{Key: key, Change: "changed", Fields: fields})
		}
	}
	if !o.NoGone {
		for key := range prev.recs {
			if _, ok := cur.recs[key]; ok || unseen[key] {
				continue
			}
			res.Gone++
			goneRows = append(goneRows, Row{Key: key, Change: "gone"})
		}
	}

	sortRows(addedRows)
	sortRows(changedRows)
	sortRows(goneRows)
	rows := make([]Row, 0, len(addedRows)+len(changedRows)+len(goneRows))
	rows = append(rows, addedRows...)
	rows = append(rows, changedRows...)
	rows = append(rows, goneRows...)
	if len(rows) > max {
		res.Truncated = true
		rows = rows[:max]
	}
	res.Rows = rows
	return res
}

func sortRows(rows []Row) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
}

// diffFields compares two records field by field; missing paths are
// differences too. Paths sorted so rows are stable. Ignore paths are also
// filtered here, so the invariant holds even for sets loaded without them.
func diffFields(prev, cur map[string]leaf, ignore []string) []FieldChange {
	seen := make(map[string]bool, len(prev))
	paths := make([]string, 0, len(prev)+len(cur))
	for p := range prev {
		paths = append(paths, p)
		seen[p] = true
	}
	for p := range cur {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var out []FieldChange
	for _, p := range paths {
		if isIgnored(p, ignore) {
			continue
		}
		pv, okPrev := prev[p]
		cv, okCur := cur[p]
		if okPrev == okCur && pv == cv {
			continue
		}
		fc := FieldChange{Path: p}
		if okPrev && !pv.long {
			fc.Old = cap256(pv.text)
		}
		if okCur && !cv.long {
			fc.New = cap256(cv.text)
		}
		if (okPrev && pv.long) || (okCur && cv.long) {
			fc.Long = true
			fc.Old, fc.New = "", ""
		}
		out = append(out, fc)
	}
	return out
}

// cap256 truncates to maxValue bytes without splitting a UTF-8 rune.
func cap256(s string) string {
	if len(s) <= maxValue {
		return s
	}
	cut := s[:maxValue]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
