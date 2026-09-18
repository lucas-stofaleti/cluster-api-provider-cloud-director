/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	cpiutil "github.com/vmware/cloud-provider-for-cloud-director/pkg/util"
	"github.com/vmware/cloud-provider-for-cloud-director/pkg/vcdsdk"
	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"
)

// This file is a copy of vcdsdk.AddNewTkgVM / vcdsdk.AddNewVM and their unexported helpers
// (cloud-provider-for-cloud-director pkg/vcdsdk/vapp.go), split in two so that only the part that
// needs the vApp-wide lock runs under it:
//   - resolveTkgVMCreationSpec does the catalog, template, policy and storage profile lookups,
//     none of which depend on the vApp's state;
//   - addTkgVMToVApp reads the vApp's network section and issues the recompose.
//
// The vendored AddNewVM also retrieves the whole vApp document first (roughly 20s for a
// 166-VM vApp) only to read its name, description, HREF and first network. Here the name,
// description and HREF come from the vApp the caller already fetched in the same reconcile, and
// the network names from the vApp's network section alone.
//
// The request sent to VCD must stay identical to the vendored one;
// TestAddTkgVMToVAppSendsTheSameRequestAsVendoredAddNewTkgVM enforces that. Keep this file in
// sync if the vendored code changes.

// tkgGuestCustomizationScript is copied verbatim from vcdsdk.AddNewTkgVM.
const tkgGuestCustomizationScript = `
#!/usr/bin/env bash
cat > /etc/cloud/cloud.cfg.d/98-cse-vmware-datasource.cfg <<EOF
datasource_list: [ "VMware" ]
EOF
`

var (
	vmCreationTrue  = true
	vmCreationFalse = false
)

// tkgVMCreationSpec holds everything about a new VM that can be resolved before taking the
// vApp-wide lock.
type tkgVMCreationSpec struct {
	vmName         string
	catalogName    string
	templateName   string
	templateHREF   string
	computePolicy  *types.ComputePolicy
	storageProfile *types.Reference
}

func resolveTkgVMCreationSpec(vdcManager *vcdsdk.VdcManager, vmName string, catalogName string, templateName string,
	placementPolicyName string, computePolicyName string, storageProfileName string) (*tkgVMCreationSpec, error) {

	if vdcManager == nil || vdcManager.Vdc == nil || vdcManager.Vdc.Vdc == nil {
		return nil, fmt.Errorf("no Vdc available to create VM [%s]", vmName)
	}

	// The vendored AddNewVM refreshes the VDC (as part of looking the vApp up) before reading its
	// storage profiles; refresh here too so the storage profile lookup behaves the same.
	if err := vdcManager.Vdc.Refresh(); err != nil {
		return nil, fmt.Errorf("error refreshing VDC [%s]: [%v]", vdcManager.VdcIdentifier, err)
	}

	orgManager, err := vcdsdk.NewOrgManager(vdcManager.Client, vdcManager.Client.ClusterOrgName)
	if err != nil {
		return nil, fmt.Errorf("error creating an orgManager object: [%v]", err)
	}

	templateHREF, err := getTemplateHREFFromName(vdcManager, orgManager, catalogName, templateName)
	if err != nil {
		return nil, fmt.Errorf("unable to get catalog/template [%s/%s] in org [%s]: [%v]",
			catalogName, templateName, vdcManager.OrgName, err)
	}

	computePolicy, err := getComputePolicy(orgManager, computePolicyName, placementPolicyName)
	if err != nil {
		return nil, fmt.Errorf("unable to get policies from computePolicy [%s], placementPolicy [%s] in org [%s]: [%v]",
			computePolicyName, placementPolicyName, vdcManager.OrgName, err)
	}

	storageProfile, err := getStorageProfile(vdcManager, storageProfileName)
	if err != nil {
		return nil, fmt.Errorf("unable to find storage Profile [%s] in ovdc [%s]: [%v]", storageProfileName,
			vdcManager.VdcIdentifier, err)
	}

	return &tkgVMCreationSpec{
		vmName:         vmName,
		catalogName:    catalogName,
		templateName:   templateName,
		templateHREF:   templateHREF,
		computePolicy:  computePolicy,
		storageProfile: storageProfile,
	}, nil
}

