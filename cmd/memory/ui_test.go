package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func uiAsset(t *testing.T, h http.Handler, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMemdUIServesPage(t *testing.T) {
	db, _ := openTestMemd(t)
	h := uiHandler(db)
	tests := []struct {
		path, ctype, contains string
	}{
		{"/", "text/html", "<title>memory"},
		{"/app.js", "text/javascript", "EventSource"},
		{"/app.css", "text/css", "prefers-color-scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := uiAsset(t, h, "127.0.0.1:37800", tt.path)
			if rec.Code != 200 {
				t.Fatalf("GET %s = %d", tt.path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tt.ctype) {
				t.Errorf("Content-Type = %q, want %s", ct, tt.ctype)
			}
			if !strings.Contains(rec.Body.String(), tt.contains) {
				t.Errorf("body of %s lacks %q", tt.path, tt.contains)
			}
		})
	}
	if rec := uiAsset(t, h, "127.0.0.1", "/missing.js"); rec.Code != 404 {
		t.Errorf("missing asset = %d, want 404", rec.Code)
	}
	if rec := uiAsset(t, h, "evil.example", "/"); rec.Code != 403 {
		t.Errorf("page with evil Host = %d, want 403", rec.Code)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
	// uiAssets alone serves the page too.
	if rec := uiAsset(t, uiAssets(), "x", "/"); rec.Code != 200 {
		t.Errorf("uiAssets GET / = %d", rec.Code)
	}
}

func uiEmbedded(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(uiFS, "ui", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(uiFS, p)
		out[strings.TrimPrefix(p, "ui/")] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"index.html", "app.js", "app.css"} {
		if out[want] == "" {
			t.Fatalf("embedded asset %s missing or empty", want)
		}
	}
	return out
}

func TestMemdUINoExternalResources(t *testing.T) {
	ext := regexp.MustCompile(`(?i)(src|href|action|poster|srcset)\s*=\s*["']?\s*(https?:)?//|url\(\s*["']?\s*(https?:)?//|@import|https?://|\bimport\s*\(`)
	for name, body := range uiEmbedded(t) {
		if m := ext.FindString(body); m != "" {
			t.Errorf("%s references an external resource: %q", name, m)
		}
	}
}

func TestMemdUIScriptsAreGETOnlyAndSafe(t *testing.T) {
	js := uiEmbedded(t)["app.js"]
	for _, m := range regexp.MustCompile(`fetch\([^)]*\)`).FindAllString(js, -1) {
		if regexp.MustCompile(`(?i)method`).MatchString(m) {
			t.Errorf("fetch call sets a method: %s", m)
		}
	}
	if !strings.Contains(js, "fetch(") {
		t.Error("app.js has no fetch call (test would pass vacuously)")
	}
	for _, bad := range []string{"POST", "PUT", "DELETE", "PATCH", "innerHTML", "outerHTML", "insertAdjacentHTML",
		"document.write", "eval(", "new Function", "XMLHttpRequest", "sendBeacon", "WebSocket"} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js contains %q", bad)
		}
	}
}

func TestMemdUIPageShape(t *testing.T) {
	a := uiEmbedded(t)
	html, js := a["index.html"], a["app.js"]
	// CSP forbids inline script, inline style and handlers.
	for _, m := range regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("index.html has an inline script: %q", m[0])
		}
	}
	if regexp.MustCompile(`(?i)<style|\sstyle\s*=|\son[a-z]+\s*=`).MatchString(html) {
		t.Error("index.html has inline style or event handler attributes")
	}
	if !strings.Contains(html, `name="viewport"`) {
		t.Error("index.html lacks a viewport meta")
	}
	for _, route := range []string{"overview", "memory", "sessions", "work", "observer", "privacy"} {
		if !strings.Contains(js, `"`+route+`"`) {
			t.Errorf("app.js lacks route %q", route)
		}
		if !strings.Contains(html, `href="#/`+route+`"`) {
			t.Errorf("index.html lacks nav link #/%s", route)
		}
	}
	for _, id := range []string{`id="health-pill"`, `id="banner"`, `id="theme-toggle"`, `role="status"`} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
	if strings.Contains(strings.ToLower(js+html), "my work") {
		t.Error("My Work view must stay out")
	}
	if total := len(html) + len(js) + len(a["app.css"]); total > 100*1024 {
		t.Errorf("assets total %d bytes, target < 100 KB", total)
	}
}
