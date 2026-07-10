package domain

import "testing"

// TestTransactionFingerprintV1MatchesDirectCall proves the "v1" scheme
// dispatches to the exact same byte-identical output as calling
// Transaction.Fingerprint() directly, so existing stored keys (and existing
// fingerprint_test.go vectors) still match after the scheme dispatch lands
// (Task 2.3, audit A1.6).
func TestTransactionFingerprintV1MatchesDirectCall(t *testing.T) {
	txn := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	want := txn.Fingerprint()

	got, ok := TransactionFingerprint("v1", txn)
	if !ok {
		t.Fatal("TransactionFingerprint(\"v1\", ...) ok = false, want true")
	}
	if got != want {
		t.Errorf("TransactionFingerprint(\"v1\", ...) = %q, want %q (byte-identical to Fingerprint())", got, want)
	}
}

// TestTransactionFingerprintUnknownScheme proves an unrecognized scheme
// fails closed: ok is false and the fingerprint is empty, never a value a
// caller might mistake for a real match.
func TestTransactionFingerprintUnknownScheme(t *testing.T) {
	txn := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}}
	fp, ok := TransactionFingerprint("v99", txn)
	if ok {
		t.Error("TransactionFingerprint with unknown scheme: ok = true, want false")
	}
	if fp != "" {
		t.Errorf("TransactionFingerprint with unknown scheme: fp = %q, want empty", fp)
	}
}

// TestConvertFingerprintV1MatchesDirectCall proves the "v1" scheme dispatches
// to the exact same byte-identical output as calling
// ConvertRequestFingerprint directly.
func TestConvertFingerprintV1MatchesDirectCall(t *testing.T) {
	want := ConvertRequestFingerprint("acct-from", "acct-to", 500)

	got, ok := ConvertFingerprint("v1", "acct-from", "acct-to", 500)
	if !ok {
		t.Fatal("ConvertFingerprint(\"v1\", ...) ok = false, want true")
	}
	if got != want {
		t.Errorf("ConvertFingerprint(\"v1\", ...) = %q, want %q (byte-identical to ConvertRequestFingerprint())", got, want)
	}
}

// TestConvertFingerprintUnknownScheme proves an unrecognized scheme fails
// closed for the convert path too.
func TestConvertFingerprintUnknownScheme(t *testing.T) {
	fp, ok := ConvertFingerprint("v99", "acct-from", "acct-to", 500)
	if ok {
		t.Error("ConvertFingerprint with unknown scheme: ok = true, want false")
	}
	if fp != "" {
		t.Errorf("ConvertFingerprint with unknown scheme: fp = %q, want empty", fp)
	}
}

// TestCurrentFingerprintSchemeIsV1 pins the constant so a future scheme bump
// is a deliberate, reviewed edit to this test, not a silent drift.
func TestCurrentFingerprintSchemeIsV1(t *testing.T) {
	if CurrentFingerprintScheme != "v1" {
		t.Errorf("CurrentFingerprintScheme = %q, want %q", CurrentFingerprintScheme, "v1")
	}
}
