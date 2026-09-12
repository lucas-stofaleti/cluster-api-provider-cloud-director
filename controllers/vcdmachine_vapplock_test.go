/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestVAppMutationLockReturnsSameMutexForSameName is the core correctness property the whole
// in-process serialisation depends on: every call site for a given vApp must land on the exact
// same *sync.Mutex, or they would not exclude each other at all.
func TestVAppMutationLockReturnsSameMutexForSameName(t *testing.T) {
	r := &VCDMachineReconciler{}

	first := r.vAppMutationLock("dev-minimal")
	second := r.vAppMutationLock("dev-minimal")

	if first != second {
		t.Fatalf("expected the same *sync.Mutex for repeated calls with the same vApp name, got %p and %p", first, second)
	}
}

// TestVAppMutationLockReturnsDifferentMutexesForDifferentNames guards the multi-zone/multi-vApp
// case: machines in different zones use different vApp names and must never share a lock, or
// unrelated clusters/zones would serialise against each other for no reason.
func TestVAppMutationLockReturnsDifferentMutexesForDifferentNames(t *testing.T) {
	r := &VCDMachineReconciler{}

	a := r.vAppMutationLock("cluster-a")
	b := r.vAppMutationLock("cluster-b_md-pool1-ams")

	if a == b {
		t.Fatalf("expected different *sync.Mutex values for different vApp names, got the same pointer %p", a)
	}
}

// TestVAppMutationLockIsSafeUnderConcurrentFirstAccess exercises the exact race the lock exists
// to close: many goroutines (standing in for --concurrency reconciles) resolving the lock for
// the same brand-new vApp name for the first time simultaneously. sync.Map.LoadOrStore must give
// every one of them back the identical mutex; run with -race to catch anything else.
func TestVAppMutationLockIsSafeUnderConcurrentFirstAccess(t *testing.T) {
	r := &VCDMachineReconciler{}

	const goroutines = 50
	locks := make([]*sync.Mutex, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			locks[i] = r.vAppMutationLock("brand-new-vapp")
		}(i)
	}
	wg.Wait()

	for i := 1; i < goroutines; i++ {
		if locks[i] != locks[0] {
			t.Fatalf("goroutine %d got a different *sync.Mutex (%p) than goroutine 0 (%p) for the same vApp name",
				i, locks[i], locks[0])
		}
	}
}

// TestVAppMutationLockActuallySerialises proves the mutex returned for a given vApp name
// provides real mutual exclusion: concurrent critical sections increment a shared counter with a
// deliberate read-modify-write gap, which only stays correct if every increment is serialised by
// the lock. A broken lock (e.g. accidentally keyed per-call instead of per-name) would let this
// race and, under `go test -race`, be reported directly.
func TestVAppMutationLockActuallySerialises(t *testing.T) {
	r := &VCDMachineReconciler{}

	var counter int64
	var wg sync.WaitGroup
	const goroutines = 100
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			lock := r.vAppMutationLock("shared-vapp")
			lock.Lock()
			defer lock.Unlock()
			// Deliberately non-atomic read-modify-write: only correct under exclusion.
			current := atomic.LoadInt64(&counter)
			time.Sleep(time.Microsecond)
			atomic.StoreInt64(&counter, current+1)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&counter); got != goroutines {
		t.Fatalf("expected counter == %d after %d serialised increments, got %d (lock did not serialise callers)",
			goroutines, goroutines, got)
	}
}
