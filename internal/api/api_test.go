package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMux wires the API and playground exactly as cmd/server does, so tests
// exercise real route registration and precedence.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	New(mux)
	RegisterPlayground(mux)
	return mux
}

func TestHealthOperation(t *testing.T) {
	mux := newMux()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Body must be exactly {"status":"ok"} with no extra keys (e.g. no $schema),
	// since the deploy health check and uptime monitors depend on this contract.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if len(body) != 1 {
		t.Errorf("body has extra keys: %v", body)
	}
}

func TestOpenAPIServed(t *testing.T) {
	mux := newMux()

	for _, path := range []string{"/openapi.json", "/openapi.yaml"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		got := rec.Body.String()
		for _, want := range []string{"/healthz", "go-ledger API", APIVersion} {
			if !strings.Contains(got, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}

func TestPlayground(t *testing.T) {
	mux := newMux()

	t.Run("page references spec and asset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playground", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{`data-url="/openapi.json"`, "/playground/scalar.js"} {
			if !strings.Contains(body, want) {
				t.Errorf("playground page missing %q", want)
			}
		}
	})

	t.Run("scalar asset served with immutable cache", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playground/scalar.js", nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control = %q", cc)
		}
		if rec.Body.Len() == 0 {
			t.Error("empty scalar asset")
		}
	})
}
