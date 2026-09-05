package auction

import (
	"time"

	"github.com/google/uuid"
)

// NewForTest builds an auction directly in a given state, bypassing the state machine, so
// a test can prove a command's guard holds even for a record that the normal path cannot
// produce. It is only compiled into the test binary.
func NewForTest(status Status, certificateHash string) *Auction {
	return &Auction{
		ID:              uuid.New(),
		Status:          status,
		CertificateHash: certificateHash,
		Version:         1,
		CreatedAt:       time.Now().UTC(),
	}
}

// AllStatuses returns every state in the machine, so a test can assert over the table
// itself rather than a hand-maintained copy of it.
func AllStatuses() []Status {
	out := make([]Status, 0, len(transitions))
	for status := range transitions {
		out = append(out, status)
	}
	return out
}

// TransitionsFrom returns the states reachable from status in one step.
func TransitionsFrom(status Status) []Status { return transitions[status] }
