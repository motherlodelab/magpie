package vertical_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestEventExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("event")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"events.example": {body: verticalFixture(t, "event.html")},
	}}
	const page = "https://events.example/e/meetup"
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]any{
		"name":      "Bangkok Scraper Meetup",
		"startDate": "2026-11-05T18:00+07:00",
		"endDate":   "2026-11-05T21:00+07:00",
		"isOnline":  true, // MixedEventAttendanceMode
		"location": map[string]any{
			"name": "The Hive",
			"address": map[string]any{
				"street": "Sukhumvit 49", "locality": "Bangkok", "region": "Bangkok", "country": "TH",
			},
		},
		"offers": []any{
			map[string]any{"price": float64(300), "currency": "THB", "availability": "https://schema.org/InStock", "url": "https://events.example/tickets/early"},
			map[string]any{"price": float64(500), "currency": "THB", "availability": "https://schema.org/InStock", "url": "https://events.example/tickets/door"},
		},
		"performers": []string{"Alice Doe", "Bob Rae"},
		"organizer":  "Bangkok Go Club",
		"url":        page, // decoy url inside the block must NOT win
	}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("got %#v\nwant %#v", rec, want)
	}
}

func TestEventExtract_AttendanceModes(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("event")
	cases := []struct {
		name   string
		json   string
		online any // expected isOnline value; nil = key must be absent
		check  func(t *testing.T, rec map[string]any)
	}{
		{"online enum", `{"@type":"Event","name":"X","eventAttendanceMode":"https://schema.org/OnlineEventAttendanceMode"}`, true, nil},
		{"offline enum", `{"@type":"Event","name":"X","eventAttendanceMode":"https://schema.org/OfflineEventAttendanceMode"}`, false, nil},
		{"virtual location", `{"@type":"Event","name":"X","location":{"@type":"VirtualLocation","name":"Live Stream","url":"https://stream.example/x"}}`, true,
			func(t *testing.T, rec map[string]any) {
				loc, ok := rec["location"].(map[string]any)
				if !ok || loc["name"] != "Live Stream" {
					t.Fatalf("location = %#v, want name only", rec["location"])
				}
				if _, ok := loc["address"]; ok {
					t.Error("VirtualLocation carried an address — must be name-only")
				}
			}},
		{"neither mode nor virtual", `{"@type":"Event","name":"X","location":{"@type":"Place","name":"Hall","address":{"addressLocality":"Oslo"}}}`, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := extractInline(t, ex, "events.example", tc.json)
			if tc.online == nil {
				if _, ok := rec["isOnline"]; ok {
					t.Errorf("isOnline = %v, want key absent (omission over guessing)", rec["isOnline"])
				}
			} else if rec["isOnline"] != tc.online {
				t.Errorf("isOnline = %v, want %v", rec["isOnline"], tc.online)
			}
			if tc.check != nil {
				tc.check(t, rec)
			}
		})
	}
}

func TestEventExtract_Variants(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("event")
	const offer = `"@type":"Offer","price":300,"priceCurrency":"THB","availability":"https://schema.org/InStock"`
	scalar := extractInline(t, ex, "events.example", `{"@type":"Event","name":"X","offers":{`+offer+`}}`)
	array := extractInline(t, ex, "events.example", `{"@type":"Event","name":"X","offers":[{`+offer+`}]}`)
	if !reflect.DeepEqual(scalar["offers"], array["offers"]) {
		t.Errorf("scalar offer %#v != array offer %#v", scalar["offers"], array["offers"])
	}
	stringPerformer := extractInline(t, ex, "events.example", `{"@type":"Event","name":"X","performer":"Solo Act"}`)
	objPerformer := extractInline(t, ex, "events.example", `{"@type":"Event","name":"X","performer":[{"@type":"Person","name":"Solo Act"}]}`)
	want := []string{"Solo Act"}
	if !reflect.DeepEqual(stringPerformer["performers"], want) || !reflect.DeepEqual(objPerformer["performers"], want) {
		t.Errorf("performers = %#v / %#v, want %v", stringPerformer["performers"], objPerformer["performers"], want)
	}
	org := extractInline(t, ex, "events.example", `{"@type":"Event","name":"X","organizer":{"@type":"Organization","name":"Org Co"}}`)
	if org["organizer"] != "Org Co" {
		t.Errorf("organizer = %#v, want string from {name} object", org["organizer"])
	}
}

func TestEventExtract_NoBlock(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("event")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"events.example": {body: []byte(`<html><body>nothing here</body></html>`)},
	}}
	const page = "https://events.example/none"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: event: no Event data at "+page) {
		t.Fatalf("err = %v, want typed no-data error naming the URL", err)
	}
}

// extractInline serves an inline JSON-LD literal through the fake fetcher.
func extractInline(t *testing.T, ex vertical.Extractor, host, jsonld string) map[string]any {
	t.Helper()
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		host: {body: []byte(`<html><head><script type="application/ld+json">` + jsonld + `</script></head></html>`)},
	}}
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://"+host+"/x"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return rec
}
