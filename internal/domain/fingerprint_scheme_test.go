package domain

import (
	"testing"
	"time"
)

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

// TestCurrentFingerprintSchemeIsV2 pins the constant so a future scheme bump
// is a deliberate, reviewed edit to this test, not a silent drift. Bumped
// from "v1" to "v2" for Task 4.3 (audit A1.3): see fingerprintV2's doc
// comment in fingerprint.go for why.
func TestCurrentFingerprintSchemeIsV2(t *testing.T) {
	if CurrentFingerprintScheme != "v2" {
		t.Errorf("CurrentFingerprintScheme = %q, want %q", CurrentFingerprintScheme, "v2")
	}
}

// TestTransactionFingerprintV2MatchesFingerprintV2MethodDirectCall proves the
// "v2" scheme dispatches to the exact same byte-identical output as calling
// the unexported fingerprintV2 method directly, mirroring
// TestTransactionFingerprintV1MatchesDirectCall's shape for the new scheme.
func TestTransactionFingerprintV2MatchesFingerprintV2MethodDirectCall(t *testing.T) {
	ref := "INV-1"
	when := time.UnixMicro(1_700_000_000_000_000).UTC()
	txn := Transaction{
		Postings: []Posting{
			{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
			{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
		},
		Reference:   &ref,
		EffectiveAt: &when,
	}
	want := txn.fingerprintV2()

	got, ok := TransactionFingerprint("v2", txn)
	if !ok {
		t.Fatal(`TransactionFingerprint("v2", ...) ok = false, want true`)
	}
	if got != want {
		t.Errorf(`TransactionFingerprint("v2", ...) = %q, want %q (byte-identical to fingerprintV2())`, got, want)
	}
}

// TestTransactionFingerprintV2DiffersByReference proves the "v2" scheme
// (unlike "v1") moves when only Reference changes: a client reusing an
// Idempotency-Key with the same postings but a different Reference must not
// silently match under "v2". This is the exact gap Task 4.3's audit found in
// "v1": the fix is "v2" folding Reference into the hash (fingerprint.go's
// fingerprintV2 doc comment).
func TestTransactionFingerprintV2DiffersByReference(t *testing.T) {
	refA, refB := "INV-A", "INV-B"
	base := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}, Reference: &refA}
	other := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}, Reference: &refB}

	if mustV2(t, base) == mustV2(t, other) {
		t.Error("v2 fingerprint did not change when only Reference changed")
	}
	// And a nil Reference must also differ from a set one.
	noRef := Transaction{Postings: base.Postings}
	if mustV2(t, base) == mustV2(t, noRef) {
		t.Error("v2 fingerprint did not change between a set and a nil Reference")
	}
	// "v1", by contrast, is blind to Reference: this is the exact gap "v2"
	// closes, pinned here so a future edit cannot silently "fix" v1 instead
	// of adding v2 (which would break every already-stored v1 key).
	if base.Fingerprint() != other.Fingerprint() {
		t.Error("v1 fingerprint moved when only Reference changed, want it to stay blind to Reference")
	}
}

// TestTransactionFingerprintV2DiffersByEffectiveAt proves the "v2" scheme
// moves when only EffectiveAt changes, and that "v1" stays blind to it (the
// same shape as the Reference test above, for the other Task 4.3 field).
func TestTransactionFingerprintV2DiffersByEffectiveAt(t *testing.T) {
	t1 := time.UnixMicro(1_700_000_000_000_000).UTC()
	t2 := time.UnixMicro(1_800_000_000_000_000).UTC()
	base := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}, EffectiveAt: &t1}
	other := Transaction{Postings: base.Postings, EffectiveAt: &t2}
	noEffectiveAt := Transaction{Postings: base.Postings}

	if mustV2(t, base) == mustV2(t, other) {
		t.Error("v2 fingerprint did not change when only EffectiveAt changed")
	}
	if mustV2(t, base) == mustV2(t, noEffectiveAt) {
		t.Error("v2 fingerprint did not change between a set and a nil EffectiveAt")
	}
	if base.Fingerprint() != other.Fingerprint() {
		t.Error("v1 fingerprint moved when only EffectiveAt changed, want it to stay blind to EffectiveAt")
	}
}

// TestTransactionFingerprintV2StableAcrossEffectiveAtSubMicrosecondNoise
// proves the "v2" scheme's EffectiveAt encoding (UnixMicro) is stable across
// a nanosecond-level difference that a DB round trip would not preserve
// anyway (Postgres's timestamptz column is microsecond precision): two
// EffectiveAt values that differ only below one microsecond must fingerprint
// identically, or a value read back from storage would never match the
// fingerprint computed from what was originally sent.
func TestTransactionFingerprintV2StableAcrossEffectiveAtSubMicrosecondNoise(t *testing.T) {
	base := time.UnixMicro(1_700_000_000_000_000).UTC()
	withNanoNoise := base.Add(437 * time.Nanosecond)
	txnBase := Transaction{Postings: []Posting{
		{AccountID: "a", Amount: mustMoney(t, 100, "USD")},
		{AccountID: "b", Amount: mustMoney(t, -100, "USD")},
	}, EffectiveAt: &base}
	txnNoisy := Transaction{Postings: txnBase.Postings, EffectiveAt: &withNanoNoise}

	if mustV2(t, txnBase) != mustV2(t, txnNoisy) {
		t.Error("v2 fingerprint changed on sub-microsecond EffectiveAt noise, want it stable at microsecond precision")
	}
}

// mustV2 is a small helper returning the "v2" fingerprint for txn, failing
// the test if the scheme somehow is not registered (it always is; this is
// just to keep the call sites above terse).
func mustV2(t *testing.T, txn Transaction) string {
	t.Helper()
	fp, ok := TransactionFingerprint("v2", txn)
	if !ok {
		t.Fatal(`TransactionFingerprint("v2", ...) ok = false, want true`)
	}
	return fp
}
