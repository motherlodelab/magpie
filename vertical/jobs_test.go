package vertical_test

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/vertical"
)

func TestJobPostingMatch_Table(t *testing.T) {
	t.Parallel()
	ex, ok := vertical.Lookup("job_posting")
	if !ok {
		t.Fatal("job_posting not registered")
	}
	yes := []string{"https://jobs.acme.example/boards/greenhouse/123", "http://careers.example/x"}
	no := []string{"file:///tmp/x.html", "ftp://example.com/x"}
	for _, raw := range yes {
		if !ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = false, want true", raw)
		}
	}
	for _, raw := range no {
		if ex.Match(mustURL(t, raw)) {
			t.Errorf("Match(%s) = true, want false", raw)
		}
	}
}

func TestJobPostingExtract(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("job_posting")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"jobs.acme.example": {body: verticalFixture(t, "job-posting.html")},
	}}
	const page = "https://jobs.acme.example/boards/greenhouse/123"
	rec, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if rec["title"] != "Senior Go Engineer" {
		t.Errorf("title = %v", rec["title"])
	}
	if rec["organization"] != "Acme Robotics" {
		t.Errorf("organization = %v", rec["organization"])
	}
	if rec["remote"] != true {
		t.Errorf("remote = %v, want true (TELECOMMUTE)", rec["remote"])
	}
	locs, ok := rec["locations"].([]string)
	if !ok || !reflect.DeepEqual(locs, []string{"Austin, TX, US", "Rotterdam, NL"}) {
		t.Errorf("locations = %#v, want two joined addresses in order", rec["locations"])
	}
	if rec["datePosted"] != "2026-09-01" || rec["validThrough"] != "2026-12-01" || rec["employmentType"] != "FULL_TIME" {
		t.Errorf("dates/type = %v/%v/%v", rec["datePosted"], rec["validThrough"], rec["employmentType"])
	}
	if rec["description"] != "Build scrapers." {
		t.Errorf("description = %q, want tag-stripped text", rec["description"])
	}
	if rec["url"] != page { // decoy url inside the JSON-LD block must NOT win
		t.Errorf("url = %v, want fetched page URL", rec["url"])
	}
	salary, ok := rec["salary"].(map[string]any)
	if !ok {
		t.Fatalf("salary = %#v, want map", rec["salary"])
	}
	for k, want := range map[string]float64{"min": 120000, "max": 160000} {
		got, _ := salary[k].(float64)
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("salary.%s = %v, want %v", k, got, want)
		}
	}
	if salary["unit"] != "YEAR" || salary["currency"] != "USD" {
		t.Errorf("salary unit/currency = %v/%v", salary["unit"], salary["currency"])
	}
}

func TestJobPostingExtract_Variants(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("job_posting")
	cases := []struct {
		name  string
		json  string
		check func(t *testing.T, rec map[string]any)
	}{
		{"number salary", `{"@type":"JobPosting","title":"X","baseSalary":85000}`,
			func(t *testing.T, rec map[string]any) {
				s, ok := rec["salary"].(map[string]any)
				if !ok {
					t.Fatalf("salary = %#v", rec["salary"])
				}
				if v, _ := s["value"].(float64); math.Abs(v-85000) > 1e-9 {
					t.Errorf("value = %v", v)
				}
				if _, ok := s["currency"]; ok {
					t.Error("currency present on number salary — omission broken")
				}
			}},
		{"scalar location + per-place telecommute", `{"@type":"JobPosting","title":"X","jobLocation":{"@type":"Place","jobLocationType":"TELECOMMUTE","address":{"addressLocality":"Remote"}}}`,
			func(t *testing.T, rec map[string]any) {
				if rec["remote"] != true {
					t.Errorf("remote = %v, want true", rec["remote"])
				}
				locs, ok := rec["locations"].([]string)
				if !ok || !reflect.DeepEqual(locs, []string{"Remote"}) {
					t.Errorf("locations = %#v, want one scalar location", rec["locations"])
				}
			}},
		{"no salary, no validThrough", `{"@type":"JobPosting","title":"X"}`,
			func(t *testing.T, rec map[string]any) {
				for _, k := range []string{"salary", "validThrough", "locations", "remote"} {
					if _, ok := rec[k]; ok {
						t.Errorf("%s present — omission over guessing broken", k)
					}
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
				"jobs.acme.example": {body: []byte(`<html><head><script type="application/ld+json">` + tc.json + `</script></head></html>`)},
			}}
			rec, err := ex.Extract(t.Context(), fx, mustURL(t, "https://jobs.acme.example/1"))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			tc.check(t, rec)
		})
	}
}

func TestJobPostingExtract_NoBlock(t *testing.T) {
	t.Parallel()
	ex, _ := vertical.Lookup("job_posting")
	fx := &fakeVerticalFetcher{bodies: map[string]fakeResp{
		"jobs.acme.example": {body: []byte(`<html><body>no structured data</body></html>`)},
	}}
	const page = "https://jobs.acme.example/none"
	_, err := ex.Extract(t.Context(), fx, mustURL(t, page))
	if err == nil || !strings.Contains(err.Error(), "vertical: job_posting: no JobPosting data at "+page) {
		t.Fatalf("err = %v, want typed no-data error naming the URL", err)
	}
}
