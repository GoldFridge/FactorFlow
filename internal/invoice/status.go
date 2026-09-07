package invoice

import (
	"fmt"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
)

// Status is a state of the invoice lifecycle defined in the specification:
//
//	DRAFT -> UPLOADED -> EXTRACTING -> ASSESSED -> APPROVED -> TOKENIZING -> TOKENIZED
//	      \-> REJECTED            \-> FAILED               \-> FAILED
//	TOKENIZED -> AUCTION_OPEN -> ALLOCATED -> SETTLED -> MATURED
//	                                                 \-> DEFAULTED
type Status string

// The invoice lifecycle states.
const (
	StatusDraft       Status = "DRAFT"
	StatusUploaded    Status = "UPLOADED"
	StatusExtracting  Status = "EXTRACTING"
	StatusAssessed    Status = "ASSESSED"
	StatusApproved    Status = "APPROVED"
	StatusRejected    Status = "REJECTED"
	StatusFailed      Status = "FAILED"
	StatusTokenizing  Status = "TOKENIZING"
	StatusTokenized   Status = "TOKENIZED"
	StatusAuctionOpen Status = "AUCTION_OPEN"
	StatusAllocated   Status = "ALLOCATED"
	StatusSettled     Status = "SETTLED"
	StatusMatured     Status = "MATURED"
	StatusDefaulted   Status = "DEFAULTED"
)

// transitions is the whole state machine. A transition that is not listed here cannot
// happen, whatever the calling code believes: the domain is the only place that moves an
// invoice, and clients never write status directly.
var transitions = map[Status][]Status{
	StatusDraft:      {StatusUploaded, StatusRejected},
	StatusUploaded:   {StatusExtracting, StatusRejected},
	StatusExtracting: {StatusAssessed, StatusFailed},
	StatusAssessed:   {StatusApproved, StatusRejected},
	StatusApproved:   {StatusTokenizing},
	StatusTokenizing: {StatusTokenized, StatusFailed},
	// A failed assessment, issuance or settlement is retried manually after an operator
	// has looked at the cause; the stage to resume is recorded on the invoice as
	// FailedFrom, so a retry cannot silently restart a different step.
	StatusFailed:      {StatusExtracting, StatusTokenizing, StatusAllocated, StatusRejected},
	StatusTokenized:   {StatusAuctionOpen},
	StatusAuctionOpen: {StatusAllocated, StatusTokenized},
	StatusAllocated:   {StatusSettled, StatusFailed},
	StatusSettled:     {StatusMatured, StatusDefaulted},
	StatusRejected:    nil,
	StatusMatured:     nil,
	StatusDefaulted:   nil,
}

// ParseStatus validates a status read from storage or an API payload.
func ParseStatus(s string) (Status, error) {
	status := Status(s)
	if !status.IsValid() {
		return "", fmt.Errorf("%w: unknown invoice status %q", apperr.ErrValidation, s)
	}
	return status, nil
}

// IsValid reports whether the status is part of the lifecycle.
func (s Status) IsValid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal reports whether the invoice can no longer move.
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
