package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/smartcontractkit/cre-sdk-go/capabilities/networking/http"
	"github.com/smartcontractkit/cre-sdk-go/capabilities/scheduler/cron"
	"github.com/smartcontractkit/cre-sdk-go/cre"
)

/*
The confidential assessment of a receivable.

This is the one place in FactorFlow where an invoice is readable. The document is encrypted
in the seller's browser and stays that way everywhere else: the platform stores bytes it
cannot open, and prices the receivable from what this workflow reports about them. Here,
inside an attested enclave, the key is released, the document is decrypted, its own
arithmetic is checked, and a handful of normalized numbers come back out.

What leaves the enclave is deliberately thin — six features in [0,1], a confidence, two
verdicts and a commitment. No text, no line items, no debtor, no amounts. That is not a
courtesy to the seller: a feature vector cannot be turned back into an invoice, so the
platform never holds anything worth stealing, and neither does a node operator.

The key is never the platform's either. It reaches this enclave from the Vault DON and
nowhere else, which is what makes "the platform cannot read the document" a fact about the
system rather than a promise about its behaviour.

The scoring is elsewhere on purpose. This workflow does not decide what a receivable is
worth; it reports what the document says about it, and a published deterministic model
turns that into a price that anybody can recompute.
*/

// Config is what a deployment supplies.
type Config struct {
	// Schedule is the cron expression the enclave wakes on.
	Schedule string `json:"schedule"`
	// PlatformURL is the FactorFlow API this workflow takes work from and returns it to.
	PlatformURL string `json:"platformUrl"`
	// SecretID names the Vault secret holding the platform token. The token authorizes
	// this workflow to collect pending work; it is released only into the enclave.
	SecretID string `json:"secretId"`
	// KeySecretID names the Vault secret holding the document key. The platform never has
	// it: it goes from the browser that made it to the enclave that reads with it.
	KeySecretID string `json:"keySecretId"`
	// SchemaVersion is the feature schema the platform expects back. A workflow answering
	// in an older schema is refused rather than scored.
	SchemaVersion string `json:"schemaVersion"`
}

// ModelVersion identifies the extraction this workflow performs, so a stored assessment
// says which code produced its features.
const ModelVersion = "cre-invoice-risk-v1"

/*
pendingWork is one assessment the platform is waiting on.

The ciphertext travels as base64 inside the response rather than as a URL to fetch
separately: one confidential request in and one report out is easier to reason about than a
signed URL whose lifetime has to be argued about, and the enclave is the only place the
bytes are ever readable anyway.
*/
type pendingWork struct {
	InvoiceID  string `json:"invoice_id"`
	Nonce      string `json:"nonce"`
	CipherHash string `json:"cipher_hash"`
	Ciphertext string `json:"ciphertext"`
	// DataKey is the document's own AES key, base64. It is filled in from the Vault secret
	// inside the enclave and is deliberately not part of what the platform sends.
	DataKey string `json:"-"`
}

// invoiceDocument is the part of a decrypted document this workflow reads.
//
// It is the demo's synthetic format. A production version parses a PDF here; nothing else
// about the design changes, because everything outside this function already treats the
// document as unreadable.
type invoiceDocument struct {
	Number    string `json:"number"`
	DebtorRef string `json:"debtor_ref"`
	Currency  string `json:"currency"`
	IssuedAt  string `json:"issued_at"`
	DueAt     string `json:"due_at"`

	// Total is what the document claims is owed, and LineItems are what it is made of.
	// They are checked against each other: a document whose own arithmetic does not hold
	// is not evidence of anything.
	Total     string `json:"total"`
	LineItems []struct {
		Description string `json:"description"`
		Amount      string `json:"amount"`
	} `json:"line_items"`

	// Terms is the payment clause, read for the words that change the risk rather than
	// interpreted: a dispute clause and a recourse clause are the two that matter.
	Terms string `json:"terms"`

	// DebtorHistory is what the seller states about this debtor. It is a claim rather than
	// a fact, and the confidence reported below reflects that.
	DebtorHistory struct {
		InvoicesPaid    int     `json:"invoices_paid"`
		InvoicesLate    int     `json:"invoices_late"`
		AverageDaysLate float64 `json:"average_days_late"`
		ShareOfRevenue  float64 `json:"share_of_revenue"`
	} `json:"debtor_history"`
}

