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

func TestPageMoreThanLimitTrimsAndReportsHasMore(t *testing.T) {
	rows := []int{1, 2, 3}
	page, hasMore := Page(rows, 2)
	if !hasMore {
		t.Error("hasMore = false, want true")
	}
	if len(page) != 2 || page[0] != 1 || page[1] != 2 {
		t.Errorf("page = %v, want [1 2]", page)
	}
}

func TestPageExactlyLimitReportsNoMore(t *testing.T) {
	rows := []int{1, 2}
	page, hasMore := Page(rows, 2)
	if hasMore {
		t.Error("hasMore = true, want false")
	}
	if len(page) != 2 {
		t.Errorf("page = %v, want [1 2]", page)
	}
}

func TestPageFewerThanLimitReportsNoMore(t *testing.T) {
	rows := []int{1}
	page, hasMore := Page(rows, 2)
	if hasMore {
		t.Error("hasMore = true, want false")
	}
	if len(page) != 1 {
		t.Errorf("page = %v, want [1]", page)
	}
}

func TestPageZeroLimitIsPassthrough(t *testing.T) {
	rows := []int{1, 2, 3}
	page, hasMore := Page(rows, 0)
	if hasMore {
		t.Error("hasMore = true, want false for limit <= 0")
	}
	if len(page) != len(rows) {
		t.Errorf("page = %v, want %v unchanged", page, rows)
	}
}
