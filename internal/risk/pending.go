package risk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"
)

/*
ErrCollectedElsewhere says the assessment will not be produced by this call.

It is not a failure. A confidential workflow running on somebody else's infrastructure
cannot be called and waited on the way a library can: it wakes on its own schedule, collects
the work it finds, and returns the result later. So the request is left standing and the
invoice stays where it is until an answer arrives.

Everything that treats a workflow error as a failed assessment has to know this one is
different, which is why it is a named error rather than a nil result.
*/
var ErrCollectedElsewhere = errors.New("the confidential workflow collects its own work")

/*
PendingWorkflow is the port's implementation when the assessment runs somewhere else.

It does nothing, on purpose. The work is discoverable from what the platform already stores
— an invoice waiting to be extracted, with a document behind it — so there is nothing to
enqueue and no second place for a request to go missing between.
*/
type PendingWorkflow struct{}

// NewPendingWorkflow returns the implementation used when a confidential workflow collects
// its own work.
func NewPendingWorkflow() PendingWorkflow { return PendingWorkflow{} }

// Assess reports that the answer will arrive by another route.
func (PendingWorkflow) Assess(context.Context, WorkflowRequest) (WorkflowResult, error) {
	return WorkflowResult{}, ErrCollectedElsewhere
}

/*
DeriveNonce is the nonce a confidential run is bound to.

It is derived rather than generated because two parties have to agree on it without talking:
the workflow computes its commitment over it, and the platform recomputes that commitment to
check the result belongs to the request. A random nonce would have to be stored and handed
out, which is a row, a lookup and a way for the two to disagree.

Deriving it from the invoice and the ciphertext digest keeps the property that matters — one
document, one run, one commitment — and adds the property that a verifier holding the stored
assessment can recompute the whole chain without asking anybody for state.
*/
func DeriveNonce(invoiceID uuid.UUID, cipherHash string) string {
	sum := sha256.Sum256([]byte("factorflow:confidential:" +
		invoiceID.String() + "|" + strings.ToLower(strings.TrimSpace(cipherHash))))
	return hex.EncodeToString(sum[:16])
}
