package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
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
