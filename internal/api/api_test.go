package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// newRouter wires the API and playground onto a chi router the way cmd/server
// does, so tests exercise real route registration and precedence. Services are
// zero because these tests hit only meta routes (health, spec, playground).
func newRouter() chi.Router {
	r := chi.NewRouter()
	RegisterPlayground(r)
	New(r, Deps{})
	return r
}

func TestHealthOperation(t *testing.T) {
	router := newRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Body must be exactly {"status":"ok","revision":...}, no other keys
	// (e.g. no $schema): the deploy health check and uptime monitors depend
	// on "status" staying exactly "ok", and revision (Task 5.6a) is the one
	// deliberate addition to that contract.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	if _, ok := body["revision"]; !ok {
		t.Errorf("body missing revision field: %v", body)
	}
	if len(body) != 2 {
		t.Errorf("body has unexpected keys: %v", body)
	}
}

// TestHealthOperationRevision proves Deps.Revision is threaded through to
// the served response, not just present as an empty string.
func TestHealthOperationRevision(t *testing.T) {
	router := chi.NewRouter()
	New(router, Deps{Revision: "abc1234"})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if body["revision"] != "abc1234" {
		t.Errorf("revision = %v, want abc1234", body["revision"])
	}
}

func TestOpenAPIServed(t *testing.T) {
	router := newRouter()

	for _, path := range []string{"/openapi.json", "/openapi.yaml"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, rec.Code)
		}
		got := rec.Body.String()
		for _, want := range []string{"/healthz", "/v1/accounts", "go-ledger API", APIVersion} {
			if !strings.Contains(got, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}

func TestPlayground(t *testing.T) {
	router := newRouter()

	t.Run("page references spec and asset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/playground", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

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
		router.ServeHTTP(rec, req)

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

// TestCSVSafeField checks the CSV/formula-injection guard used by the export
// endpoints (audit A: CSV formula injection): a free-text cell that begins with
// a formula trigger is prefixed with a single quote so a spreadsheet treats it
// as text, while ordinary text (and, importantly, values that are not the first
// character) is left untouched.
func TestCSVSafeField(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty untouched", in: "", want: ""},
		{name: "plain text untouched", in: "salary payment", want: "salary payment"},
		{name: "equals neutralized", in: `=HYPERLINK("http://evil","x")`, want: `'=HYPERLINK("http://evil","x")`},
		{name: "plus neutralized", in: "+1+1", want: "'+1+1"},
		{name: "minus neutralized", in: "-cmd", want: "'-cmd"},
		{name: "at neutralized", in: "@SUM(A1)", want: "'@SUM(A1)"},
		{name: "tab neutralized", in: "\t=1", want: "'\t=1"},
		{name: "trigger not first char untouched", in: "a=b", want: "a=b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := csvSafeField(tt.in); got != tt.want {
				t.Errorf("csvSafeField(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
