package vertical

import (
	"context"
	"fmt"
	"net/url"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:     "local_business",
			Label:    "Local business",
			Desc:     "Business record from schema.org LocalBusiness JSON-LD (Restaurant/Store and other subtypes). Explicit-only: matches any page.",
			Patterns: []string{"https://{business-homepage}"},
		},
		Match:   matchHTTP,
		Extract: extractLocalBusiness,
		OptIn:   true,
	})
}

// businessTypes is the Contains-matched @type set. ponytail: schema.org
// has ~50 LocalBusiness subtypes; this covers the common spellings and
// anything containing "LocalBusiness" — widening is a data edit here.
var businessTypes = []string{"LocalBusiness", "Restaurant", "Store", "Hotel", "FoodEstablishment", "HealthAndBeautyBusiness", "HomeAndConstructionBusiness", "LegalService", "MedicalOrganization", "ProfessionalService"}

func extractLocalBusiness(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	body, err := fetchBytes(ctx, f, u.String())
	if err != nil {
		return nil, err
	}
	m, ok := firstTypedBlock(body, businessTypes...)
	if !ok {
		return nil, fmt.Errorf("vertical: local_business: no LocalBusiness data at %s", u.String())
	}
	rec := map[string]any{"url": u.String()}
	if v := str(m, "name"); v != "" {
		rec["name"] = v
	}
	if v := str(m, "telephone"); v != "" {
		rec["telephone"] = v
	}
	if v := str(m, "email"); v != "" {
		rec["email"] = v
	}
	if addr := child(m, "address"); addr != nil {
		if fa := flatAddress(addr); len(fa) > 0 {
			rec["address"] = fa
		}
	}
	if geo := child(m, "geo"); geo != nil {
		lat, lng := num(geo, "latitude"), num(geo, "longitude")
		if lat != 0 || lng != 0 {
			rec["geo"] = map[string]any{"lat": lat, "lng": lng}
		}
	}
	if hours := nameList(m["openingHours"]); len(hours) > 0 {
		rec["openingHours"] = hours
	}
	if v := str(m, "priceRange"); v != "" {
		rec["priceRange"] = v
	}
	if links := nameList(m["sameAs"]); len(links) > 0 {
		rec["sameAs"] = links
	}
	return rec, nil
}
