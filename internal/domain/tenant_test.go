package domain

import (
	"errors"
	"testing"
)

func TestTenantStatus_Valid(t *testing.T) {
	tests := []struct {
		status TenantStatus
		want   bool
	}{
		{TenantActive, true},
		{TenantSuspended, true},
		{TenantClosed, true},
		{"", false},
		{"ACTIVE", false},
		{"pending", false},
		{"active ", false},
	}
	for _, tt := range tests {
		if got := tt.status.Valid(); got != tt.want {
			t.Errorf("TenantStatus(%q).Valid() = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestTenant_Validate(t *testing.T) {
	tests := []struct {
		name    string
		tenant  Tenant
		wantErr bool
	}{
		{"valid active tenant", Tenant{Name: "Acme Corp", Status: TenantActive}, false},
		{"valid suspended tenant", Tenant{Name: "Acme Corp", Status: TenantSuspended}, false},
		{"valid closed tenant", Tenant{Name: "Acme Corp", Status: TenantClosed}, false},
		{"empty name", Tenant{Name: "", Status: TenantActive}, true},
		{"invalid status", Tenant{Name: "Acme Corp", Status: "pending"}, true},
		{"empty status", Tenant{Name: "Acme Corp", Status: ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tenant.Validate()
			if tt.wantErr && !errors.Is(err, ErrInvalidTenant) {
				t.Errorf("Validate() = %v, want ErrInvalidTenant", err)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestTenantNotActiveError(t *testing.T) {
	err := &TenantNotActiveError{TenantID: "tenant-1", Status: TenantSuspended}

	if !errors.Is(err, ErrTenantNotActive) {
		t.Error("TenantNotActiveError does not match ErrTenantNotActive via errors.Is")
	}
	wantReason := "tenant is suspended"
	if got := err.Reason(); got != wantReason {
		t.Errorf("Reason() = %q, want %q", got, wantReason)
	}
	if err.Error() == "" {
		t.Error("Error() returned an empty string")
	}

	closedErr := &TenantNotActiveError{TenantID: "tenant-2", Status: TenantClosed}
	if got := closedErr.Reason(); got != "tenant is closed" {
		t.Errorf("Reason() for closed = %q, want %q", got, "tenant is closed")
	}
}
