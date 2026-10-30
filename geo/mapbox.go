package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
		Properties struct {
			Name        string `json:"name"`
			FullAddress string `json:"full_address"`
		} `json:"properties"`
	} `json:"features"`
}

func (m *Mapbox) Geocode(ctx context.Context, address string) (Point, error) {
	places, err := m.Search(ctx, address, 1)
	if err != nil {
		return Point{}, err
	}
	if len(places) == 0 {
		return Point{}, ErrNotFound
	}
	return places[0].Point, nil
}

func (m *Mapbox) Search(ctx context.Context, query string, limit int) ([]Place, error) {
	query = strings.TrimSpace(query)
	// Limits of the q and limit parameters.
	if query == "" || strings.Contains(query, ";") || utf8.RuneCountInString(query) > 256 {
		return nil, fmt.Errorf("address must be 1-256 characters without ';'")
	}
	if limit < 1 || limit > MaxSearchResults {
		return nil, fmt.Errorf("limit must be 1-%d", MaxSearchResults)
	}
	q := url.Values{"q": {query}, "limit": {strconv.Itoa(limit)}, "access_token": {m.Token}}
	if m.Permanent {
		q.Set("permanent", "true")
	}
	base := m.BaseURL
	if base == "" {
		base = DefaultMapboxURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/search/geocode/v6/forward?"+q.Encode(), nil)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("mapbox: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("mapbox: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mapbox: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out mapboxResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("mapbox: %w", err)
	}
	places := make([]Place, 0, len(out.Features))
	for _, f := range out.Features {
		c := f.Geometry.Coordinates
		if len(c) < 2 {
			return nil, errors.New("mapbox: feature without coordinates")
		}
		p := Point{Lat: c[1], Lng: c[0]}
		if !p.Valid() {
			return nil, fmt.Errorf("mapbox: coordinates out of range: %v", c)
		}
		addr := f.Properties.FullAddress
		if addr == "" {
			addr = f.Properties.Name
		}
		places = append(places, Place{Address: addr, Point: p})
	}
	return places, nil
}
