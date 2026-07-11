package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHashAPIKeyIsSHA256Hex(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("glk_example"))
	want := hex.EncodeToString(sum[:])
	if got := HashAPIKey("glk_example"); got != want {
		t.Errorf("HashAPIKey(%q) = %q, want %q", "glk_example", got, want)
	}
}

func TestGenerateAPIKey(t *testing.T) {
	t.Parallel()
	plaintext, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if !strings.HasPrefix(plaintext, "glk_") {
		t.Errorf("plaintext = %q, want glk_ prefix", plaintext)
	}
	if got := HashAPIKey(plaintext); got != hash {
		t.Errorf("HashAPIKey(plaintext) = %q, want returned hash %q", got, hash)
	}
}

func TestGenerateAPIKeyDiffersAcrossCalls(t *testing.T) {
	t.Parallel()
	plaintext1, hash1, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	plaintext2, hash2, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if plaintext1 == plaintext2 {
		t.Error("two GenerateAPIKey calls returned the same plaintext")
	}
	if hash1 == hash2 {
		t.Error("two GenerateAPIKey calls returned the same hash")
	}
}

// --- Task 2.2: scopes, expiry, InsufficientScopeError. ---

func TestScopeValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		scope Scope
		want  bool
	}{
		{ScopeRead, true},
		{ScopePost, true},
		{ScopeAdmin, true},
		{Scope(""), false},
		{Scope("write"), false},
		{Scope("READ"), false},
	}
	for _, tt := range tests {
		if got := tt.scope.Valid(); got != tt.want {
			t.Errorf("Scope(%q).Valid() = %v, want %v", tt.scope, got, tt.want)
		}
	}
}

func TestAPIKeyHasScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		scopes   []Scope
		required Scope
		want     bool
	}{
		{"read key satisfies read", []Scope{ScopeRead}, ScopeRead, true},
		{"read key does not satisfy post", []Scope{ScopeRead}, ScopePost, false},
		{"read+post key satisfies post", []Scope{ScopeRead, ScopePost}, ScopePost, true},
		{"read+post key does not satisfy admin", []Scope{ScopeRead, ScopePost}, ScopeAdmin, false},
		{"admin key satisfies read (superset)", []Scope{ScopeAdmin}, ScopeRead, true},
		{"admin key satisfies post (superset)", []Scope{ScopeAdmin}, ScopePost, true},
		{"admin key satisfies admin", []Scope{ScopeAdmin}, ScopeAdmin, true},
		{"no scopes satisfies nothing", nil, ScopeRead, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			k := APIKey{Scopes: tt.scopes}
			if got := k.HasScope(tt.required); got != tt.want {
				t.Errorf("HasScope(%q) with scopes %v = %v, want %v", tt.required, tt.scopes, got, tt.want)
			}
		})
	}
}

func TestAPIKeyIsExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name      string
		expiresAt *time.Time
		want      bool
	}{
		{"nil never expires", nil, false},
		{"future expiry is not expired", &future, false},
		{"past expiry is expired", &past, true},
		{"expiry exactly now is expired", &now, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			k := APIKey{ExpiresAt: tt.expiresAt}
			if got := k.IsExpired(now); got != tt.want {
				t.Errorf("IsExpired(now) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInsufficientScopeError(t *testing.T) {
	t.Parallel()
	err := &InsufficientScopeError{Required: ScopePost}

	if !errors.Is(err, ErrInsufficientScope) {
		t.Error("InsufficientScopeError does not match ErrInsufficientScope via errors.Is")
	}
	wantReason := "missing required scope: post"
	if got := err.Reason(); got != wantReason {
		t.Errorf("Reason() = %q, want %q", got, wantReason)
	}
	if err.Error() == "" {
		t.Error("Error() should not be empty")
	}
}