// assessment is the whole of what leaves the enclave.
type assessment struct {
	InvoiceID string `json:"invoice_id"`

	// Features are normalized to [0,1] here, so nothing outside has to know what a raw
	// days-sales-outstanding figure looks like.
	Features struct {
		DSONorm             string `json:"dso_norm"`
		LatePaymentRate     string `json:"late_payment_rate"`
		DisputeFlag         string `json:"dispute_flag"`
		DebtorConcentration string `json:"debtor_concentration"`
		MarketVolatility    string `json:"market_volatility"`
		DebtorRisk          string `json:"debtor_risk"`
	} `json:"features"`

	Confidence      string `json:"confidence"`
	ArithmeticValid bool   `json:"arithmetic_valid"`

	Mitigations struct {
		Recourse       bool `json:"recourse"`
		Collateralized bool `json:"collateralized"`
	} `json:"mitigations"`

	// Commitment binds this result to the document it was computed from, the nonce of the
	// run and the version of this code. A verifier with the ciphertext digest can recompute
	// it; nobody can invert it into a document.
	Commitment    string `json:"commitment"`
	SchemaVersion string `json:"schema_version"`
	ModelVersion  string `json:"model_version"`
}

/*
onSchedule is the enclave handler.

Everything here runs inside the attested enclave: the secrets, the document and every
intermediate value. The only thing that leaves is the assessment, which was built to be
safe to publish.
*/
func onSchedule(config *Config, runtime cre.TeeRuntime, _ *cron.Payload) (string, error) {
	secret, err := runtime.GetSecret(&cre.SecretRequest{Id: config.SecretID}).Await()
	if err != nil {
		return "", fmt.Errorf("the platform token was not released into this enclave: %w", err)
	}

	// The document's key comes from the Vault DON, not from the platform. That is the whole
	// privacy argument in one line: the platform holds ciphertext and a digest, and there is
	// no request it could make that would hand it the means to read what it stores.
	//
	// A production deployment releases a key per document, named by the reference stored
	// beside it. The demo uses one key for the seeded document, which changes where the
	// secret comes from and nothing about who can see it.
	dataKey, err := runtime.GetSecret(&cre.SecretRequest{Id: config.KeySecretID}).Await()
	if err != nil {
		return "", fmt.Errorf("the document key was not released into this enclave: %w", err)
	}

	client := &http.Client{}
	response, err := client.SendRequestInTee(runtime, &http.Request{
		Url:    strings.TrimRight(config.PlatformURL, "/") + "/api/v1/confidential/work",
		Method: "GET",
		MultiHeaders: map[string]*http.HeaderValues{
			"Authorization": {Values: []string{"Bearer " + secret.Value}},
			"Accept":        {Values: []string{"application/json"}},
		},
	}).Await()
	if err != nil {
		return "", fmt.Errorf("collecting work from the platform: %w", err)
	}
	if response.StatusCode == 204 {
		return "nothing to assess", nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("the platform answered %d when asked for work", response.StatusCode)
	}

	var work pendingWork
	if err := json.Unmarshal(response.Body, &work); err != nil {
		return "", fmt.Errorf("the platform's answer was not work this workflow understands: %w", err)
	}

	work.DataKey = dataKey.Value

	result, err := assess(work, config.SchemaVersion)
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encoding the assessment: %w", err)
	}

	// The assessment is delivered from inside the enclave as well.
	//
	// It could be handed across to the Workflow DON instead — the result is minimal by
	// construction, so nothing in it needs hiding. The reason to cross back is consensus:
	// a deployment that wanted the DON's signature over these numbers would call
	// GenerateReport there and deliver the signed report. That is the natural next step and
	// it needs deploy access, which this workflow does not yet have; until then, delivering
	// from the enclave keeps the platform token's use in one place.
	delivered, err := client.SendRequestInTee(runtime, &http.Request{
		Url:    strings.TrimRight(config.PlatformURL, "/") + "/api/v1/confidential/results",
		Method: "POST",
		Body:   body,
		MultiHeaders: map[string]*http.HeaderValues{
			"Content-Type":  {Values: []string{"application/json"}},
			"Authorization": {Values: []string{"Bearer " + secret.Value}},
		},
	}).Await()
	if err != nil {
		return "", fmt.Errorf("returning the assessment: %w", err)
	}
	if delivered.StatusCode < 200 || delivered.StatusCode > 299 {
		return "", fmt.Errorf("the platform answered %d when given the assessment", delivered.StatusCode)
	}

	// The invoice id and the commitment are safe to say out loud: one is public, the other
	// is a digest. Nothing about the document is.
	return fmt.Sprintf("assessed %s (commitment %s)", result.InvoiceID, result.Commitment), nil
}

