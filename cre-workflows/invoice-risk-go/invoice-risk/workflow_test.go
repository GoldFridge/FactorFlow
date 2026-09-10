package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// sealed encrypts a document the way the seller's browser does: AES-256-GCM, nonce first.
func sealed(t *testing.T, document any) (pendingWork, string) {
	t.Helper()

	plaintext, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encoding the document: %v", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("preparing the cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("preparing the cipher: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generating a nonce: %v", err)
	}

	ciphertext := append(append([]byte{}, nonce...), gcm.Seal(nil, nonce, plaintext, nil)...)
	digest := sha256.Sum256(ciphertext)

	return pendingWork{
		InvoiceID:  "44444444-4444-4444-8444-444444444444",
		Nonce:      "62d4160d62512739c480825115996cf4",
		CipherHash: hex.EncodeToString(digest[:]),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		DataKey:    base64.StdEncoding.EncodeToString(key),
	}, base64.StdEncoding.EncodeToString(key)
}

// document is a receivable as a seller's system writes one.
func document() map[string]any {
	return map[string]any{
		"number":     "INV-2026-0110",
		"debtor_ref": "Helios Manufacturing SpA",
		"currency":   "USD",
		"issued_at":  "2026-09-01T00:00:00Z",
		"due_at":     "2026-11-15T00:00:00Z",
		"total":      "18500.00",
		"line_items": []map[string]string{
			{"description": "Cold-rolled steel coil", "amount": "14200.00"},
			{"description": "Delivery", "amount": "3100.00"},
			{"description": "Certification", "amount": "1200.00"},
		},
		"terms": "Net 75 days. Seller retains recourse. No dispute has been raised.",
		"debtor_history": map[string]any{
			"invoices_paid":     41,
			"invoices_late":     6,
			"average_days_late": 9.5,
			"share_of_revenue":  0.28,
		},
	}
}

/*
TestTheEnclaveReturnsNumbersAndNothingElse is the privacy claim, checked rather than
asserted: everything that leaves this function is looked at, and none of it is the document.
*/
func TestTheEnclaveReturnsNumbersAndNothingElse(t *testing.T) {
	work, _ := sealed(t, document())

	result, err := assess(work, "features-v1")
	if err != nil {
		t.Fatalf("assessing: %v", err)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encoding the assessment: %v", err)
	}

	for _, secret := range []string{
		"Helios", "INV-2026-0110", "steel", "18500.00", "Net 75 days", "recourse has",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("the assessment carries %q out of the enclave", secret)
		}
	}
}

