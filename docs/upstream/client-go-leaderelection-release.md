> **Hinweis:** Entwurf eines Upstream-Bugreports für kubernetes/kubernetes; der Maintainer reicht ihn ein
> (unten: Vorlage „Bug Report“ und was vorher zu prüfen ist).

# client-go leaderelection: a leader that resumes after a pause releases the lease of the new holder

**Where to file:** [kubernetes/kubernetes](https://github.com/kubernetes/kubernetes/issues/new/choose),
template "Bug Report". client-go is published from `staging/src/k8s.io/client-go` there and takes no issues of
its own (client-go `CONTRIBUTING.md`). Suggested labels: `/kind bug`, `/sig api-machinery`. OWNERS of
`tools/leaderelection`: approver jefftree; reviewers wojtek-t, deads2k, ingvagabund.

**Upstream state, checked on 2026-11-25** (`git ls-remote` and a partial clone of
https://github.com/kubernetes/client-go):

| Ref | Commit | Holder check in the release |
|---|---|---|
| `master` | `96596e7a999d6a7b10f037ee1eabe384c4dd91a2` | yes, since `4563439f` |
| `v0.38.0-beta.0` | `0444ebe2bf7924342a50f74c9fbf9644532d8799` | yes |
| `release-1.37` | `489a20ab97256774fb537b1b1a1633c8be7d85f7` (`tools/leaderelection` identical to `v0.37.2`) | no |
| `v0.37.2` | `8e16766f8da2f011aa9e5e197edc7a3d84351753` | no |
| `release-1.36` | `a92355f831864a76c52a177ccd05c35c82d817ef` (same logic in `release()`) | no |
| `release-1.35` | `7bd3cb52585bbb82e4126d4bde695e34983a123e` (same logic in `release()`) | no |

The fix is on master as part of a feature: `4563439f729012b84179286a88bc9d191bf3d35e` "client-go/leaderelection:
add recovery mode and write gate" (KEP-6361, Kubernetes-commit `38677f6673cd`, merged on 2026-10-27 with
kubernetes/kubernetes#143289 according to the merge commit `ea0e89a3` in client-go). It adds the holder check to
`tryRelease` and the unit test `TestReleaseDoesNotClearAnotherHolder`. The feature will not be backported, the
bug fix should be. So the issue below asks for a cherry-pick of the one-line check and its test into the
supported release branches.

**Before filing:** search kubernetes/kubernetes for an existing issue or cherry-pick PR (for example
`TestReleaseDoesNotClearAnotherHolder`, `tryRelease HolderIdentity`); we could not search GitHub from our
environment. If a cherry-pick exists, add the reproduction below there instead.

---

**Title:** client-go leaderelection: with ReleaseOnCancel, a leader that resumes after a pause empties the new
holder's lease (fixed on master, please cherry-pick to 1.37, 1.36 and 1.35)

### What happened?

With `ReleaseOnCancel: true`, a leader that pauses for longer than `LeaseDuration` while another candidate takes
the expired lease over empties the lease of the new holder when it resumes. A pause is anything that stops the
process or its requests without killing it: SIGSTOP, a paused or migrated VM, a long stop-the-world or I/O stall,
CPU starvation, or a network partition that heals while the release is still being attempted.

1. Candidate A holds the lease and renews it.
2. A freezes. Its lease expires, candidate B acquires it (`HolderIdentity` B, new `resourceVersion`).
3. A resumes. Its pending renewal has missed `RenewDeadline`, so `renew()` gives up and, because of
   `ReleaseOnCancel`, calls `release()` (v0.37.2: `leaderelection.go:310`); no context needs to be cancelled.
4. `tryRelease` reads the lease with `Lock.Get`. That returns B's record, and `LeaseLock` keeps B's object with its
   `resourceVersion` for the next update. But `tryRelease` then decides with `le.IsLeader()`
   (`leaderelection.go:338`), which compares its identity with `observedRecord`: the record A stored at its last
   successful renewal, which still names A.
5. So A writes the release record (`HolderIdentity` empty, `LeaseDurationSeconds` 1). The update carries the
   `resourceVersion` it has just read, optimistic concurrency does not stop it, and the API server accepts it. B's
   lease is gone, and B does not know.

What follows depends on the other candidates. Any candidate, including a restarted A, can acquire the empty lease
at once; B acts as leader until its next renewal finds the lease taken and gives up after `RenewDeadline`. For
that time two processes act as leader, and then B's leadership ends: a second, unnecessary failover. With only two
candidates B acquires the empty lease again at its next renewal, with `LeaderTransitions` incremented.

We hit this in an HA test of our gateway (two replicas on kind, Kubernetes v1.37, client-go v0.37.2): the leader
was stopped with SIGSTOP until the follower had taken over, then continued with SIGCONT. It emptied the new
leader's lease, and we saw the second failover and, briefly, two processes that both considered themselves
leader.

### What did you expect to happen?

A release clears the lease only if the record it has just read names the releasing candidate. Here A should
leave B's lease alone and return. That is what master does since `4563439f`.

### How can we reproduce it (as minimally and precisely as possible)?

Two electors with `ReleaseOnCancel` on the fake clientset. A's lock calls block while A is "frozen" and fail with
the context error once it thaws, as an HTTP request whose deadline passed would. The fake's object tracker does not
check `resourceVersion` on updates, so the test adds reactors that reject stale updates like the API server does:
the release succeeds because it carries the current `resourceVersion`, not because the fake is lax.

`go.mod`:

```
module repro

go 1.26.0

require (
	k8s.io/api v0.37.2
	k8s.io/apimachinery v0.37.2
	k8s.io/client-go v0.37.2
)
```

`release_test.go` (then `go mod tidy && go test -count=1 -v .`):

<details>
<summary>release_test.go</summary>

```go
// Reproduction for k8s.io/client-go/tools/leaderelection: with
// ReleaseOnCancel, a leader that resumes after a freeze, while another
// candidate took the lease over, releases (empties) the lease of the new
// holder.
package repro

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/leaderelection"
	rl "k8s.io/client-go/tools/leaderelection/resourcelock"
)

const namespace, name = "default", "repro"

// Candidate a leads and freezes (SIGSTOP, a paused VM, a long GC or I/O
// stall) for longer than its lease. Candidate b takes the expired lease over.
// When a resumes, its pending renewal has missed the renew deadline, renew()
// gives up, and ReleaseOnCancel releases the lease. The lease is b's by now:
// a must leave it alone.
func TestReleaseAfterFreezeKeepsTheNewHoldersLease(t *testing.T) {
	client := fake.NewClientset()
	writes := recordWrites(client)

	a := &freezableLock{Interface: leaseLock(client, "a")}
	leadingA, doneA := run(t, a, 2*time.Second, time.Second, 200*time.Millisecond)
	wait(t, leadingA, "a to lead")
	leadingB, doneB := run(t, leaseLock(client, "b"), 10*time.Second, 5*time.Second, 2*time.Second)

	a.freeze()
	wait(t, leadingB, "b to take the expired lease over")
	a.thaw()
	wait(t, doneA, "a to stop leading")

	select {
	case <-doneB:
		t.Fatal("b stopped leading")
	default: // b still leads, as far as it knows
	}
	for _, w := range writes.after("b") {
		if w.holder == "" {
			t.Errorf("a released the lease after b had taken it over: write %d set holder %q, leaseDurationSeconds %d", w.seq, w.holder, w.leaseDurationSeconds)
		}
	}
	lease, err := client.CoordinationV1().Leases(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lease writes: %v", writes.all())
	t.Logf("lease after a stopped: holder %q, leaseDurationSeconds %d", holder(lease), *lease.Spec.LeaseDurationSeconds)
}

// freezableLock stands in for a process that freezes: while frozen, calls of
// the lock block; when it thaws, a call whose context ended in the meantime
// fails, as an HTTP request would.
type freezableLock struct {
	rl.Interface
	mu     sync.Mutex
	thawed chan struct{} // nil while running
}

func (l *freezableLock) freeze() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.thawed = make(chan struct{})
}

func (l *freezableLock) thaw() {
	l.mu.Lock()
	defer l.mu.Unlock()
	close(l.thawed)
	l.thawed = nil
}

func (l *freezableLock) block(ctx context.Context) error {
	l.mu.Lock()
	thawed := l.thawed
	l.mu.Unlock()
	if thawed != nil {
		<-thawed
	}
	return ctx.Err()
}

func (l *freezableLock) Get(ctx context.Context) (*rl.LeaderElectionRecord, []byte, error) {
	if err := l.block(ctx); err != nil {
		return nil, nil, err
	}
	return l.Interface.Get(ctx)
}

func (l *freezableLock) Create(ctx context.Context, r rl.LeaderElectionRecord) error {
	if err := l.block(ctx); err != nil {
		return err
	}
	return l.Interface.Create(ctx, r)
}

func (l *freezableLock) Update(ctx context.Context, r rl.LeaderElectionRecord) error {
	if err := l.block(ctx); err != nil {
		return err
	}
	return l.Interface.Update(ctx, r)
}

func leaseLock(client *fake.Clientset, identity string) rl.Interface {
	return &rl.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: namespace, Name: name},
		Client:     client.CoordinationV1(),
		LockConfig: rl.ResourceLockConfig{Identity: identity},
	}
}

// run starts an elector with ReleaseOnCancel. leading is closed when it
// starts leading, done when Run has returned.
func run(t *testing.T, lock rl.Interface, leaseDuration, renewDeadline, retryPeriod time.Duration) (leading, done <-chan struct{}) {
	t.Helper()
	l, d := make(chan struct{}), make(chan struct{})
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) { close(l) },
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(d)
		le.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-d
	})
	return l, d
}

type write struct {
	seq                  int
	holder               string
	leaseDurationSeconds int32
}

type writeLog struct {
	mu     sync.Mutex
	writes []write
}

func (w *writeLog) all() []write {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]write(nil), w.writes...)
}

// after returns the writes that followed the first one naming holder.
func (w *writeLog) after(holder string) []write {
	all := w.all()
	for i, x := range all {
		if x.holder == holder {
			return all[i+1:]
		}
	}
	return nil
}

// recordWrites logs every successful create and update of the lease, and
// makes the fake clientset reject updates with a stale resourceVersion as
// the API server does (the fake's object tracker does not check it).
func recordWrites(c *fake.Clientset) *writeLog {
	gvr := coordinationv1.SchemeGroupVersion.WithResource("leases")
	log := &writeLog{}
	rv := 0
	store := func(lease *coordinationv1.Lease, create bool) (runtime.Object, error) {
		rv++
		lease.ResourceVersion = strconv.Itoa(rv)
		var err error
		if create {
			err = c.Tracker().Create(gvr, lease, lease.Namespace)
		} else {
			err = c.Tracker().Update(gvr, lease, lease.Namespace)
		}
		if err != nil {
			return nil, err
		}
		var d int32
		if lease.Spec.LeaseDurationSeconds != nil {
			d = *lease.Spec.LeaseDurationSeconds
		}
		log.writes = append(log.writes, write{seq: len(log.writes) + 1, holder: holder(lease), leaseDurationSeconds: d})
		return lease, nil
	}
	c.PrependReactor("create", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		log.mu.Lock()
		defer log.mu.Unlock()
		obj, err := store(a.(k8stesting.CreateAction).GetObject().(*coordinationv1.Lease).DeepCopy(), true)
		return true, obj, err
	})
	c.PrependReactor("update", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		log.mu.Lock()
		defer log.mu.Unlock()
		lease := a.(k8stesting.UpdateAction).GetObject().(*coordinationv1.Lease).DeepCopy()
		current, err := c.Tracker().Get(gvr, lease.Namespace, lease.Name)
		if err != nil {
			return true, nil, err
		}
		if current.(*coordinationv1.Lease).ResourceVersion != lease.ResourceVersion {
			return true, nil, apierrors.NewConflict(gvr.GroupResource(), lease.Name, errors.New("the object has been modified"))
		}
		obj, err := store(lease, false)
		return true, obj, err
	})
	return log
}

func holder(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}

func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
```

</details>

With client-go v0.37.2 (go1.26.0, linux/amd64) it fails; write 5 is A's release, after B's takeover (3) and
renewal (4):

```
=== RUN   TestReleaseAfterFreezeKeepsTheNewHoldersLease
I1125 11:33:07.362776    3882 leaderelection.go:258] "Attempting to acquire leader lease..." lock="default/repro"
I1125 11:33:07.365098    3882 leaderelection.go:272] "Successfully acquired lease" lock="default/repro"
I1125 11:33:07.365487    3882 leaderelection.go:258] "Attempting to acquire leader lease..." lock="default/repro"
I1125 11:33:11.123778    3882 leaderelection.go:272] "Successfully acquired lease" lock="default/repro"
E1125 11:33:11.124346    3882 leaderelection.go:473] "Error retrieving lease lock" err="context deadline exceeded" lock="default/repro"
I1125 11:33:11.124558    3882 leaderelection.go:304] "Failed to renew lease" lock="default/repro" err="context deadline exceeded"
    release_test.go:53: a released the lease after b had taken it over: write 5 set holder "", leaseDurationSeconds 1
    release_test.go:60: lease writes: [{1 a 2} {2 a 2} {3 b 10} {4 b 10} {5  1}]
    release_test.go:61: lease after a stopped: holder "", leaseDurationSeconds 1
--- FAIL: TestReleaseAfterFreezeKeepsTheNewHoldersLease (3.86s)
FAIL
```

With the fix below applied to v0.37.2, and with v0.38.0-beta.0, it passes:

```
    release_test.go:60: lease writes: [{1 a 2} {2 a 2} {3 b 10} {4 b 10}]
    release_test.go:61: lease after a stopped: holder "b", leaseDurationSeconds 10
--- PASS: TestReleaseAfterFreezeKeepsTheNewHoldersLease (2.61s)
```

The unit test that master added, `TestReleaseDoesNotClearAnotherHolder`, applies to v0.37.2 unchanged. Without the
fix it fails (`leaderelection_test.go:1346: release must not update a lease held by someone else`); with it, it
passes, and so does `go test ./tools/leaderelection/...`.

### Anything else we need to know?

v0.37.2, `tools/leaderelection/leaderelection.go`:

```go
func (le *LeaderElector) tryRelease(ctx context.Context) bool {
	logger := klog.FromContext(ctx)
	oldLeaderElectionRecord, _, err := le.config.Lock.Get(ctx)
	// ... error handling ...

	if !le.IsLeader() { // observedRecord, i.e. the state at the last renewal
		return true
	}
	now := metav1.NewTime(le.clock.Now())
	leaderElectionRecord := rl.LeaderElectionRecord{
		LeaderTransitions:    oldLeaderElectionRecord.LeaderTransitions,
		LeaseDurationSeconds: 1,
		RenewTime:            now,
		AcquireTime:          now,
	}
	if err := le.config.Lock.Update(ctx, leaderElectionRecord); err != nil {
```

The record read right before is used for `LeaderTransitions` only. The fix, as on master (`4563439f`), decides on
it as well:

```diff
--- a/staging/src/k8s.io/client-go/tools/leaderelection/leaderelection.go
+++ b/staging/src/k8s.io/client-go/tools/leaderelection/leaderelection.go
@@ -335,7 +335,10 @@ func (le *LeaderElector) tryRelease(ctx context.Context) bool {
 		return false
 	}
 
-	if !le.IsLeader() {
+	// Check the record just fetched as well as the cached one: after an
+	// outage the cache can still name this elector as the holder while the
+	// lease has changed hands, and releasing would clear the new holder.
+	if !le.IsLeader() || oldLeaderElectionRecord.HolderIdentity != le.config.Lock.Identity() {
 		return true
 	}
 	now := metav1.NewTime(le.clock.Now())
```

plus `TestReleaseDoesNotClearAnotherHolder` from master. release-1.36 and release-1.35 have no `tryRelease`; the
same condition stands in `release()` after its `Lock.Get`, and the same change applies there. We verified the
patch and the test on v0.37.2 (= release-1.37) only.

Workaround on unfixed versions: wrap the `resourcelock.Interface`, remember the `HolderIdentity` of the last
`Get`, and fail an `Update` whose record has an empty `HolderIdentity` (a release) unless that holder is the own
identity; the elector then logs "Failed to release lease" and the lease of the new holder stays. quackmux does
this in `internal/ha/ha.go` (`renewTracker`, `errNotHolder`).

### Kubernetes version

client-go v0.37.2 (also the head of release-1.37; release-1.36 and release-1.35 have the same logic). Seen with
kind v0.33.0 and the node image kindest/node v1.37.0; the reproduction needs no cluster.

### Cloud provider

None (kind).

### OS version

Linux 6.18, amd64.

### Install tools

kind v0.33.0.

### Container runtime (CRI) and version (if applicable)

containerd of the kind node image.

### Related plugins (CNI, CSI, ...) and versions (if applicable)

None.
