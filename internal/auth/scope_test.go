package auth

import (
	"errors"
	"net/http"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

func TestRequiredHTTPScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
		want   domain.Scope
	}{
		{"GET is read", http.MethodGet, "/v1/accounts", domain.ScopeRead},
		{"HEAD is read", http.MethodHead, "/v1/accounts", domain.ScopeRead},
		{"OPTIONS is read", http.MethodOptions, "/v1/accounts", domain.ScopeRead},
		{"POST is post", http.MethodPost, "/v1/transactions", domain.ScopePost},
		{"PUT is post", http.MethodPut, "/v1/accounts/1", domain.ScopePost},
		{"PATCH is post", http.MethodPatch, "/v1/accounts/1", domain.ScopePost},
		{"DELETE is post", http.MethodDelete, "/v1/accounts/1", domain.ScopePost},
		{"admin path with GET is admin", http.MethodGet, "/v1/admin/keys", domain.ScopeAdmin},
		{"admin path with POST is admin", http.MethodPost, "/v1/admin/keys", domain.ScopeAdmin},
		{"admin path with DELETE is admin", http.MethodDelete, "/v1/admin/keys/1", domain.ScopeAdmin},
		{"unrecognized method fails closed to post", "TRACE", "/v1/accounts", domain.ScopePost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RequiredHTTPScope(tt.method, tt.path); got != tt.want {
				t.Errorf("RequiredHTTPScope(%q, %q) = %q, want %q", tt.method, tt.path, got, tt.want)
			}
		})
	}
}

func TestCheckScope(t *testing.T) {
	t.Parallel()

	t.Run("key with required scope passes", func(t *testing.T) {
		t.Parallel()
		key := domain.APIKey{Scopes: []domain.Scope{domain.ScopeRead}}
		if err := CheckScope(key, domain.ScopeRead); err != nil {
			t.Errorf("CheckScope = %v, want nil", err)
		}
	})

	t.Run("key missing required scope fails", func(t *testing.T) {
		t.Parallel()
		key := domain.APIKey{Scopes: []domain.Scope{domain.ScopeRead}}
		err := CheckScope(key, domain.ScopePost)
		if err == nil {
			t.Fatal("CheckScope = nil, want an InsufficientScopeError")
		}
		if !errors.Is(err, domain.ErrInsufficientScope) {
			t.Errorf("CheckScope error = %v, want it to match domain.ErrInsufficientScope", err)
		}
		var scopeErr *domain.InsufficientScopeError
		if !errors.As(err, &scopeErr) || scopeErr.Required != domain.ScopePost {
			t.Errorf("CheckScope error = %v, want *InsufficientScopeError{Required: post}", err)
		}
	})

	t.Run("admin key satisfies any required scope", func(t *testing.T) {
		t.Parallel()
		key := domain.APIKey{Scopes: []domain.Scope{domain.ScopeAdmin}}
		for _, required := range []domain.Scope{domain.ScopeRead, domain.ScopePost, domain.ScopeAdmin} {
			if err := CheckScope(key, required); err != nil {
				t.Errorf("CheckScope(admin key, %q) = %v, want nil", required, err)
			}
		}
	})
}