/*
assess opens the document and reduces it to numbers.

This is the only function in the system that sees an invoice, and it is written to give
back as little as it can while still being useful: six features, a confidence and two
verdicts. Everything it read is gone when it returns.
*/
func assess(work pendingWork, schemaVersion string) (assessment, error) {
	var out assessment

	ciphertext, err := base64.StdEncoding.DecodeString(work.Ciphertext)
	if err != nil {
		return out, fmt.Errorf("the ciphertext was not base64: %w", err)
	}

	// The digest is checked before the key is used. A document that is not the one the
	// platform stored must not be assessed under its identity, and the cheapest moment to
	// find that out is before decrypting.
	digest := sha256.Sum256(ciphertext)
	if hex.EncodeToString(digest[:]) != strings.ToLower(strings.TrimSpace(work.CipherHash)) {
		return out, fmt.Errorf("the ciphertext does not match the digest the platform stored")
	}

	plaintext, err := open(ciphertext, work.DataKey)
	if err != nil {
		return out, err
	}

	var document invoiceDocument
	if err := json.Unmarshal(plaintext, &document); err != nil {
		// The parse failure is reported without the document: an error message that quotes
		// what it could not parse is a leak with a stack trace attached.
		return out, fmt.Errorf("the decrypted document is not in a format this workflow reads")
	}

	arithmetic := totalsAgree(document)
	features := derive(document)

	out.InvoiceID = work.InvoiceID
	out.Features.DSONorm = rate(features.dsoNorm)
	out.Features.LatePaymentRate = rate(features.latePaymentRate)
	out.Features.DisputeFlag = rate(features.disputeFlag)
	out.Features.DebtorConcentration = rate(features.debtorConcentration)
	// Volatility is an observation of the market rather than of the document, so this
	// workflow reports zero and the platform substitutes its own snapshot.
	out.Features.MarketVolatility = rate(0)
	out.Features.DebtorRisk = rate(features.debtorRisk)

	out.Confidence = rate(confidence(document, arithmetic))
	out.ArithmeticValid = arithmetic
	out.Mitigations.Recourse = mentions(document.Terms, "recourse")
	out.Mitigations.Collateralized = mentions(document.Terms, "collateral")
	out.SchemaVersion = schemaVersion
	out.ModelVersion = ModelVersion
	out.Commitment = commit(work, out)

	return out, nil
}

// open decrypts what the browser sealed: a 12-byte nonce followed by AES-GCM ciphertext.
func open(ciphertext []byte, keyBase64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyBase64))
	if err != nil {
		return nil, fmt.Errorf("the data key was not base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("the data key is not an AES key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("preparing the cipher: %w", err)
	}
	if len(ciphertext) <= gcm.NonceSize() {
		return nil, fmt.Errorf("the ciphertext is too short to contain a nonce")
	}

	plaintext, err := gcm.Open(nil, ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():], nil)
	if err != nil {
		// AES-GCM authenticates as well as encrypts, so this is either the wrong key or a
		// document somebody altered. Neither is worth guessing between.
		return nil, fmt.Errorf("the document did not open with the key that was released")
	}
	return plaintext, nil
}

// features are the normalized signals the published model consumes.
type features struct {
	dsoNorm             float64
	latePaymentRate     float64
	disputeFlag         float64
	debtorConcentration float64
	debtorRisk          float64
}

/*
derive turns what the document says into numbers in [0,1].

Each one is a stated fact reduced to a scale, not an opinion. The normalizations are here
rather than in the platform because the raw figures never leave: a tenor in days and a count
of late payments are facts about a debtor, and the whole point of this workflow is that they
stay inside it.
*/
func derive(document invoiceDocument) features {
	var out features

	// Tenor, normalized against 120 days — the long end of ordinary trade credit. Longer
	// paper is riskier because more can go wrong before it is paid.
	if issued, due, ok := dates(document); ok {
		out.dsoNorm = clamp(due.Sub(issued).Hours() / 24 / 120)
	}

	history := document.DebtorHistory
	if total := history.InvoicesPaid + history.InvoicesLate; total > 0 {
		out.latePaymentRate = clamp(float64(history.InvoicesLate) / float64(total))
	}
	if claims(document.Terms, "dispute") {
		out.disputeFlag = 1
	}
	out.debtorConcentration = clamp(history.ShareOfRevenue)

	// How badly this debtor pays when it pays late, normalized against 60 days overdue.
	// A debtor that is occasionally a week late is a different risk from one that is
	// routinely two months late, and a rate alone cannot tell them apart.
	out.debtorRisk = clamp(history.AverageDaysLate / 60)

	return out
}

/*
totalsAgree checks the document against itself.

An invoice whose line items do not add up to its total is not evidence of a debt; it is a
document somebody edited. The platform is told the verdict and not the numbers, and its
confidence gate refuses to auto-approve anything that failed here.
*/
func totalsAgree(document invoiceDocument) bool {
	if len(document.LineItems) == 0 {
		return false
	}

	var sum float64
	for _, item := range document.LineItems {
		amount, ok := decimal(item.Amount)
		if !ok {
			return false
		}
		sum += amount
	}

	total, ok := decimal(document.Total)
	if !ok {
		return false
	}
	// A cent of tolerance, because the document was written by somebody else's system and
	// rounding is not fraud.
	return math.Abs(sum-total) < 0.01
}

