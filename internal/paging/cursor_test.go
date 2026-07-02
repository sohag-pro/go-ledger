package paging

import (
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 2, 10, 30, 0, 123, time.UTC)
	tok := EncodeCursor(at, "abc-123")
	got, err := DecodeCursor(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil || !got.CreatedAt.Equal(at) || got.ID != "abc-123" {
		t.Fatalf("round trip mismatch: got %+v", got)
	}
}

func TestDecodeEmptyIsFirstPage(t *testing.T) {
	got, err := DecodeCursor("")
	if err != nil || got != nil {
		t.Fatalf("empty token: got %+v err %v, want nil,nil", got, err)
	}
}

func TestDecodeMalformed(t *testing.T) {
	for _, tok := range []string{"!!!not-base64!!!", "bm9waXBl"} { // 2nd decodes but has no separator
		if _, err := DecodeCursor(tok); err == nil {
			t.Errorf("token %q: expected error, got nil", tok)
		}
	}
}