// TestFeaturesComeFromTheDocument checks each number against the fact it is derived from.
func TestFeaturesComeFromTheDocument(t *testing.T) {
	work, _ := sealed(t, document())

	result, err := assess(work, "features-v1")
	if err != nil {
		t.Fatalf("assessing: %v", err)
	}

	cases := []struct{ name, got, want string }{
		// 75 days between the two dates, against the 120-day long end of trade credit.
		{"dso_norm", result.Features.DSONorm, "0.625000"},
		// Six late of forty-seven.
		{"late_payment_rate", result.Features.LatePaymentRate, "0.127660"},
		// Nine and a half days late on average, against sixty.
		{"debtor_risk", result.Features.DebtorRisk, "0.158333"},
		{"debtor_concentration", result.Features.DebtorConcentration, "0.280000"},
		// The market is not the document's to report; the platform substitutes its own.
		{"market_volatility", result.Features.MarketVolatility, "0.000000"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}

	if !result.ArithmeticValid {
		t.Error("the line items add up to the total and the workflow says they do not")
	}
	if !result.Mitigations.Recourse {
		t.Error("the terms retain recourse and the workflow missed it")
	}
}

/*
TestADenialIsNotAnAssertion.

"No dispute has been raised" contains the word dispute and means the opposite of it. A
substring match read that as a disputed receivable, which is a whole grade of difference
produced by ignoring one word.
*/
func TestADenialIsNotAnAssertion(t *testing.T) {
	cases := []struct {
		terms string
		want  string
	}{
		{"Net 75 days. No dispute has been raised.", "0.000000"},
		{"Net 30 days. The buyer has not raised a dispute.", "0.000000"},
		{"Net 30 days. Delivered free of dispute.", "0.000000"},
		{"Net 60 days. A dispute is open over the second delivery.", "1.000000"},
		{"Net 60 days. This invoice is in dispute.", "1.000000"},
	}

	for _, tc := range cases {
		doc := document()
		doc["terms"] = tc.terms

		work, _ := sealed(t, doc)
		result, err := assess(work, "features-v1")
		if err != nil {
			t.Fatalf("assessing %q: %v", tc.terms, err)
		}
		if result.Features.DisputeFlag != tc.want {
			t.Errorf("%q gave dispute_flag %s, want %s",
				tc.terms, result.Features.DisputeFlag, tc.want)
		}
	}
}

/*
TestADocumentThatIsNotTheOneStoredIsRefused. The digest is checked before the key is used:
a document that is not what the platform stored must not be assessed under its identity, and
the cheapest moment to find that out is before decrypting.
*/
func TestADocumentThatIsNotTheOneStoredIsRefused(t *testing.T) {
	work, _ := sealed(t, document())
	work.CipherHash = strings.Repeat("a", 64)

	if _, err := assess(work, "features-v1"); err == nil {
		t.Fatal("a ciphertext that does not match its digest was assessed anyway")
	}
}

// TestTheWrongKeyOpensNothing: AES-GCM authenticates as well as encrypts, so a document
// that does not open is either altered or somebody else's, and neither is worth guessing at.
func TestTheWrongKeyOpensNothing(t *testing.T) {
	work, _ := sealed(t, document())
	other, _ := sealed(t, document())
	work.DataKey = other.DataKey

	if _, err := assess(work, "features-v1"); err == nil {
		t.Fatal("the document opened with a key that did not seal it")
	}
}

/*
TestBrokenArithmeticLowersConfidence. An invoice whose line items do not add up to its total
is not evidence of a debt, and the platform's gate refuses to auto-approve what this reports.
*/
func TestBrokenArithmeticLowersConfidence(t *testing.T) {
	doc := document()
	doc["total"] = "21000.00"

	work, _ := sealed(t, doc)
	result, err := assess(work, "features-v1")
	if err != nil {
		t.Fatalf("assessing: %v", err)
	}

	if result.ArithmeticValid {
		t.Error("the totals disagree and the workflow says they hold")
	}
	if result.Confidence >= "0.850000" {
		t.Errorf("confidence %s would pass a gate that exists for this case", result.Confidence)
	}
}

/*
TestTheCommitmentBindsTheDocument. Two runs over the same document commit to the same value —
which is what lets a verifier recompute it — and a different document commits to another.
*/
func TestTheCommitmentBindsTheDocument(t *testing.T) {
	work, _ := sealed(t, document())

	first, err := assess(work, "features-v1")
	if err != nil {
		t.Fatalf("assessing: %v", err)
	}
	second, err := assess(work, "features-v1")
	if err != nil {
		t.Fatalf("assessing again: %v", err)
	}
	if first.Commitment != second.Commitment {
		t.Error("the same document committed to two different values")
	}
	if !strings.HasPrefix(first.Commitment, "0x") || len(first.Commitment) != 66 {
		t.Errorf("the commitment %q is not a 0x-prefixed digest", first.Commitment)
	}

	other := document()
	other["debtor_history"] = map[string]any{
		"invoices_paid": 41, "invoices_late": 20,
		"average_days_late": 9.5, "share_of_revenue": 0.28,
	}
	changed, _ := sealed(t, other)
	third, err := assess(changed, "features-v1")
	if err != nil {
		t.Fatalf("assessing the other document: %v", err)
	}
	if third.Commitment == first.Commitment {
		t.Error("a different document committed to the same value")
	}
}
