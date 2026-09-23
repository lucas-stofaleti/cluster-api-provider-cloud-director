/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

func testMachine(name string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
}

func TestIsVAppContentionError(t *testing.T) {
	testCases := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{
			// The exact shape observed in production logs.
			name:     "VDC_RECOMPOSE_VAPP",
			err:      fmt.Errorf(`API Error: 400: [ 470472-... ] Unable to perform this action. Contact your cloud administrator. VDC_RECOMPOSE_VAPP(com.vmware.vcloud.entity.task:02726804)`),
			expected: true,
		},
		{
			name:     "VAPP_UPDATE_VM",
			err:      fmt.Errorf(`API Error: 400: [ x ] Unable to perform this action. VAPP_UPDATE_VM(com.vmware.vcloud.entity.task:abc)`),
			expected: true,
		},
		{
			name:     "VAPP_DEPLOY",
			err:      fmt.Errorf(`API Error: 400: [ x ] Unable to perform this action. VAPP_DEPLOY(com.vmware.vcloud.entity.task:abc)`),
			expected: true,
		},
		{
			name:     "BUSY_ENTITY minor code",
			err:      fmt.Errorf(`majorErrorCode="400" minorErrorCode="BUSY_ENTITY"`),
			expected: true,
		},
		{
			// Deliberately NOT treated as contention: VCD returns this same generic
			// message for real faults, so matching it would retry genuine errors forever.
			name:     "bare 500 with no entity code is NOT contention",
			err:      fmt.Errorf(`API Error: 500: [ 469634-... ] Unable to perform this action. Contact your cloud administrator.`),
			expected: false,
		},
		{
			name:     "unrelated error",
			err:      fmt.Errorf("connection refused"),
			expected: false,
		},
		{
			// The real errors reach us wrapped several layers deep with %v/%s.
			name:     "contention code survives deep wrapping",
			err:      fmt.Errorf("unable to provision infrastructure: [%v]", fmt.Errorf("error for adding TKG VM to vApp[c]: [%v]", fmt.Errorf("VDC_RECOMPOSE_VAPP(com.vmware.vcloud.entity.task:x)"))),
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVAppContentionError(tc.err); got != tc.expected {
				t.Errorf("isVAppContentionError() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestDeferOnVAppContentionDefersThenGivesUp(t *testing.T) {
	r := &VCDMachineReconciler{}
	ctx := context.Background()
	machine := testMachine("m1")
	contention := fmt.Errorf("VDC_RECOMPOSE_VAPP(com.vmware.vcloud.entity.task:x)")

	// Up to the bound, every contention error should be deferred with a positive requeue.
	for i := 1; i <= maxVAppContentionDeferrals; i++ {
		res, deferred := r.deferOnVAppContention(ctx, machine, contention)
		if !deferred {
			t.Fatalf("attempt %d: expected deferral, got none", i)
		}
		if res.RequeueAfter < vAppContentionRequeueBase {
			t.Errorf("attempt %d: RequeueAfter %v below base %v", i, res.RequeueAfter, vAppContentionRequeueBase)
		}
		if res.RequeueAfter >= vAppContentionRequeueBase+vAppContentionRequeueJitter {
			t.Errorf("attempt %d: RequeueAfter %v above base+jitter", i, res.RequeueAfter)
		}
	}

	// Past the bound the error must be handed back so it surfaces and backoff applies.
	if _, deferred := r.deferOnVAppContention(ctx, machine, contention); deferred {
		t.Error("expected deferral to stop after the bound was exceeded")
	}

	// Exceeding the bound resets the counter, so the machine is not permanently poisoned.
	if _, deferred := r.deferOnVAppContention(ctx, machine, contention); !deferred {
		t.Error("expected the counter to reset after giving up")
	}
}

func TestDeferOnVAppContentionIgnoresNonContention(t *testing.T) {
	r := &VCDMachineReconciler{}
	ctx := context.Background()
	machine := testMachine("m2")

	for _, err := range []error{
		fmt.Errorf("connection refused"),
		fmt.Errorf(`API Error: 500: [ x ] Unable to perform this action. Contact your cloud administrator.`),
	} {
		res, deferred := r.deferOnVAppContention(ctx, machine, err)
		if deferred {
			t.Errorf("error %q should not be deferred", err)
		}
		if res.RequeueAfter != 0 {
			t.Errorf("error %q should not request a requeue, got %v", err, res.RequeueAfter)
		}
	}
}

func TestVAppContentionCountersArePerMachine(t *testing.T) {
	r := &VCDMachineReconciler{}
	ctx := context.Background()
	contention := fmt.Errorf("BUSY_ENTITY")
	a, b := testMachine("a"), testMachine("b")

	// Drive machine a to its limit.
	for i := 0; i < maxVAppContentionDeferrals; i++ {
		r.deferOnVAppContention(ctx, a, contention)
	}
	if _, deferred := r.deferOnVAppContention(ctx, a, contention); deferred {
		t.Error("machine a should have stopped deferring")
	}
	// Machine b must be unaffected.
	if _, deferred := r.deferOnVAppContention(ctx, b, contention); !deferred {
		t.Error("machine b should still defer; counters must be per machine")
	}
}

func TestClearVAppContentionResetsTheCounter(t *testing.T) {
	r := &VCDMachineReconciler{}
	ctx := context.Background()
	machine := testMachine("m3")
	contention := fmt.Errorf("BUSY_ENTITY")

	for i := 0; i < maxVAppContentionDeferrals-1; i++ {
		r.deferOnVAppContention(ctx, machine, contention)
	}
	r.clearVAppContention(machine)

	// After clearing, the machine gets a full budget again.
	for i := 1; i <= maxVAppContentionDeferrals; i++ {
		if _, deferred := r.deferOnVAppContention(ctx, machine, contention); !deferred {
			t.Fatalf("attempt %d after clear: expected deferral", i)
		}
	}
}
