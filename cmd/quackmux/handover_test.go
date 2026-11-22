package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// handoverRun records what a handover does, and when.
type handoverRun struct {
	mu     sync.Mutex
	events []string
	at     map[string]time.Time
	logs   bytes.Buffer
}

func (r *handoverRun) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	r.at[event] = time.Now()
}

func (r *handoverRun) order() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, " ")
}

// newHandover returns a handover with fakes: unlabel takes unlabelFor and
// fails with unlabelErr, the election ends releaseAfter after release (never
// if negative).
func newHandover(r *handoverRun, served bool, delay, unlabelFor time.Duration, unlabelErr error, releaseAfter time.Duration) handover {
	released := make(chan struct{})
	return handover{
		keepAlives: func(on bool) { r.record(fmt.Sprintf("keepalives=%v", on)) },
		unlabel: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				r.record("unlabel without deadline")
			}
			select {
			case <-time.After(unlabelFor):
			case <-ctx.Done():
				r.record("unlabel gave up")
				return ctx.Err()
			}
			r.record("unlabelled")
			return unlabelErr
		},
		unlabelTimeout: time.Second,
		release: func() {
			r.record("release")
			if releaseAfter >= 0 {
				time.AfterFunc(releaseAfter, func() { r.record("released"); close(released) })
			}
		},
		released:       released,
		releaseTimeout: 300 * time.Millisecond,
		served:         func() bool { return served },
		delay:          delay,
		log:            slog.New(slog.NewTextHandler(&lockedWriter{mu: &r.mu, w: &r.logs}, nil)),
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// A leader that served removes its label, releases the lease and keeps the
// Quack listener open until ha.deregistrationDelay after the label is gone.
func TestHandoverOrderAndDelay(t *testing.T) {
	r := &handoverRun{at: map[string]time.Time{}}
	h := newHandover(r, true, 400*time.Millisecond, 50*time.Millisecond, nil, 20*time.Millisecond)
	h.run(context.Background())
	r.record("closing")
	if got, want := r.order(), "keepalives=false unlabelled release released closing"; got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	if d := r.at["closing"].Sub(r.at["unlabelled"]); d < h.delay {
		t.Fatalf("the listener closes %s after the label removal, want at least the delay %s", d, h.delay)
	} else if d > h.delay+200*time.Millisecond {
		t.Fatalf("the listener closes %s after the label removal, want about the delay %s", d, h.delay)
	}
	if !strings.Contains(r.logs.String(), "closing the Quack listener") {
		t.Errorf("no log line for closing the listener:\n%s", r.logs.String())
	}
}

// The delay counts from the label removal: a slow release does not add to it.
func TestHandoverDelayIncludesTheRelease(t *testing.T) {
	r := &handoverRun{at: map[string]time.Time{}}
	h := newHandover(r, true, 300*time.Millisecond, 0, nil, 200*time.Millisecond)
	h.run(context.Background())
	r.record("closing")
	if d := r.at["closing"].Sub(r.at["unlabelled"]); d < h.delay || d > h.delay+150*time.Millisecond {
		t.Fatalf("closes %s after the label removal, want about %s", d, h.delay)
	}
}

// A follower was no endpoint of the Service: no delay.
func TestHandoverFollowerDoesNotWait(t *testing.T) {
	r := &handoverRun{at: map[string]time.Time{}}
	h := newHandover(r, false, time.Hour, 0, nil, 0)
	start := time.Now()
	h.run(context.Background())
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("a follower waited %s", d)
	}
}

// Neither a failed label removal nor an election that does not end stops
// the handover; the shutdown deadline ends the delay.
func TestHandoverFailures(t *testing.T) {
	t.Run("label removal fails", func(t *testing.T) {
		r := &handoverRun{at: map[string]time.Time{}}
		h := newHandover(r, true, 100*time.Millisecond, 0, errors.New("injected: patch refused"), 0)
		h.run(context.Background())
		if got := r.order(); !strings.Contains(got, "unlabelled release released") {
			t.Fatalf("order %q: the lease was not released after the failed removal", got)
		}
		if !strings.Contains(r.logs.String(), "injected: patch refused") {
			t.Errorf("the failure is not logged:\n%s", r.logs.String())
		}
	})
	t.Run("label removal hangs", func(t *testing.T) {
		r := &handoverRun{at: map[string]time.Time{}}
		h := newHandover(r, true, 0, time.Hour, nil, 0)
		h.unlabelTimeout = 50 * time.Millisecond
		start := time.Now()
		h.run(context.Background())
		if d := time.Since(start); d > time.Second {
			t.Fatalf("took %s", d)
		}
		if got := r.order(); !strings.Contains(got, "unlabel gave up release") || strings.Contains(got, "without deadline") {
			t.Fatalf("order %q", got)
		}
	})
	t.Run("election does not end", func(t *testing.T) {
		r := &handoverRun{at: map[string]time.Time{}}
		h := newHandover(r, true, 0, 0, nil, -1)
		start := time.Now()
		h.run(context.Background())
		if d := time.Since(start); d < h.releaseTimeout || d > h.releaseTimeout+200*time.Millisecond {
			t.Fatalf("took %s, want about the release timeout %s", d, h.releaseTimeout)
		}
		if !strings.Contains(r.logs.String(), "did not stop in time") {
			t.Errorf("not logged:\n%s", r.logs.String())
		}
	})
	t.Run("shutdown deadline", func(t *testing.T) {
		r := &handoverRun{at: map[string]time.Time{}}
		h := newHandover(r, true, time.Hour, 0, nil, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		h.run(ctx)
		if d := time.Since(start); d > time.Second {
			t.Fatalf("the delay outlasted the shutdown deadline: %s", d)
		}
	})
}
