package invoice

import "github.com/google/uuid"

// NewForTest builds an aggregate directly in a given state, bypassing the state machine.
// It exists so a test can prove a command's guard holds even for a state that the normal
// path cannot produce, such as a record corrupted in storage. It is only compiled into the
// test binary.
func NewForTest(status Status, assessmentID uuid.UUID) *Invoice {
	return &Invoice{
		ID:           uuid.New(),
		Status:       status,
		AssessmentID: assessmentID,
		Version:      1,
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
