package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConsoleConfig(t *testing.T) {
	h := ConsoleConfig(ConsoleConfigData{DemoMode: true, DefaultTenantID: "00000000-0000-0000-0000-000000000001"})
	req := httptest.NewRequest(http.MethodGet, "/console/config", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	var got ConsoleConfigData
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.DemoMode || got.DefaultTenantID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("got %+v", got)
	}
}
