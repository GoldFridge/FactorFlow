package demo_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/GoldFridge/factorflow/internal/app/demo"
)

// TestSeededIDsAreTheSameEveryRun is the regression test for a demo that worked by luck:
// the confidential workflow derives an invoice's features from its identifier, so random
// ids gave every seed a different risk grade, and the seeded auction cleared on some runs
// and allocated nothing on others.
func TestSeededIDsAreTheSameEveryRun(t *testing.T) {
	t.Parallel()

	first, second := demo.IDs(), demo.IDs()

	var seen []uuid.UUID
	for i := 0; i < 8; i++ {
		id := first()
		assert.Equal(t, id, second(), "two runs of the seed must produce the same identifiers")
		assert.NotContains(t, seen, id, "and never the same one twice")
		seen = append(seen, id)
	}

	assert.NotEqual(t, uuid.Nil, seen[0])
}
