package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/GoldFridge/factorflow/internal/platform/clock"
)

var testAt = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

func TestFixedClockMovesOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	c := clock.At(testAt)
	assert.True(t, c.IsFixed())
	assert.Equal(t, testAt, c.Now())

	c.Advance(90 * time.Minute)
	assert.Equal(t, testAt.Add(90*time.Minute), c.Now())

	c.Set(testAt.Add(48 * time.Hour))
	assert.Equal(t, testAt.Add(48*time.Hour), c.Now())
}

// TestAFixedClockNeverGoesBackwards protects the ordering the audit timeline is read in:
// events written under a clock that rewound could not be put in the order they happened.
func TestAFixedClockNeverGoesBackwards(t *testing.T) {
	t.Parallel()

	c := clock.At(testAt)
	c.Advance(time.Hour)

	c.Advance(-time.Hour)
	assert.Equal(t, testAt.Add(time.Hour), c.Now())

	c.Set(testAt)
	assert.Equal(t, testAt.Add(time.Hour), c.Now(), "an instant already past is ignored")
}

// TestALiveClockCannotBeMoved is the property that keeps this type safe to wire into a
// running server: nothing it is handed to can change what time the service thinks it is.
func TestALiveClockCannotBeMoved(t *testing.T) {
	t.Parallel()

	c := clock.Live()
	assert.False(t, c.IsFixed())

	before := time.Now()
	c.Advance(24 * time.Hour)
	c.Set(before.Add(365 * 24 * time.Hour))

	assert.WithinDuration(t, before, c.Now(), time.Minute)
}

func TestTimesAreUTC(t *testing.T) {
	t.Parallel()

	local := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60))

	c := clock.At(local)
	assert.Equal(t, time.UTC, c.Now().Location())
	assert.True(t, c.Now().Equal(local))

	c.Set(local.Add(time.Hour))
	assert.Equal(t, time.UTC, c.Now().Location())
}

// TestAClockIsSafeForConcurrentUse matters because the services reading it and the seeder
// moving it are not serialized by anything else.
func TestAClockIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	c := clock.At(testAt)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.Advance(time.Second) }()
		go func() { defer wg.Done(); _ = c.Now() }()
	}
	wg.Wait()

	assert.Equal(t, testAt.Add(8*time.Second), c.Now())
}
