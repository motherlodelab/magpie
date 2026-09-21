package vertical

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "event",
			Label:    "Event",
			Desc:     "Event record from schema.org Event JSON-LD (subtypes included). Explicit-only: matches any page.",
			Patterns: []string{"https://{events-site}/e/{event}"},
		},
		Match:   matchHTTP,
		Extract: extractEvent,
		OptIn:   true,
	})
}

func extractEvent(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	m, ok := firstTypedBlock(body, "Event")
	if !ok {
		return nil, fmt.Errorf("vertical: event: no Event data at %s", u.String())
	}
	rec := map[string]any{"url": u.String()}
	if v := str(m, "name"); v != "" {
		rec["name"] = v
	}
	if v := str(m, "startDate"); v != "" {
		rec["startDate"] = v
	}
	if v := str(m, "endDate"); v != "" {
		rec["endDate"] = v
	}
	if online, known := eventOnline(m); known {
		rec["isOnline"] = online
	}
	if loc := eventLocation(m); loc != nil {
		rec["location"] = loc
	}
	var offers []any
	for _, o := range blockArray(m["offers"]) {
		om := anyMap(o)
		if om == nil {
			continue
		}
		offer := map[string]any{}
		if v, present := om["price"]; present {
			offer["price"] = v
		}
		if v := str(om, "priceCurrency"); v != "" {
			offer["currency"] = v
		}
		if v := str(om, "availability"); v != "" {
			offer["availability"] = v
		}
		if v := str(om, "url"); v != "" {
			offer["url"] = v
		}
		if len(offer) > 0 {
			offers = append(offers, offer)
		}
	}
	if len(offers) > 0 {
		rec["offers"] = offers
	}
	if performers := nameList(m["performer"]); len(performers) > 0 {
		rec["performers"] = performers
	}
	if names := nameList(m["organizer"]); len(names) > 0 {
		rec["organizer"] = names[0]
	}
	return rec, nil
}

// eventOnline resolves the isOnline tri-state: attendance-mode enum first
// (Online/Mixed ⇒ true, Offline ⇒ false), VirtualLocation location as the
// fallback ⇒ true; nothing known ⇒ (false, false) and the key is omitted.
func eventOnline(m map[string]any) (online, known bool) {
	mode := str(m, "eventAttendanceMode")
	switch {
	case strings.Contains(mode, "Mixed"), strings.Contains(mode, "Online"):
		return true, true
	case strings.Contains(mode, "Offline"):
		return false, true
	}
	for _, l := range blockArray(m["location"]) {
		if lm := anyMap(l); lm != nil && typedAny(lm["@type"], []string{"VirtualLocation"}) {
			return true, true
		}
	}
	return false, false
}

// eventLocation flattens the first location (Place or VirtualLocation);
// nil when the event carries none.
func eventLocation(m map[string]any) map[string]any {
	for _, l := range blockArray(m["location"]) {
		lm := anyMap(l)
		if lm == nil {
			continue
		}
		loc := map[string]any{}
		if n := str(lm, "name"); n != "" {
			loc["name"] = n
		}
		if addr := child(lm, "address"); addr != nil {
			if fa := flatAddress(addr); len(fa) > 0 {
				loc["address"] = fa
			}
		}
		if len(loc) > 0 {
			return loc
		}
	}
	return nil
}
