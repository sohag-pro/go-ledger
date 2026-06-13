package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newServer builds a mux with the web routes plus a healthz stub, matching how
// cmd/server wires things, so tests exercise real route precedence.
func newServer() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	Register(mux)
	return mux
}

func TestRoutes(t *testing.T) {
	srv := newServer()

	tests := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantBody    string // substring that must appear
		wantHeaders map[string]string
	}{
		{
			name:       "index served at root",
			method:     http.MethodGet,
			path:       "/",
			wantStatus: http.StatusOK,
			wantBody:   "production-grade payment ledger",
			wantHeaders: map[string]string{
				"Content-Type":           "text/html; charset=utf-8",
				"Cache-Control":          "public, max-age=300, must-revalidate",
				"X-Content-Type-Options": "nosniff",
			},
		},
		{
			name:       "index has local font, not google",
			method:     http.MethodGet,
			path:       "/",
			wantStatus: http.StatusOK,
			wantBody:   "/static/fonts/space-grotesk.woff2",
		},
		{
			name:       "healthz still works alongside web routes",
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusOK,
			wantBody:   `{"status":"ok"}`,
		},
		{
			name:       "unknown path is 404, not the index",
			method:     http.MethodGet,
			path:       "/does-not-exist",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "missing font is 404",
			method:     http.MethodGet,
			path:       "/static/fonts/nope.woff2",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body missing %q", tt.wantBody)
			}
			for k, v := range tt.wantHeaders {
				if got := rec.Header().Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

func TestIndexNoEmDashes(t *testing.T) {
	// CLAUDE.md bans em (U+2014) and en (U+2013) dashes repo-wide. The embedded
	// page is text we ship, so guard it here too. Code points are written as
	// numeric literals so this guard does not itself trip the repo dash scan.
	const enDash, emDash rune = 0x2013, 0x2014
	for _, r := range string(indexHTML) {
		if r == enDash || r == emDash {
			t.Fatalf("index.html contains a banned dash %U", r)
		}
	}
}

func TestIndexETagRevalidation(t *testing.T) {
	srv := newServer()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on index response")
	}

	// Same ETag -> 304 with empty body.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 response should have empty body, got %d bytes", rec2.Body.Len())
	}
}

func TestFontsServedWithImmutableCache(t *testing.T) {
	srv := newServer()

	// Discover the embedded fonts instead of hardcoding names.
	fonts, err := fs.Glob(staticFS, "static/fonts/*.woff2")
	if err != nil {
		t.Fatal(err)
	}
	if len(fonts) == 0 {
		t.Fatal("no embedded fonts found")
	}

	for _, f := range fonts {
		name := strings.TrimPrefix(f, "static/fonts/")
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/static/fonts/"+name, nil)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q", got)
			}
			if rec.Body.Len() == 0 {
				t.Error("empty font body")
			}
		})
	}
}
