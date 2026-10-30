package geo

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDistance(t *testing.T) {
	berlin := Point{Lat: 52.5200, Lng: 13.4050}
	munich := Point{Lat: 48.1351, Lng: 11.5820}
	if d := Distance(berlin, munich); math.Abs(d-504) > 3 {
		t.Fatalf("Berlin-Munich: got %.1f km", d)
	}
	if d := Distance(berlin, berlin); d != 0 {
		t.Fatalf("same point: got %f", d)
	}
}

func TestParsePoint(t *testing.T) {
	p, err := ParsePoint("52.52, 13.405")
	if err != nil || p != (Point{Lat: 52.52, Lng: 13.405}) {
		t.Fatalf("got %+v, %v", p, err)
	}
	for _, bad := range []string{"", "52.52", "x,y", "91,0", "0,181"} {
		if _, err := ParsePoint(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestMapbox(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/search/geocode/v6/forward" || q.Get("access_token") != "tok" || q.Get("limit") != "1" || q.Get("permanent") != "true" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if q.Get("q") == "nowhere" {
			w.Write([]byte(`{"type":"FeatureCollection","features":[]}`))
			return
		}
		w.Write([]byte(`{"type":"FeatureCollection","features":[{"geometry":{"type":"Point","coordinates":[13.4132,52.5219]}}]}`))
	}))
	defer srv.Close()

	m := &Mapbox{Token: "tok", Permanent: true, BaseURL: srv.URL}
	p, err := m.Geocode(context.Background(), "Alexanderplatz, Berlin")
	if err != nil || p != (Point{Lat: 52.5219, Lng: 13.4132}) {
		t.Fatalf("got %+v, %v", p, err)
	}
	if _, err := m.Geocode(context.Background(), "nowhere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := m.Geocode(context.Background(), "a;b"); err == nil {
		t.Fatal("expected error for ';'")
	}
}

func TestMapboxErrorHidesToken(t *testing.T) {
	m := &Mapbox{Token: "secret-token", BaseURL: "http://127.0.0.1:1"}
	_, err := m.Geocode(context.Background(), "Berlin")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected an error without the token, got %v", err)
	}
}
