/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// A lock that should be free must be acquired within this; generous so slow CI or -race
	// never turns into a false failure.
	lockAcquireTimeout = 5 * time.Second
	// A lock that should be blocked must stay blocked for at least this long. A goroutine that
	// wrongly acquires does so immediately, so this cannot produce false failures.
	lockBlockedWindow = 150 * time.Millisecond
)

func acquireAsync(acquire func() func()) <-chan func() {
	ch := make(chan func(), 1)
	go func() { ch <- acquire() }()
	return ch
}

func mustAcquire(t *testing.T, ch <-chan func(), what string) func() {
	t.Helper()
	select {
	case unlock := <-ch:
		return unlock
	case <-time.After(lockAcquireTimeout):
		t.Fatalf("%s: not acquired within %v", what, lockAcquireTimeout)
		return nil
	}
}

func mustStayBlocked(t *testing.T, ch <-chan func(), what string) {
	t.Helper()
	select {
	case unlock := <-ch:
		unlock()
		t.Fatalf("%s: acquired, but should have been blocked", what)
	case <-time.After(lockBlockedWindow):
	}
}

// waitUntilWriterPending blocks until a goroutine is waiting for (or holding) the vApp-wide
// lock. TryRLock fails exactly when a writer is pending or active, which makes this
// deterministic instead of relying on the writer goroutine having been scheduled in time.
func waitUntilWriterPending(t *testing.T, l *vAppLockSet) {
	t.Helper()
	deadline := time.Now().Add(lockAcquireTimeout)
	for time.Now().Before(deadline) {
		if !l.vApp.TryRLock() {
			return
		}
		l.vApp.RUnlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("vApp-wide lock request never became pending")
}

func TestLockSetForReturnsSameSetForSameName(t *testing.T) {
	r := &VCDMachineReconciler{}
	if first, second := r.lockSetFor("dev-minimal"), r.lockSetFor("dev-minimal"); first != second {
		t.Fatalf("expected the same lock set for the same vApp name, got %p and %p", first, second)
	}
}

func TestLockSetForReturnsDifferentSetsForDifferentNames(t *testing.T) {
	r := &VCDMachineReconciler{}
	if a, b := r.lockSetFor("cluster-a"), r.lockSetFor("cluster-b_md-pool1-ams"); a == b {
		t.Fatalf("expected different lock sets for different vApp names, got the same %p", a)
	}
}

func TestLockSetForIsSafeUnderConcurrentFirstAccess(t *testing.T) {
	r := &VCDMachineReconciler{}
	const goroutines = 50
	sets := make([]*vAppLockSet, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			sets[i] = r.lockSetFor("brand-new-vapp")
		}(i)
	}
	wg.Wait()
	for i := 1; i < goroutines; i++ {
		if sets[i] != sets[0] {
			t.Fatalf("goroutine %d got lock set %p, goroutine 0 got %p", i, sets[i], sets[0])
		}
	}
}

func TestLockSetsForDifferentVAppsDoNotBlockEachOther(t *testing.T) {
	r := &VCDMachineReconciler{}
	unlockA := r.lockSetFor("vapp-a").lockVAppWide()
	defer unlockA()
	unlockB := mustAcquire(t, acquireAsync(r.lockSetFor("vapp-b").lockVAppWide), "vApp-wide lock of another vApp")
	unlockB()
}

func TestPerVMChangesRunConcurrently(t *testing.T) {
	l := &vAppLockSet{}
	unlock1 := l.lockVM()
	unlock2 := mustAcquire(t, acquireAsync(l.lockVM), "second per-VM lock while one is held")
	unlockNIC := mustAcquire(t, acquireAsync(l.lockVMNIC), "NIC lock while per-VM locks are held")
	unlockNIC()
	unlock2()
	unlock1()
}

func TestVAppWideLockExcludesPerVMChanges(t *testing.T) {
	l := &vAppLockSet{}
	unlockWide := l.lockVAppWide()

	vm := acquireAsync(l.lockVM)
	nic := acquireAsync(l.lockVMNIC)
	wide := acquireAsync(l.lockVAppWide)
	mustStayBlocked(t, vm, "per-VM lock while vApp-wide lock is held")
	mustStayBlocked(t, nic, "NIC lock while vApp-wide lock is held")
	mustStayBlocked(t, wide, "second vApp-wide lock while one is held")

	unlockWide()
	// The waiting requests can be granted in any order the lock allows; release each as it
	// arrives so all of them are eventually granted.
	granted := 0
	for granted < 3 {
		select {
		case unlock := <-vm:
			unlock()
			granted++
		case unlock := <-nic:
			unlock()
			granted++
		case unlock := <-wide:
			unlock()
			granted++
		case <-time.After(lockAcquireTimeout):
			t.Fatalf("only %d of 3 waiting requests were granted after the vApp-wide lock was released", granted)
		}
	}
}

func TestPerVMChangesBlockVAppWideLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(*vAppLockSet) func()
	}{
		{"per-VM lock", (*vAppLockSet).lockVM},
		{"NIC lock", (*vAppLockSet).lockVMNIC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &vAppLockSet{}
			unlock := tc.hold(l)
			wide := acquireAsync(l.lockVAppWide)
			mustStayBlocked(t, wide, "vApp-wide lock while "+tc.name+" is held")
			unlock()
			mustAcquire(t, wide, "vApp-wide lock after "+tc.name+" was released")()
		})
	}
}

func TestNICChangesAreSerialised(t *testing.T) {
	l := &vAppLockSet{}
	unlockNIC := l.lockVMNIC()

	secondNIC := acquireAsync(l.lockVMNIC)
	mustStayBlocked(t, secondNIC, "second NIC lock while one is held")
	mustAcquire(t, acquireAsync(l.lockVM), "per-VM lock while a NIC lock is held")()

	unlockNIC()
	mustAcquire(t, secondNIC, "second NIC lock after the first was released")()
}

func TestMetadataLockIsIndependentOfVAppLocks(t *testing.T) {
	l := &vAppLockSet{}
	unlockWide := l.lockVAppWide()
	unlockMeta := mustAcquire(t, acquireAsync(l.lockMetadata), "metadata lock while vApp-wide lock is held")

	secondMeta := acquireAsync(l.lockMetadata)
	mustStayBlocked(t, secondMeta, "second metadata lock while one is held")

	unlockWide()
	mustAcquire(t, acquireAsync(l.lockVMNIC), "NIC lock while the metadata lock is held")()

	unlockMeta()
	mustAcquire(t, secondMeta, "second metadata lock after the first was released")()
}

func TestWaitingVAppWideLockIsNotStarvedByNewPerVMChanges(t *testing.T) {
	l := &vAppLockSet{}
	unlockVM := l.lockVM()

	wide := acquireAsync(l.lockVAppWide)
	waitUntilWriterPending(t, l)

	lateVM := acquireAsync(l.lockVM)
	mustStayBlocked(t, lateVM, "new per-VM lock while a vApp-wide lock is waiting")

	unlockVM()
	unlockWide := mustAcquire(t, wide, "vApp-wide lock after the earlier per-VM lock was released")
	mustStayBlocked(t, lateVM, "new per-VM lock while the vApp-wide lock is held")

	unlockWide()
	mustAcquire(t, lateVM, "per-VM lock after the vApp-wide lock was released")()
}

// TestLockSetInvariantsUnderConcurrentLoad hammers one lock set from many goroutines and checks,
// at every acquisition, that nothing that must be excluded is active. It also fails if the
// workload does not finish, which would indicate a deadlock.
func TestLockSetInvariantsUnderConcurrentLoad(t *testing.T) {
	l := &vAppLockSet{}
	var wideActive, vmActive, nicActive, metaActive, violations int64
	const goroutines = 64
	const iterations = 300

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func(seed int64) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(seed))
				hold := func() { time.Sleep(time.Duration(rng.Intn(200)) * time.Microsecond) }
				for i := 0; i < iterations; i++ {
					switch rng.Intn(4) {
					case 0:
						unlock := l.lockVAppWide()
						if atomic.AddInt64(&wideActive, 1) != 1 || atomic.LoadInt64(&vmActive) != 0 {
							atomic.AddInt64(&violations, 1)
						}
						hold()
						atomic.AddInt64(&wideActive, -1)
						unlock()
					case 1:
						unlock := l.lockVM()
						atomic.AddInt64(&vmActive, 1)
						if atomic.LoadInt64(&wideActive) != 0 {
							atomic.AddInt64(&violations, 1)
						}
						hold()
						atomic.AddInt64(&vmActive, -1)
						unlock()
					case 2:
						unlock := l.lockVMNIC()
						atomic.AddInt64(&vmActive, 1)
						if atomic.AddInt64(&nicActive, 1) != 1 || atomic.LoadInt64(&wideActive) != 0 {
							atomic.AddInt64(&violations, 1)
						}
						hold()
						atomic.AddInt64(&nicActive, -1)
						atomic.AddInt64(&vmActive, -1)
						unlock()
					case 3:
						unlock := l.lockMetadata()
						if atomic.AddInt64(&metaActive, 1) != 1 {
							atomic.AddInt64(&violations, 1)
						}
						hold()
						atomic.AddInt64(&metaActive, -1)
						unlock()
					}
				}
			}(int64(g) + 1)
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("concurrent lock workload did not finish; possible deadlock")
	}
	if v := atomic.LoadInt64(&violations); v != 0 {
		t.Fatalf("%d lock exclusion violations observed", v)
	}
}
