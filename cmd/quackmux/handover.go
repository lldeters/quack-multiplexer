package main

import (
	"context"
	"log/slog"
	"time"
)

// handover takes a replica with leader election out of service when it
// shuts down, before the in-flight requests drain. The Service routes Quack
// traffic to the pod that carries the leader label, so the order matters:
//
//  1. Idle keep-alive connections close (their clients reconnect through the
//     Service), and the label goes (within unlabelTimeout; if that fails,
//     the readiness probe takes the pod out of the Service instead).
//  2. The lease is released: release ends the election, and released is
//     closed once it is over (normally within milliseconds, waited for at
//     most releaseTimeout). A follower takes over within seconds instead of
//     after the drain or the expiry of the lease.
//  3. If this process served, its pod was an endpoint of the Service, and
//     until kube-proxy has caught up with the removal of the label it still
//     sends new connections there. The Quack listener stays open until
//     delay (ha.deregistrationDelay) after the removal, so that they get an
//     answer (an error) instead of hanging in connect once the pod is gone.
//
// Then the caller closes the listener and drains.
type handover struct {
	keepAlives     func(enabled bool)
	unlabel        func(context.Context) error
	unlabelTimeout time.Duration
	release        func()
	released       <-chan struct{}
	releaseTimeout time.Duration
	served         func() bool
	delay          time.Duration
	log            *slog.Logger
}

// run hands over. The delay ends early when ctx (the shutdown deadline)
// ends.
func (h handover) run(ctx context.Context) {
	h.keepAlives(false)
	uctx, cancel := context.WithTimeout(context.Background(), h.unlabelTimeout)
	if err := h.unlabel(uctx); err != nil {
		h.log.Warn("could not remove the leader label before handing over; the readiness probe takes the pod out of the Service instead", "err", err)
	}
	cancel()
	unlabelled := time.Now()
	h.release()
	timeout := time.NewTimer(h.releaseTimeout)
	select {
	case <-h.released:
	case <-timeout.C:
		h.log.Warn("leader election did not stop in time; the lease may expire instead of being released")
	}
	timeout.Stop()
	if h.served() {
		wait := time.NewTimer(time.Until(unlabelled.Add(h.delay)))
		select {
		case <-wait.C:
		case <-ctx.Done():
		}
		wait.Stop()
	}
	h.log.Info("closing the Quack listener", "sinceLabelRemoval", time.Since(unlabelled).Round(time.Millisecond).String())
}