// addTkgVMToVApp issues the recompose that adds the VM described by spec to vApp. Callers must
// hold the vApp-wide lock. vApp must be the vApp as fetched earlier in the same reconcile; only
// its name, description and HREF are used from it.
func addTkgVMToVApp(vdcManager *vcdsdk.VdcManager, vApp *govcd.VApp, spec *tkgVMCreationSpec) (govcd.Task, error) {
	if vdcManager == nil || vdcManager.Client == nil || vdcManager.Client.VCDClient == nil {
		return govcd.Task{}, errors.New("no VCD client available to add a VM")
	}
	if vApp == nil || vApp.VApp == nil {
		return govcd.Task{}, errors.New("cannot add a VM to a nil vApp")
	}
	if spec == nil {
		return govcd.Task{}, fmt.Errorf("no VM creation spec to add a VM to vApp [%s]", vApp.VApp.Name)
	}

	apiEndpoint, err := url.ParseRequestURI(vApp.VApp.HREF)
	if err != nil {
		return govcd.Task{}, fmt.Errorf("invalid HREF [%s] for vApp [%s]: [%v]", vApp.VApp.HREF, vApp.VApp.Name, err)
	}
	apiEndpoint.Path += "/action/recomposeVApp"

	// Read under the lock, never from an earlier fetch: attaching a network to the vApp (which
	// takes the same lock) can change which network is listed first.
	networkConfig, err := vApp.GetNetworkConfig()
	if err != nil {
		return govcd.Task{}, fmt.Errorf("error for adding TKG VM to vApp[%s]: [unable to get network config of vApp [%s]: [%v]]",
			vApp.VApp.Name, vApp.VApp.Name, err)
	}
	networkNames := networkConfig.NetworkNames()
	if len(networkNames) == 0 {
		return govcd.Task{}, fmt.Errorf("error for adding TKG VM to vApp[%s]: [vApp has no network to connect VM [%s] to]",
			vApp.VApp.Name, spec.vmName)
	}

	// these are the settings used in VMware Cloud Director
	passwd, err := cpiutil.GeneratePassword(15, 5, 3, false, false)
	if err != nil {
		return govcd.Task{}, fmt.Errorf("failed to generate a password to create a VM in the VApp [%s]", vApp.VApp.Name)
	}

	vmDef := &types.ReComposeVAppParams{
		Ovf:                 types.XMLNamespaceOVF,
		Xsi:                 types.XMLNamespaceXSI,
		Xmlns:               types.XMLNamespaceVCloud,
		Name:                vApp.VApp.Name,
		Deploy:              false,
		PowerOn:             false,
		LinkedClone:         false,
		Description:         vApp.VApp.Description,
		VAppParent:          nil,
		InstantiationParams: nil,
		SourcedItem: &types.SourcedCompositionItemParam{
			Source: &types.Reference{
				HREF: spec.templateHREF,
				Name: spec.vmName,
			},
			// add this to enable Customization
			VMGeneralParams: &types.VMGeneralParams{
				Name:               spec.vmName,
				Description:        "Auto-created VM",
				NeedsCustomization: true,
				RegenerateBiosUuid: true,
			},
			VAppScopedLocalID: spec.vmName,
			InstantiationParams: &types.InstantiationParams{
				GuestCustomizationSection: &types.GuestCustomizationSection{
					Enabled:               &vmCreationTrue,
					AdminPasswordEnabled:  &vmCreationTrue,
					AdminPasswordAuto:     &vmCreationFalse,
					AdminPassword:         passwd,
					ResetPasswordRequired: &vmCreationFalse,
					ComputerName:          spec.vmName,
					CustomizationScript:   tkgGuestCustomizationScript,
				},
				NetworkConnectionSection: &types.NetworkConnectionSection{
					NetworkConnection: []*types.NetworkConnection{
						{
							Network:                 networkNames[0],
							NeedsCustomization:      false,
							IsConnected:             true,
							IPAddressAllocationMode: "POOL",
							NetworkAdapterType:      "VMXNET3",
						},
					},
				},
			},
			StorageProfile: spec.storageProfile,
			ComputePolicy:  spec.computePolicy,
		},
		AllEULAsAccepted: true,
		DeleteItem:       nil,
	}

	task, err := vdcManager.Client.VCDClient.Client.ExecuteTaskRequest(apiEndpoint.String(),
		http.MethodPost, types.MimeRecomposeVappParams, "error instantiating a new VM: [%s]",
		vmDef)
	if err != nil {
		return govcd.Task{}, fmt.Errorf("error for adding TKG VM to vApp[%s]: [unable to issue call to create VM [%s] in vApp [%s] with template [%s/%s]: [%v]]",
			vApp.VApp.Name, spec.vmName, vApp.VApp.Name, spec.catalogName, spec.templateName, err)
	}

	return task, nil
}

