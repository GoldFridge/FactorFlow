package auction

import (
	"regexp"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// MaxReasonLen bounds a cancellation or failure reason.
const MaxReasonLen = 512

// certificateHex matches an allocation certificate hash.
var certificateHex = regexp.MustCompile(`^0x[0-9a-f]{64}$`)

// Open starts accepting bids.
func (a *Auction) Open(now time.Time) error {
	if now.Before(a.OpensAt) {
		return apperr.Conflictf("auction %s opens at %s", a.ID, a.OpensAt.Format(time.RFC3339))
	}
	if !now.Before(a.ClosesAt) {
		return apperr.Conflictf("auction %s already closed at %s", a.ID, a.ClosesAt.Format(time.RFC3339))
	}
	return a.transition(StatusOpen, now)
}

// StartClearing closes bidding and hands the batch to the solver. It refuses to run before
// the closing time: the specification stops accepting bids first, then clears, so nobody
// can bid against a partially known result.
func (a *Auction) StartClearing(now time.Time) error {
	if a.Status == StatusOpen && now.Before(a.ClosesAt) {
		return apperr.Conflictf("auction %s still accepts bids until %s", a.ID, a.ClosesAt.Format(time.RFC3339))
	}
	if err := a.transition(StatusClearing, now); err != nil {
		return err
	}
	a.Reason = ""
	return nil
}

// MarkCleared records a completed clearing, including an empty allocation: a batch with no
// feasible match is CLEARED with nothing allocated, not FAILED.
func (a *Auction) MarkCleared(solverVersion, certificateHash string, now time.Time) error {
	solverVersion = strings.TrimSpace(solverVersion)
	if solverVersion == "" {
		return apperr.Invalid("solver_version", "must not be empty")
	}
	if !certificateHex.MatchString(strings.ToLower(certificateHash)) {
		return apperr.Invalid("certificate_hash", "must be a 0x-prefixed SHA-256 digest")
	}
	if err := a.transition(StatusCleared, now); err != nil {
		return err
	}
	a.SolverVersion = solverVersion
	a.CertificateHash = strings.ToLower(certificateHash)
	return nil
}

// FailClearing records a clearing that could not produce a verified allocation. The
// independent verifier rejecting a solution lands here.
func (a *Auction) FailClearing(reason string, now time.Time) error {
	return a.fail(reason, now)
}

// StartSettling begins executing the transfer plan.
func (a *Auction) StartSettling(now time.Time) error {
	if a.CertificateHash == "" {
		return apperr.Conflictf("auction %s has no allocation certificate to settle", a.ID)
	}
	if err := a.transition(StatusSettling, now); err != nil {
		return err
	}
	a.Reason = ""
	return nil
}

// MarkSettled records that every transfer in the plan is confirmed on chain.
func (a *Auction) MarkSettled(now time.Time) error {
	return a.transition(StatusSettled, now)
}

// FailSettlement records a settlement that stopped part way. Transfers already finalized
// on chain are not rolled back; reconciliation converges the local state before a retry.
func (a *Auction) FailSettlement(reason string, now time.Time) error {
	return a.fail(reason, now)
}

// Cancel ends the auction without settling.
func (a *Auction) Cancel(reason string, now time.Time) error {
	cleaned, err := cleanReason(reason)
	if err != nil {
		return err
	}
	if err := a.transition(StatusCancelled, now); err != nil {
		return err
	}
	a.Reason = cleaned
	return nil
}

// RetryClearing resumes a batch that failed during clearing.
func (a *Auction) RetryClearing(now time.Time) error {
	if a.Status != StatusFailed {
		return apperr.Conflictf("auction %s is %s, not %s", a.ID, a.Status, StatusFailed)
	}
	if a.CertificateHash != "" {
		return apperr.Conflictf("auction %s failed after clearing; retry settlement instead", a.ID)
	}
	if err := a.transition(StatusClearing, now); err != nil {
		return err
	}
	a.Reason = ""
	return nil
}

// RetrySettlement resumes a batch that failed during settlement, once reconciliation has
// established what actually happened on chain.
func (a *Auction) RetrySettlement(now time.Time) error {
	if a.Status != StatusFailed {
		return apperr.Conflictf("auction %s is %s, not %s", a.ID, a.Status, StatusFailed)
	}
	if a.CertificateHash == "" {
		return apperr.Conflictf("auction %s never cleared; retry clearing instead", a.ID)
	}
	if err := a.transition(StatusSettling, now); err != nil {
		return err
	}
	a.Reason = ""
	return nil
}

func (a *Auction) fail(reason string, now time.Time) error {
	cleaned, err := cleanReason(reason)
	if err != nil {
		return err
	}
	if err := a.transition(StatusFailed, now); err != nil {
		return err
	}
	a.Reason = cleaned
	return nil
}

// transition applies the state machine and the optimistic-concurrency rule.
func (a *Auction) transition(next Status, now time.Time) error {
	if !a.Status.IsValid() {
		return apperr.Conflictf("auction %s has unknown status %q", a.ID, a.Status)
	}
	if !a.Status.CanTransitionTo(next) {
		return apperr.Conflictf("auction %s cannot move from %s to %s", a.ID, a.Status, next)
	}
	a.Status = next
	a.Version++
	a.UpdatedAt = now.UTC()
	return nil
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
