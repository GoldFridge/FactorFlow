package invoice

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/skimer2king/factorflow/internal/platform/apperr"
)

// MaxReasonLen bounds a rejection or failure reason. Reasons are operator-facing text and
// must never carry document content.
const MaxReasonLen = 512

// MarkUploaded records that encrypted document bytes were stored for this invoice.
func (i *Invoice) MarkUploaded(now time.Time) error {
	return i.transition(StatusUploaded, now)
}

// StartAssessment hands the invoice to the confidential workflow. It is also the retry
// path after an assessment failure.
func (i *Invoice) StartAssessment(now time.Time) error {
	if i.Status == StatusFailed && i.FailedFrom != StatusExtracting {
		return apperr.Conflictf("invoice %s failed during %s and cannot resume assessment", i.ID, i.FailedFrom)
	}
	if err := i.transition(StatusExtracting, now); err != nil {
		return err
	}
	i.clearFailure()
	return nil
}

// CompleteAssessment attaches the immutable risk assessment produced by the workflow.
func (i *Invoice) CompleteAssessment(assessmentID uuid.UUID, now time.Time) error {
	if assessmentID == uuid.Nil {
		return apperr.Invalid("assessment_id", "must be a non-nil UUID")
	}
	if err := i.transition(StatusAssessed, now); err != nil {
		return err
	}
	i.AssessmentID = assessmentID
	return nil
}

// FailAssessment records a failed confidential assessment, keeping the stage so a retry
// resumes the right step.
func (i *Invoice) FailAssessment(reason string, now time.Time) error {
	return i.fail(reason, now)
}

// Approve records the issuer confirming the extracted facts. It requires an assessment,
// because approval without a score would let an unpriced receivable reach the market.
func (i *Invoice) Approve(now time.Time) error {
	if i.AssessmentID == uuid.Nil {
		return apperr.Conflictf("invoice %s cannot be approved without a risk assessment", i.ID)
	}
	return i.transition(StatusApproved, now)
}

// Reject ends the lifecycle before tokenization.
func (i *Invoice) Reject(reason string, now time.Time) error {
	cleaned, err := cleanReason(reason)
	if err != nil {
		return err
	}
	if err := i.transition(StatusRejected, now); err != nil {
		return err
	}
	i.Reason = cleaned
	return nil
}

// StartTokenization begins ATS issuance. It is also the retry path after a failed
// issuance.
func (i *Invoice) StartTokenization(now time.Time) error {
	if i.Status == StatusFailed && i.FailedFrom != StatusTokenizing {
		return apperr.Conflictf("invoice %s failed during %s and cannot resume tokenization", i.ID, i.FailedFrom)
	}
	if err := i.transition(StatusTokenizing, now); err != nil {
		return err
	}
	i.clearFailure()
	return nil
}

// CompleteTokenization records the issued asset once the chain receipt is final.
func (i *Invoice) CompleteTokenization(assetID uuid.UUID, now time.Time) error {
	if assetID == uuid.Nil {
		return apperr.Invalid("asset_id", "must be a non-nil UUID")
	}
	if err := i.transition(StatusTokenized, now); err != nil {
		return err
	}
	i.AssetID = assetID
	return nil
}

// FailTokenization records a failed issuance.
func (i *Invoice) FailTokenization(reason string, now time.Time) error {
	return i.fail(reason, now)
}

// OpenAuction publishes the tokenized asset for bidding.
func (i *Invoice) OpenAuction(now time.Time) error {
	return i.transition(StatusAuctionOpen, now)
}

// CancelAuction returns an unsold asset to the tokenized state. It is the invoice-side
// counterpart of an auction reaching CANCELLED.
func (i *Invoice) CancelAuction(reason string, now time.Time) error {
	cleaned, err := cleanReason(reason)
	if err != nil {
		return err
	}
	if i.Status != StatusAuctionOpen {
		return apperr.Conflictf("invoice %s is %s, not %s", i.ID, i.Status, StatusAuctionOpen)
	}
	if err := i.transition(StatusTokenized, now); err != nil {
		return err
	}
	i.Reason = cleaned
	return nil
}

// MarkAllocated records a cleared auction. Settlement has not executed yet.
func (i *Invoice) MarkAllocated(now time.Time) error {
	return i.transition(StatusAllocated, now)
}

// MarkSettled records that the transfer plan finished on chain.
func (i *Invoice) MarkSettled(now time.Time) error {
	return i.transition(StatusSettled, now)
}

// FailSettlement records a settlement that could not be completed. Transfers already
// finalized on chain are not rolled back; reconciliation converges the local state.
func (i *Invoice) FailSettlement(reason string, now time.Time) error {
	return i.fail(reason, now)
}

// MarkMatured records repayment. Early repayment is allowed, so there is no due-date
// check here: the demo settles maturity on a receivable that is not yet due.
func (i *Invoice) MarkMatured(now time.Time) error {
	return i.transition(StatusMatured, now)
}

// MarkDefaulted records non-payment.
func (i *Invoice) MarkDefaulted(reason string, now time.Time) error {
	cleaned, err := cleanReason(reason)
	if err != nil {
		return err
	}
	if err := i.transition(StatusDefaulted, now); err != nil {
		return err
	}
	i.Reason = cleaned
	return nil
}

// fail moves the invoice to FAILED, remembering the stage that failed.
func (i *Invoice) fail(reason string, now time.Time) error {
	cleaned, err := cleanReason(reason)
	if err != nil {
		return err
	}
	failedFrom := i.Status
	if err := i.transition(StatusFailed, now); err != nil {
		return err
	}
	i.FailedFrom = failedFrom
	i.Reason = cleaned
	return nil
}

// transition applies the state machine and the aggregate's optimistic-concurrency rule.
func (i *Invoice) transition(next Status, now time.Time) error {
	if !i.Status.IsValid() {
		return apperr.Conflictf("invoice %s has unknown status %q", i.ID, i.Status)
	}
	if !i.Status.CanTransitionTo(next) {
		return apperr.Conflictf("invoice %s cannot move from %s to %s", i.ID, i.Status, next)
	}
	i.Status = next
	i.Version++
	i.UpdatedAt = now.UTC()
	return nil
}

func (i *Invoice) clearFailure() {
	i.FailedFrom = ""
	i.Reason = ""
}

func cleanReason(reason string) (string, error) {
	cleaned := strings.TrimSpace(reason)
	if cleaned == "" {
		return "", apperr.Invalid("reason", "must not be empty")
	}
	if len(cleaned) > MaxReasonLen {
		return "", apperr.Invalid("reason", "must be at most %d characters", MaxReasonLen)
	}
	return cleaned, nil
}
