package proxy

import (
	"sync"

	"golang.org/x/sync/semaphore"
)

// bufferBudget is the memory for buffered messages
// (server.upstream.bufferBudgetBytes), with a share per user
// (server.upstream.maxBufferedBytesPerUser): one user's uploads or results
// cannot take the whole budget and push everyone else onto streaming. A
// message that finds no room is streamed, as before.
type bufferBudget struct {
	total   *semaphore.Weighted
	perUser int64 // <= 0: no share per user

	mu   sync.Mutex
	used map[string]int64 // bytes per user
}

func newBufferBudget(total, perUser int64) *bufferBudget {
	return &bufferBudget{total: semaphore.NewWeighted(max(total, 0)), perUser: perUser, used: map[string]int64{}}
}

// tryAcquire reserves n bytes for user, or reports false without waiting.
func (b *bufferBudget) tryAcquire(user string, n int64) bool {
	if b.perUser > 0 {
		b.mu.Lock()
		if b.used[user]+n > b.perUser {
			b.mu.Unlock()
			return false
		}
		b.used[user] += n
		b.mu.Unlock()
	}
	if b.total.TryAcquire(n) {
		return true
	}
	b.unreserve(user, n)
	return false
}

// release returns n bytes that tryAcquire reserved for user.
func (b *bufferBudget) release(user string, n int64) {
	b.total.Release(n)
	b.unreserve(user, n)
}

func (b *bufferBudget) unreserve(user string, n int64) {
	if b.perUser <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used[user] -= n; b.used[user] <= 0 {
		delete(b.used, user)
	}
}

// userBytes reports the bytes reserved for user (tests).
func (b *bufferBudget) userBytes(user string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used[user]
}
