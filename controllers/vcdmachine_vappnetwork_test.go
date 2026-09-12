/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"testing"

	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"
)

// vAppWithNetworks builds a minimal *govcd.VApp carrying exactly the network names
// given, mirroring the shape vAppHasNetworkAttached (and the vendored
// isVappNetworkPresentInVapp it must stay in sync with) reads.
func vAppWithNetworks(names ...string) *govcd.VApp {
	var networkConfig []types.VAppNetworkConfiguration
	for _, name := range names {
		networkConfig = append(networkConfig, types.VAppNetworkConfiguration{NetworkName: name})
	}
	return &govcd.VApp{
		VApp: &types.VApp{
			NetworkConfigSection: &types.NetworkConfigSection{
				NetworkConfig: networkConfig,
			},
		},
	}
}

func TestVAppHasNetworkAttached(t *testing.T) {
	testCases := []struct {
		name            string
		vApp            *govcd.VApp
		ovdcNetworkName string
		expected        bool
	}{
		{
			name:            "nil vApp",
			vApp:            nil,
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name:            "vApp with nil VApp field",
			vApp:            &govcd.VApp{VApp: nil},
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name:            "nil NetworkConfigSection",
			vApp:            &govcd.VApp{VApp: &types.VApp{NetworkConfigSection: nil}},
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name: "nil NetworkConfig slice",
			vApp: &govcd.VApp{VApp: &types.VApp{
				NetworkConfigSection: &types.NetworkConfigSection{NetworkConfig: nil},
			}},
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name:            "empty NetworkConfig slice",
			vApp:            vAppWithNetworks(),
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name:            "network present, exact match",
			vApp:            vAppWithNetworks("ci-k8s-network"),
			ovdcNetworkName: "ci-k8s-network",
			expected:        true,
		},
		{
			name:            "network present alongside unrelated networks",
			vApp:            vAppWithNetworks("some-other-network", "ci-k8s-network", "yet-another-network"),
			ovdcNetworkName: "ci-k8s-network",
			expected:        true,
		},
		{
			name:            "network absent, only unrelated networks present",
			vApp:            vAppWithNetworks("some-other-network"),
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name:            "case mismatch does not match",
			vApp:            vAppWithNetworks("CI-K8S-NETWORK"),
			ovdcNetworkName: "ci-k8s-network",
			expected:        false,
		},
		{
			name:            "empty requested network name against real networks",
			vApp:            vAppWithNetworks("ci-k8s-network"),
			ovdcNetworkName: "",
			expected:        false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := vAppHasNetworkAttached(tc.vApp, tc.ovdcNetworkName)
			if got != tc.expected {
				t.Errorf("vAppHasNetworkAttached() = %v, want %v", got, tc.expected)
			}
		})
	}
}
