package proxy

import (
	"sync"
	"testing"
)

// One user's messages take at most their share of the budget; other users
// still find room, and everything reserved comes back.
func TestBufferBudgetSharePerUser(t *testing.T) {
	b := newBufferBudget(100, 25)
	for i := 0; i < 5; i++ {
		if !b.tryAcquire("alice", 5) {
			t.Fatalf("alice's message %d did not fit her share", i)
		}
	}
	if b.tryAcquire("alice", 1) {
		t.Fatal("alice got more than her share of 25")
	}
	for _, u := range []string{"bob", "carol", "dave"} {
		if !b.tryAcquire(u, 25) {
			t.Fatalf("%s found no room while alice is at her share", u)
		}
	}
	if b.tryAcquire("erin", 1) {
		t.Fatal("the total budget of 100 was exceeded")
	}
	// A user who fails on the total gives back the share reserved for it.
	if got := b.userBytes("erin"); got != 0 {
		t.Fatalf("erin holds %d bytes after a refusal", got)
	}
	for i := 0; i < 5; i++ {
		b.release("alice", 5)
	}
	if !b.tryAcquire("erin", 25) {
		t.Fatal("alice's release did not return her bytes")
	}
	if got := b.userBytes("alice"); got != 0 {
		t.Fatalf("alice holds %d bytes after releasing everything", got)
	}
}

// Without a share (negative maxBufferedBytesPerUser) only the total counts.
func TestBufferBudgetWithoutShare(t *testing.T) {
	b := newBufferBudget(100, -1)
	if !b.tryAcquire("alice", 100) || b.tryAcquire("bob", 1) {
		t.Fatal("without a share one user may take the whole budget, and no more")
	}
	b.release("alice", 100)
	if !b.tryAcquire("bob", 1) {
		t.Fatal("the budget did not come back")
	}
	// A negative total buffers nothing (as before).
	if newBufferBudget(-1, 0).tryAcquire("alice", 1) {
		t.Fatal("a negative budget buffered a message")
	}
}

// Concurrent reservations never exceed the share or the total.
func TestBufferBudgetConcurrent(t *testing.T) {
	b := newBufferBudget(64, 16)
	var wg sync.WaitGroup
	for _, u := range []string{"a", "b", "c", "d", "e", "f"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if b.tryAcquire(u, 4) {
					if got := b.userBytes(u); got > 16 {
						t.Errorf("%s holds %d bytes, more than its share", u, got)
					}
					b.release(u, 4)
				}
			}
		}()
	}
	wg.Wait()
	if !b.tryAcquire("x", 16) || !b.tryAcquire("y", 16) || !b.tryAcquire("z", 16) || !b.tryAcquire("w", 16) {
		t.Fatal("bytes were lost")
	}
}
