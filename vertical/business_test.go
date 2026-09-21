package vertical_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestLocalBusinessExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("local_business")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"bistro.example": {body: verticalFixture(t, "local-business.html")},
	}}
	const page = "https://bistro.example/"
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]any{
		"name":      "The Example Bistro",
		"telephone": "+66-2-555-0100",
		"email":     "hello@bistro.example",
		"url":       page,
		"address": map[string]any{
			"street": "12 Sukhumvit Rd", "locality": "Bangkok", "region": "Bangkok",
			"postalCode": "10110", "country": "TH",
		},
		"geo":          map[string]any{"lat": 13.7307, "lng": 100.5589},
		"openingHours": []string{"Mo-Fr 08:00-18:00", "Sa-Su 09:00-22:00"},
		"priceRange":   "$$",
		"sameAs":       []string{"https://facebook.com/examplebistro", "https://instagram.com/examplebistro"},
	}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("got %#v\nwant %#v", rec, want)
	}
}

func TestLocalBusiness_Subtypes(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("local_business")
	for _, typ := range []string{"LocalBusiness", "Store"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			rec := extractInline(t, ex, "biz.example", `{"@type":"`+typ+`","name":"Sub Co","telephone":"+1-555-0100"}`)
			if rec["name"] != "Sub Co" || rec["telephone"] != "+1-555-0100" {
				t.Errorf("@type %s: got %#v", typ, rec)
			}
		})
	}
}

func TestLocalBusiness_Variants(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("local_business")
	t.Run("openingHours scalar", func(t *testing.T) {
		t.Parallel()
		rec := extractInline(t, ex, "biz.example", `{"@type":"Store","name":"X","openingHours":"Mo-Fr 09:00-17:00"}`)
		hours, ok := rec["openingHours"].([]string)
		if !ok || !reflect.DeepEqual(hours, []string{"Mo-Fr 09:00-17:00"}) {
			t.Errorf("openingHours = %#v, want one-element array", rec["openingHours"])
		}
	})
	t.Run("addressRegion omitted", func(t *testing.T) {
		t.Parallel()
		rec := extractInline(t, ex, "biz.example", `{"@type":"Store","name":"X","address":{"addressLocality":"Oslo","addressCountry":"NO"}}`)
		addr, ok := rec["address"].(map[string]any)
		if !ok {
			t.Fatalf("address = %#v", rec["address"])
		}
		if _, ok := addr["region"]; ok {
			t.Errorf("region = %v, want key absent (source omits it)", addr["region"])
		}
		if !reflect.DeepEqual(addr, map[string]any{"locality": "Oslo", "country": "NO"}) {
			t.Errorf("address = %#v", addr)
		}
	})
	t.Run("no geo", func(t *testing.T) {
		t.Parallel()
		rec := extractInline(t, ex, "biz.example", `{"@type":"Store","name":"X"}`)
		if _, ok := rec["geo"]; ok {
			t.Error("geo present with no source geo — omission broken")
		}
	})
}

func TestLocalBusinessMatch_OptInHTTPOnly(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("local_business")
	if !ok {
		t.Fatal("local_business not registered")
	}
	if !ex.Match(mustURL(t, "https://bistro.example/")) || ex.Match(mustURL(t, "file:///x")) {
		t.Error("local_business.Match: want http yes, file no")
	}
	if _, err := ex.Extract(t.Context(), &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"biz.example": {body: []byte(`<html><body>no data</body></html>`)},
	}}, mustURL(t, "https://biz.example/")); err == nil || !strings.Contains(err.Error(), "vertical: local_business: no LocalBusiness data at") {
		t.Fatalf("err = %v, want typed no-data error", err)
	}
}
