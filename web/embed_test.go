package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	if _, err := fs.Stat(dist, "dist/index.html"); err != nil {
		t.Skip("site not built; run npm run build in web/")
	}
	assets, _ := fs.ReadDir(dist, "dist/assets")
	if len(assets) == 0 {
		t.Fatal("build has no assets")
	}
	h := Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	for _, page := range []string{"/", "/ride", "/ride?id=abc", "/anything/else"} {
		rec := get(page)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="app">`) {
			t.Errorf("%s: expected index.html, got %d", page, rec.Code)
		}
	}
	asset := get("/assets/" + assets[0].Name())
	if asset.Code != http.StatusOK || !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: got %d, cache %q", asset.Code, asset.Header().Get("Cache-Control"))
	}
	if rec := get("/kiko.png"); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("logo: got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get("/assets/missing.js"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset: got %d", rec.Code)
	}
	if rec := get("/assets/"); strings.Contains(rec.Body.String(), "<pre>") {
		t.Error("directory listing served")
	}
	if rec := get("/"); rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy")
	}
}