/*
confidence is how much of the document this workflow could actually read.

It is not a score of the receivable. It answers a narrower question the platform's gate
depends on: were the fields present, did the dates parse, did the totals hold. A document
that arrives half-empty produces a low confidence and no automatic approval, which is the
correct outcome for evidence nobody could check.
*/
func confidence(document invoiceDocument, arithmetic bool) float64 {
	present := 0
	for _, field := range []string{
		document.Number, document.DebtorRef, document.Currency,
		document.IssuedAt, document.DueAt, document.Total,
	} {
		if strings.TrimSpace(field) != "" {
			present++
		}
	}

	score := float64(present) / 6
	if !arithmetic {
		score -= 0.3
	}
	if _, _, ok := dates(document); !ok {
		score -= 0.2
	}
	if document.DebtorHistory.InvoicesPaid+document.DebtorHistory.InvoicesLate == 0 {
		// Nothing is claimed about the debtor, so the two features that come from its
		// history are guesses at zero. Saying so is the honest thing the gate can act on.
		score -= 0.15
	}
	return clamp(score)
}

/*
commit binds the result to the document and the run.

The digest covers the ciphertext hash, the nonce of this run, the version of this code and
the features that came out. That is enough for anybody holding the stored assessment to
recompute it and see that these numbers came from that document — and not enough to learn
anything about the document itself.
*/
func commit(work pendingWork, result assessment) string {
	material := strings.Join([]string{
		work.InvoiceID,
		strings.ToLower(work.CipherHash),
		work.Nonce,
		ModelVersion,
		result.Features.DSONorm,
		result.Features.LatePaymentRate,
		result.Features.DisputeFlag,
		result.Features.DebtorConcentration,
		result.Features.DebtorRisk,
		result.Confidence,
		fmt.Sprintf("%t", result.ArithmeticValid),
	}, "|")

	digest := sha256.Sum256([]byte(material))
	return "0x" + hex.EncodeToString(digest[:])
}

// dates parses the two the tenor depends on.
func dates(document invoiceDocument) (time.Time, time.Time, bool) {
	issued, err := time.Parse(time.RFC3339, strings.TrimSpace(document.IssuedAt))
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	due, err := time.Parse(time.RFC3339, strings.TrimSpace(document.DueAt))
	if err != nil || !due.After(issued) {
		return time.Time{}, time.Time{}, false
	}
	return issued, due, true
}

// decimal reads an amount written as a decimal string.
func decimal(value string) (float64, bool) {
	var parsed float64
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%f", &parsed); err != nil {
		return 0, false
	}
	return parsed, true
}

// mentions reports whether a clause contains a word, case-insensitively.
func mentions(terms, word string) bool {
	return strings.Contains(strings.ToLower(terms), word)
}

/*
claims reports whether a clause asserts something rather than denying it.

"No dispute has been raised" contains the word dispute and means the opposite of it. A
substring match read that sentence as a disputed invoice and priced it accordingly, which
is a whole grade of difference produced by ignoring one word.

The rule is a negation anywhere earlier in the same clause. That covers the sentences an
invoice actually contains — "no dispute has been raised", "the buyer has not raised a
dispute", "delivered free of dispute" — and it will get "there is no delay, but a dispute is
open" exactly wrong, because two claims in one clause are more than word-spotting can hold.

Which is the argument for putting a language model in this position, inside the enclave,
where it can read a clause instead of scanning it. That is what the specification asks of
AI: read the unstructured evidence, and leave the arithmetic to code that can be recomputed.
*/
func claims(terms, word string) bool {
	lower := strings.ToLower(terms)

	for _, clause := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == '.' || r == ';' || r == '\n'
	}) {
		index := strings.Index(clause, word)
		if index < 0 {
			continue
		}

		negated := false
		for _, candidate := range strings.Fields(clause[:index]) {
			switch strings.Trim(candidate, ",:()") {
			case "no", "not", "never", "without", "free":
				negated = true
			}
		}
		if !negated {
			return true
		}
	}
	return false
}

// clamp keeps a feature inside [0,1], which is the range the published model is defined on.
func clamp(value float64) float64 {
	switch {
	case math.IsNaN(value) || value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

// rate renders a feature the way the platform stores rates: a decimal string with six
// places, never a float on the wire.
func rate(value float64) string { return fmt.Sprintf("%.6f", value) }

// InitWorkflow registers the handler inside an attested enclave.
func InitWorkflow(config *Config, _ *slog.Logger, _ cre.SecretsProvider) (cre.Workflow[*Config], error) {
	return cre.Workflow[*Config]{
		cre.HandlerInTee(
			cron.Trigger(&cron.Config{Schedule: config.Schedule}),
			onSchedule,
			cre.OneOfTees{cre.Nitro{Regions: []cre.NitroRegion{cre.NitroUsWest2}}},
		),
	}, nil
}