// getTemplateHREFFromName is copied from vcdsdk.VdcManager.getTemplateHREFFromName.
func getTemplateHREFFromName(vdcManager *vcdsdk.VdcManager, orgManager *vcdsdk.OrgManager,
	catalogName string, templateName string) (string, error) {

	catalog, err := orgManager.GetCatalogByName(catalogName)
	if err != nil {
		return "", fmt.Errorf("unable to find catalog [%s] in org [%s]: [%v]",
			catalogName, vdcManager.OrgName, err)
	}

	vAppTemplateList, err := catalog.QueryVappTemplateList()
	if err != nil {
		return "", fmt.Errorf("unable to query templates of catalog [%s]: [%v]", catalogName, err)
	}

	var queryVAppTemplate *types.QueryResultVappTemplateType = nil
	for _, template := range vAppTemplateList {
		if template.Name == templateName {
			queryVAppTemplate = template
			break
		}
	}
	if queryVAppTemplate == nil {
		return "", fmt.Errorf("unable to get template of name [%s] in catalog [%s]",
			templateName, catalogName)
	}
	vAppTemplate := govcd.NewVAppTemplate(&vdcManager.Client.VCDClient.Client)
	resp, err := vdcManager.Client.VCDClient.Client.ExecuteRequest(queryVAppTemplate.HREF, http.MethodGet,
		"", "error retrieving vApp template: %s", nil, vAppTemplate.VAppTemplate)
	if err != nil {
		return "", fmt.Errorf("unable to issue get for template with HREF [%s]: [%v], resp = [%v]",
			queryVAppTemplate.HREF, err, resp)
	}
	templateHref := vAppTemplate.VAppTemplate.HREF
	if vAppTemplate.VAppTemplate.Children != nil && len(vAppTemplate.VAppTemplate.Children.VM) != 0 {
		templateHref = vAppTemplate.VAppTemplate.Children.VM[0].HREF
	}

	return templateHref, nil
}

// getComputePolicy is copied from vcdsdk.VdcManager.getComputePolicy.
func getComputePolicy(orgManager *vcdsdk.OrgManager, computePolicyName string, placementPolicyName string) (*types.ComputePolicy, error) {
	var computePolicy *types.ComputePolicy = nil
	if placementPolicyName != "" {
		vmPlacementPolicy, err := orgManager.GetComputePolicyDetailsFromName(placementPolicyName)
		if err != nil {
			return nil, fmt.Errorf("unable to find placement policy [%s]: [%v]", placementPolicyName, err)
		}
		if computePolicy == nil {
			computePolicy = &types.ComputePolicy{}
		}
		computePolicy.VmPlacementPolicy = &types.Reference{
			HREF: vmPlacementPolicy.ID,
		}
	}

	if computePolicyName != "" {
		vmComputePolicy, err := orgManager.GetComputePolicyDetailsFromName(computePolicyName)
		if err != nil {
			return nil, fmt.Errorf("unable to find compute policy [%s]: [%v]", computePolicyName, err)
		}
		if computePolicy == nil {
			computePolicy = &types.ComputePolicy{}
		}
		computePolicy.VmSizingPolicy = &types.Reference{
			HREF: vmComputePolicy.ID,
		}
	}

	return computePolicy, nil
}

// getStorageProfile is copied from vcdsdk.VdcManager.getStorageProfile.
func getStorageProfile(vdcManager *vcdsdk.VdcManager, storageProfileName string) (*types.Reference, error) {
	if storageProfileName == "" {
		return nil, nil
	}

	if vdcManager.Vdc.Vdc.VdcStorageProfiles != nil {
		for _, profile := range vdcManager.Vdc.Vdc.VdcStorageProfiles.VdcStorageProfile {
			if profile.Name == storageProfileName {
				return profile, nil
			}
		}
	}

	return nil, fmt.Errorf("storage profile [%s] could not be found", storageProfileName)
}
