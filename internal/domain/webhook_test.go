package domain_test

import (
	"strings"
	"testing"

	"github.com/sohag-pro/go-ledger/internal/domain"
)

func TestGenerateWebhookSecret(t *testing.T) {
	t.Parallel()
	secret, err := domain.GenerateWebhookSecret()
	if err != nil {
		t.Fatalf("GenerateWebhookSecret: %v", err)
	}
	if !strings.HasPrefix(secret, "whsec_") {
		t.Errorf("secret = %q, want a whsec_ prefix", secret)
	}
	if len(secret) < 20 {
		t.Errorf("secret = %q, suspiciously short", secret)
	}

	second, err := domain.GenerateWebhookSecret()
	if err != nil {
		t.Fatalf("GenerateWebhookSecret (second): %v", err)
	}
	if second == secret {
		t.Error("two generated secrets must differ")
	}
}

func TestWebhookSubscriptionValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		url     string
		wantErr error
	}{
		{"empty url", "", domain.ErrInvalidWebhookURL},
		{"whitespace only", "   ", domain.ErrInvalidWebhookURL},
		{"not a url", "not a url", domain.ErrInvalidWebhookURL},
		{"missing scheme", "example.com/hooks", domain.ErrInvalidWebhookURL},
		{"wrong scheme", "ftp://example.com/hooks", domain.ErrInvalidWebhookURL},
		{"no host", "https:///hooks", domain.ErrInvalidWebhookURL},
		{"valid https", "https://example.com/hooks", nil},
		{"valid http", "http://localhost:8080/hooks", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sub := domain.WebhookSubscription{URL: tc.url}
			err := sub.Validate()
			if tc.wantErr == nil && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.url, err)
			}
			if tc.wantErr != nil && err != tc.wantErr {
				t.Errorf("Validate(%q) = %v, want %v", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestWebhookSubscriptionMatches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		eventTypes []string
		action     string
		want       bool
	}{
		{"empty filter matches everything", nil, "transaction.created", true},
		{"empty filter matches an unrelated action too", nil, "anything.at.all", true},
		{"exact match", []string{"transaction.created"}, "transaction.created", true},
		{"one of several", []string{"transaction.reversed", "transaction.created"}, "transaction.created", true},
		{"no match", []string{"transaction.reversed"}, "transaction.created", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sub := domain.WebhookSubscription{EventTypes: tc.eventTypes}
			if got := sub.Matches(tc.action); got != tc.want {
				t.Errorf("Matches(%q) with EventTypes=%v = %v, want %v", tc.action, tc.eventTypes, got, tc.want)
			}
		})
	}
}
