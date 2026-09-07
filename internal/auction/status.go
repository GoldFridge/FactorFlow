package auction

import (
	"fmt"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Status is a state of the auction lifecycle defined in the specification:
//
//	DRAFT -> OPEN -> CLEARING -> CLEARED -> SETTLING -> SETTLED
//	      \-> CANCELLED     \-> FAILED           \-> FAILED
type Status string

// The auction lifecycle states.
const (
	StatusDraft     Status = "DRAFT"
	StatusOpen      Status = "OPEN"
	StatusClearing  Status = "CLEARING"
	StatusCleared   Status = "CLEARED"
	StatusSettling  Status = "SETTLING"
	StatusSettled   Status = "SETTLED"
	StatusCancelled Status = "CANCELLED"
	StatusFailed    Status = "FAILED"
)

// transitions is the whole auction state machine.
var transitions = map[Status][]Status{
	StatusDraft:    {StatusOpen, StatusCancelled},
	StatusOpen:     {StatusClearing, StatusCancelled},
	StatusClearing: {StatusCleared, StatusFailed},
	StatusCleared:  {StatusSettling, StatusCancelled},
	StatusSettling: {StatusSettled, StatusFailed},
	// A failed clearing can be retried once its cause is fixed, and a failed settlement
	// resumes from CLEARED after reconciliation has established what actually happened on
	// chain. Neither retry may skip a stage.
	StatusFailed:    {StatusClearing, StatusSettling, StatusCancelled},
	StatusSettled:   nil,
	StatusCancelled: nil,
}

// ParseStatus validates a status read from storage or an API payload.
func ParseStatus(s string) (Status, error) {
	status := Status(s)
	if !status.IsValid() {
		return "", fmt.Errorf("%w: unknown auction status %q", apperr.ErrValidation, s)
	}
	return status, nil
}

// IsValid reports whether the status is part of the lifecycle.
func (s Status) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal reports whether the auction can no longer move.
func (s Status) IsTerminal() bool {
	return s.IsValid() && len(transitions[s]) == 0
}

// CanTransitionTo reports whether next is reachable from s in one step.
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// String returns the wire representation of the status.
func (s Status) String() string { return string(s) }

// BidStatus is the state of a single investor bid.
type BidStatus string

// The bid states. A bid is only ever cancelled by its own investor before clearing starts.
const (
	BidStatusActive    BidStatus = "ACTIVE"
	BidStatusCancelled BidStatus = "CANCELLED"
	BidStatusAllocated BidStatus = "ALLOCATED"
	BidStatusRejected  BidStatus = "REJECTED"
)

// IsValid reports whether the bid status is known.
func (s BidStatus) IsValid() bool {
	switch s {
	case BidStatusActive, BidStatusCancelled, BidStatusAllocated, BidStatusRejected:
		return true
	default:
		return false
	}
}

// String returns the wire representation.
func (s BidStatus) String() string { return string(s) }
