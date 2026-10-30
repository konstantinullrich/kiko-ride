package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// DefaultMapboxURL is the Mapbox API base URL.
const DefaultMapboxURL = "https://api.mapbox.com"

// Mapbox geocodes with the Mapbox Geocoding API v6 forward endpoint.
type Mapbox struct {
	// Token is a Mapbox access token.
	Token string
	// Permanent requests permanent geocoding. Mapbox only allows storing
	// results (as rides do) with permanent=true, which needs billing set up.
	Permanent bool
	// BaseURL defaults to DefaultMapboxURL.
	BaseURL string
	// Client defaults to http.DefaultClient.
	Client *http.Client
}

type mapboxResponse struct {
	Features []struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"` // [lng, lat]
		} `json:"geometry"`
	} `json:"features"`
}

func (m *Mapbox) Geocode(ctx context.Context, address string) (Point, error) {
	address = strings.TrimSpace(address)
	// Limits of the q parameter.
	if address == "" || strings.Contains(address, ";") || utf8.RuneCountInString(address) > 256 {
		return Point{}, fmt.Errorf("address must be 1-256 characters without ';'")
	}
	q := url.Values{"q": {address}, "limit": {"1"}, "access_token": {m.Token}}
	if m.Permanent {
		q.Set("permanent", "true")
	}
	base := m.BaseURL
	if base == "" {
		base = DefaultMapboxURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/search/geocode/v6/forward?"+q.Encode(), nil)
	if err != nil {
		return Point{}, err
	}
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error repeats the URL, which carries the access token.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return Point{}, fmt.Errorf("mapbox: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Point{}, fmt.Errorf("mapbox: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Point{}, fmt.Errorf("mapbox: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out mapboxResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Point{}, fmt.Errorf("mapbox: %w", err)
	}
	if len(out.Features) == 0 {
		return Point{}, ErrNotFound
	}
	c := out.Features[0].Geometry.Coordinates
	if len(c) < 2 {
		return Point{}, errors.New("mapbox: feature without coordinates")
	}
	p := Point{Lat: c[1], Lng: c[0]}
	if !p.Valid() {
		return Point{}, fmt.Errorf("mapbox: coordinates out of range: %v", c)
	}
	return p, nil
}
