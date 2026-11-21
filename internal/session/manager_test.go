package session

import (
	"testing"
	"time"
)

// Run skips a round only after a real stand-still, not after a slow round.
func TestPaused(t *testing.T) {
	const every = 5 * time.Second
	for gap, want := range map[time.Duration]bool{
		0:                false,
		every:            false,
		2 * every:        false,
		11 * time.Second: false,
		12 * time.Second: true,
		45 * time.Second: true,
	} {
		if got := paused(gap, every); got != want {
			t.Errorf("paused(%s, %s) = %v, want %v", gap, every, got, want)
		}
	}
}
