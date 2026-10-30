// Package geo turns addresses into coordinates and measures distances
// between them.
package geo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ErrNotFound is returned by a Geocoder when an address has no match.
var ErrNotFound = errors.New("address not found")

// Point is a WGS84 coordinate.
type Point struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Place is a geocoded address.
type Place struct {
	Address string `json:"address"`
	Point
}

// MaxSearchResults is the most places a Geocoder's Search returns.
const MaxSearchResults = 10

// Geocoder resolves free-form addresses to coordinates.
type Geocoder interface {
	// Geocode returns the best match for address, or ErrNotFound.
	Geocode(ctx context.Context, address string) (Point, error)
	// Search returns up to limit matches for query, best first; none is not
	// an error.
	Search(ctx context.Context, query string, limit int) ([]Place, error)
}

// ParsePoint parses "lat,lng".
func ParsePoint(s string) (Point, error) {
	latS, lngS, ok := strings.Cut(s, ",")
	if !ok {
		return Point{}, fmt.Errorf("%q is not lat,lng", s)
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(latS), 64)
	if err != nil {
		return Point{}, fmt.Errorf("%q is not lat,lng", s)
	}
	lng, err := strconv.ParseFloat(strings.TrimSpace(lngS), 64)
	if err != nil {
		return Point{}, fmt.Errorf("%q is not lat,lng", s)
	}
	p := Point{Lat: lat, Lng: lng}
	if !p.Valid() {
		return Point{}, fmt.Errorf("%q is out of range", s)
	}
	return p, nil
}

// Valid reports whether the point lies within latitude and longitude bounds.
func (p Point) Valid() bool {
	return p.Lat >= -90 && p.Lat <= 90 && p.Lng >= -180 && p.Lng <= 180
}

const earthRadiusKm = 6371.0088

// Distance returns the great-circle distance in kilometres.
func Distance(a, b Point) float64 {
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	dLat := rad(b.Lat - a.Lat)
	dLng := rad(b.Lng - a.Lng)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(a.Lat))*math.Cos(rad(b.Lat))*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusKm * math.Asin(math.Min(1, math.Sqrt(h)))
}
