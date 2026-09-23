/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/cloud-provider-for-cloud-director/pkg/vcdsdk"
	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"
)

// fakeVCD is a minimal VCD API serving exactly what the vendored vcdsdk.AddNewTkgVM and this
// package's copy of it request: one org, one VDC, one vApp, one catalog with one template, and
// the VDC compute policies. It records every request so tests can compare what was sent.
type fakeVCD struct {
	t   *testing.T
	srv *httptest.Server

	// vApp networks as returned in the full vApp document and by the networkConfigSection
	// endpoint; tests may set them differently to simulate a change between two reads.
	vAppDocumentNetworks []string
	networkSectionNets   []string
	storageProfiles      []string
	computePolicies      []string
	templateNames        []string
	recomposeStatus      int
	recomposeBody        string

	mu        sync.Mutex
	requests  []string
	recompose []capturedRequest
	unhandled []string
}

type capturedRequest struct {
	path        string
	contentType string
	body        string
}

const (
	fakeOrgName     = "CI"
	fakeVdcName     = "PoC"
	fakeVAppName    = "dev-minimal"
	fakeCatalogName = "Kubernetes Catalog"
)

func newFakeVCD(t *testing.T) *fakeVCD {
	f := &fakeVCD{
		t:                    t,
		vAppDocumentNetworks: []string{"ci-k8s-network"},
		networkSectionNets:   []string{"ci-k8s-network"},
		storageProfiles:      []string{"Premium SSD Tier (Amsterdam, VM Based)", "Premium SSD Tier (Rotterdam, VM Based)"},
		computePolicies:      []string{"k8s-2m-1c", "Medium", "Amsterdam", "Rotterdam"},
		templateNames:        []string{"ubuntu-24.04_k8s-1.35.6_20260817"},
		recomposeStatus:      http.StatusAccepted,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVCD) href(path string) string { return f.srv.URL + path }

func (f *fakeVCD) writeXML(w http.ResponseWriter, status int, v interface{}) {
	out, err := xml.Marshal(v)
	if err != nil {
		f.t.Errorf("fake VCD: marshal %T: %v", v, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/*+xml;version=37.2")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func networkConfigSection(names []string) *types.NetworkConfigSection {
	section := &types.NetworkConfigSection{}
	for _, name := range names {
		section.NetworkConfig = append(section.NetworkConfig, types.VAppNetworkConfiguration{NetworkName: name})
	}
	return section
}

func (f *fakeVCD) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	if q := r.URL.Query().Get("type"); q != "" {
		key += "?type=" + q
	}
	f.mu.Lock()
	f.requests = append(f.requests, key)
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/versions":
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<SupportedVersions xmlns="http://www.vmware.com/vcloud/versions">` +
			`<VersionInfo deprecated="false"><Version>37.2</Version></VersionInfo></SupportedVersions>`))

	case r.Method == http.MethodGet && r.URL.Path == "/api/org":
		f.writeXML(w, http.StatusOK, &types.OrgList{Org: []*types.Org{{HREF: f.href("/api/org/org-1"), Name: fakeOrgName}}})

	case r.Method == http.MethodGet && r.URL.Path == "/api/org/org-1":
		f.writeXML(w, http.StatusOK, &types.Org{HREF: f.href("/api/org/org-1"), Name: fakeOrgName, ID: "urn:vcloud:org:org-1"})

	case r.Method == http.MethodGet && r.URL.Path == "/api/vdc/vdc-1":
		vdc := &types.Vdc{
			HREF: f.href("/api/vdc/vdc-1"), Name: fakeVdcName, ID: "urn:vcloud:vdc:vdc-1",
			ResourceEntities: []*types.ResourceEntities{{ResourceEntity: []*types.ResourceReference{{
				HREF: f.href("/api/vApp/vapp-1"), Name: fakeVAppName, Type: "application/vnd.vmware.vcloud.vApp+xml",
			}}}},
			VdcStorageProfiles: &types.VdcStorageProfiles{},
		}
		for i, name := range f.storageProfiles {
			vdc.VdcStorageProfiles.VdcStorageProfile = append(vdc.VdcStorageProfiles.VdcStorageProfile, &types.Reference{
				HREF: f.href(fmt.Sprintf("/api/vdcStorageProfile/sp-%d", i)), Name: name, Type: "application/vnd.vmware.vcloud.vdcStorageProfile+xml",
			})
		}
		f.writeXML(w, http.StatusOK, vdc)

	case r.Method == http.MethodGet && r.URL.Path == "/api/vApp/vapp-1":
		f.writeXML(w, http.StatusOK, &types.VApp{
			HREF: f.href("/api/vApp/vapp-1"), Name: fakeVAppName, ID: "urn:vcloud:vapp:vapp-1",
			Description:          "Description for [dev-minimal]",
			NetworkConfigSection: networkConfigSection(f.vAppDocumentNetworks),
		})

	case r.Method == http.MethodGet && r.URL.Path == "/api/vApp/vapp-1/networkConfigSection/":
		f.writeXML(w, http.StatusOK, networkConfigSection(f.networkSectionNets))

	case r.Method == http.MethodGet && r.URL.Path == "/api/query" && r.URL.Query().Get("type") == "catalog":
		f.writeXML(w, http.StatusOK, &types.QueryResultRecordsType{Total: 1, Page: 1, PageSize: 25,
			CatalogRecord: []*types.CatalogRecord{{HREF: f.href("/api/catalog/cat-1"), Name: fakeCatalogName, OrgName: fakeOrgName}}})

	case r.Method == http.MethodGet && r.URL.Path == "/api/catalog/cat-1":
		f.writeXML(w, http.StatusOK, &types.Catalog{HREF: f.href("/api/catalog/cat-1"), Name: fakeCatalogName, ID: "urn:vcloud:catalog:cat-1"})

	case r.Method == http.MethodGet && r.URL.Path == "/api/query" && r.URL.Query().Get("type") == "vAppTemplate":
		records := &types.QueryResultRecordsType{Total: float64(len(f.templateNames)), Page: 1, PageSize: 25}
		for i, name := range f.templateNames {
			records.VappTemplateRecord = append(records.VappTemplateRecord, &types.QueryResultVappTemplateType{
				HREF: f.href(fmt.Sprintf("/api/vAppTemplate/vappTemplate-%d", i)), Name: name, CatalogName: fakeCatalogName,
			})
		}
		f.writeXML(w, http.StatusOK, records)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/vAppTemplate/vappTemplate-"):
		f.writeXML(w, http.StatusOK, &types.VAppTemplate{
			HREF: f.href(r.URL.Path), Name: "template",
			Children: &types.VAppTemplateChildren{VM: []*types.VAppTemplate{{HREF: f.href(r.URL.Path + "-vm"), Name: "template-vm"}}},
		})

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/cloudapi/") && strings.Contains(r.URL.Path, "vdcComputePolicies"):
		var values []map[string]interface{}
		for i, name := range f.computePolicies {
			values = append(values, map[string]interface{}{"id": fmt.Sprintf("urn:vcloud:vdcComputePolicy:policy-%d", i), "name": name})
		}
		raw, _ := json.Marshal(values)
		out, _ := json.Marshal(map[string]interface{}{"resultTotal": len(values), "pageCount": 1, "page": 1, "pageSize": 128, "values": json.RawMessage(raw)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)

	case r.Method == http.MethodPost && r.URL.Path == "/api/vApp/vapp-1/action/recomposeVApp":
		f.mu.Lock()
		f.recompose = append(f.recompose, capturedRequest{path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: string(body)})
		f.mu.Unlock()
		if f.recomposeStatus != http.StatusAccepted {
			w.Header().Set("Content-Type", "application/*+xml;version=37.2")
			w.WriteHeader(f.recomposeStatus)
			_, _ = w.Write([]byte(f.recomposeBody))
			return
		}
		f.writeXML(w, http.StatusAccepted, &types.Task{HREF: f.href("/api/task/task-1"), Status: "running", OperationName: "vdcRecomposeVapp"})

	default:
		f.mu.Lock()
		f.unhandled = append(f.unhandled, key+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeVCD) resetRecording() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
	f.recompose = nil
	f.unhandled = nil
}

func (f *fakeVCD) recorded() (requests []string, recompose []capturedRequest, unhandled []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]capturedRequest(nil), f.recompose...), append([]string(nil), f.unhandled...)
}

// vdcManager builds a vcdsdk.VdcManager wired to the fake, the same shape CAPVCD gets from
// vcdsdk.NewVDCManager (the VDC document already fetched once).
func (f *fakeVCD) vdcManager() *vcdsdk.VdcManager {
	f.t.Helper()
	endpoint, err := url.ParseRequestURI(f.srv.URL + "/api")
	if err != nil {
		f.t.Fatalf("parse fake URL: %v", err)
	}
	vcdClient := govcd.NewVCDClient(*endpoint, true)
	client := &vcdsdk.Client{VCDClient: vcdClient, ClusterOrgName: fakeOrgName, ClusterOVDCIdentifier: fakeVdcName}
	vdc := govcd.NewVdc(&vcdClient.Client)
	vdc.Vdc = &types.Vdc{HREF: f.href("/api/vdc/vdc-1"), Name: fakeVdcName}
	if err := vdc.Refresh(); err != nil {
		f.t.Fatalf("initial VDC fetch from fake: %v", err)
	}
	return &vcdsdk.VdcManager{OrgName: fakeOrgName, VdcIdentifier: fakeVdcName, Vdc: vdc, Client: client}
}

var adminPasswordRe = regexp.MustCompile(`<AdminPassword>[^<]*</AdminPassword>`)

func maskPassword(body string) string {
	return adminPasswordRe.ReplaceAllString(body, "<AdminPassword>MASKED</AdminPassword>")
}

const (
	testVMName   = "dev-minimal-md-pool1-ams-abcde-12345"
	testTemplate = "ubuntu-24.04_k8s-1.35.6_20260817"
	testStorage  = "Premium SSD Tier (Amsterdam, VM Based)"
)

type vmCreationInput struct {
	placement, sizing, storage string
}

// addViaNewPath runs the new code the way reconcileVM does: the vApp is the one fetched earlier
// in the reconcile, the lookups run first, then addTkgVMToVApp (the part under the lock).
func addViaNewPath(t *testing.T, f *fakeVCD, in vmCreationInput) (govcd.Task, error) {
	t.Helper()
	vdcManager := f.vdcManager()
	vApp, err := vdcManager.Vdc.GetVAppByName(fakeVAppName, true)
	if err != nil {
		t.Fatalf("fetch vApp from fake: %v", err)
	}
	spec, err := resolveTkgVMCreationSpec(vdcManager, testVMName, fakeCatalogName, testTemplate, in.placement, in.sizing, in.storage)
	if err != nil {
		return govcd.Task{}, err
	}
	return addTkgVMToVApp(vdcManager, vApp, spec)
}

func TestAddTkgVMToVAppSendsTheSameRequestAsVendoredAddNewTkgVM(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       vmCreationInput
		networks []string
	}{
		{"worker with placement, sizing and storage", vmCreationInput{"Amsterdam", "k8s-2m-1c", testStorage}, []string{"ci-k8s-network"}},
		{"control plane with sizing only", vmCreationInput{"", "Medium", testStorage}, []string{"ci-k8s-network"}},
		{"no compute or placement policy", vmCreationInput{"", "", testStorage}, []string{"ci-k8s-network"}},
		{"no storage profile", vmCreationInput{"Rotterdam", "k8s-2m-1c", ""}, []string{"ci-k8s-network"}},
		{"vApp with two networks", vmCreationInput{"Amsterdam", "k8s-2m-1c", testStorage}, []string{"ci-k8s-network", "C0025-AMS01-SVM-EFCI-CI-DATA-2654"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVCD(t)
			f.vAppDocumentNetworks, f.networkSectionNets = tc.networks, tc.networks

			if _, err := f.vdcManager().AddNewTkgVM(testVMName, fakeVAppName, fakeCatalogName, testTemplate,
				tc.in.placement, tc.in.sizing, tc.in.storage); err != nil {
				t.Fatalf("vendored AddNewTkgVM: %v", err)
			}
			_, vendored, unhandled := f.recorded()
			if len(unhandled) != 0 || len(vendored) != 1 {
				t.Fatalf("vendored path: unhandled requests %v, recompose requests %d", unhandled, len(vendored))
			}

			f.resetRecording()
			if _, err := addViaNewPath(t, f, tc.in); err != nil {
				t.Fatalf("new path: %v", err)
			}
			_, copied, unhandled := f.recorded()
			if len(unhandled) != 0 || len(copied) != 1 {
				t.Fatalf("new path: unhandled requests %v, recompose requests %d", unhandled, len(copied))
			}

			for _, body := range []string{vendored[0].body, copied[0].body} {
				if !adminPasswordRe.MatchString(body) || strings.Contains(body, "<AdminPassword></AdminPassword>") {
					t.Fatalf("expected a generated admin password in the request body:\n%s", body)
				}
			}
			if vendored[0].path != copied[0].path {
				t.Errorf("request path differs: vendored %q, new %q", vendored[0].path, copied[0].path)
			}
			if vendored[0].contentType != copied[0].contentType {
				t.Errorf("content type differs: vendored %q, new %q", vendored[0].contentType, copied[0].contentType)
			}
			if v, c := maskPassword(vendored[0].body), maskPassword(copied[0].body); v != c {
				t.Errorf("request body differs from the vendored one.\n--- vendored ---\n%s\n--- new ---\n%s", v, c)
			}
		})
	}
}

func TestNewAddPathNeverFetchesTheVAppDocument(t *testing.T) {
	f := newFakeVCD(t)
	vdcManager := f.vdcManager()
	vApp, err := vdcManager.Vdc.GetVAppByName(fakeVAppName, true)
	if err != nil {
		t.Fatalf("fetch vApp from fake: %v", err)
	}
	f.resetRecording()

	spec, err := resolveTkgVMCreationSpec(vdcManager, testVMName, fakeCatalogName, testTemplate, "Amsterdam", "k8s-2m-1c", testStorage)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	lookups, _, _ := f.recorded()
	for _, req := range lookups {
		if strings.Contains(req, "/api/vApp/") {
			t.Errorf("the lookups must not touch the vApp, but requested %q", req)
		}
	}

	f.resetRecording()
	if _, err := addTkgVMToVApp(vdcManager, vApp, spec); err != nil {
		t.Fatalf("add: %v", err)
	}
	underLock, _, _ := f.recorded()
	want := []string{"GET /api/vApp/vapp-1/networkConfigSection/", "POST /api/vApp/vapp-1/action/recomposeVApp"}
	if strings.Join(underLock, ",") != strings.Join(want, ",") {
		t.Errorf("under the lock expected exactly %v, got %v", want, underLock)
	}
}

func TestAddTkgVMToVAppUsesTheNetworksAsTheyAreNow(t *testing.T) {
	for _, tc := range []struct {
		name            string
		fetchedNetworks []string
		currentNetworks []string
		wantNetwork     string
	}{
		{"network order changed after the vApp was fetched", []string{"ci-k8s-network", "data"}, []string{"data", "ci-k8s-network"}, "data"},
		{"vApp fetched before its network was attached (new cluster)", nil, []string{"ci-k8s-network"}, "ci-k8s-network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVCD(t)
			f.vAppDocumentNetworks, f.networkSectionNets = tc.fetchedNetworks, tc.currentNetworks
			if _, err := addViaNewPath(t, f, vmCreationInput{"Amsterdam", "k8s-2m-1c", testStorage}); err != nil {
				t.Fatalf("new path: %v", err)
			}
			_, recompose, _ := f.recorded()
			if len(recompose) != 1 {
				t.Fatalf("expected one recompose request, got %d", len(recompose))
			}
			if want := fmt.Sprintf(`<NetworkConnection network=%q>`, tc.wantNetwork); !strings.Contains(recompose[0].body, want) {
				t.Errorf("expected %s in the request body:\n%s", want, recompose[0].body)
			}
		})
	}
}

func TestAddTkgVMToVAppFailsWhenVAppHasNoNetwork(t *testing.T) {
	f := newFakeVCD(t)
	f.vAppDocumentNetworks, f.networkSectionNets = nil, nil
	_, err := addViaNewPath(t, f, vmCreationInput{"Amsterdam", "k8s-2m-1c", testStorage})
	if err == nil || !strings.Contains(err.Error(), "no network") {
		t.Fatalf("expected a no-network error, got %v", err)
	}
	if _, recompose, _ := f.recorded(); len(recompose) != 0 {
		t.Fatalf("no recompose request may be sent when the vApp has no network, got %d", len(recompose))
	}
}

func TestAddTkgVMToVAppReportsBusyVAppAsContention(t *testing.T) {
	f := newFakeVCD(t)
	f.recomposeStatus = http.StatusBadRequest
	f.recomposeBody = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Error xmlns="http://www.vmware.com/vcloud/v1.5" message="[ 1-2-3 ] Unable to perform this action. Contact your cloud administrator. ` +
		`VAPP_DEPLOY(com.vmware.vcloud.entity.task:0e9f39f2-1065-4651-b3fd-f6d949b502c1)" majorErrorCode="400" minorErrorCode="BUSY_ENTITY"/>`

	_, err := addViaNewPath(t, f, vmCreationInput{"Amsterdam", "k8s-2m-1c", testStorage})
	if err == nil {
		t.Fatal("expected an error for a busy vApp")
	}
	if !isVAppContentionError(err) {
		t.Errorf("a busy vApp must be recognised as contention, got: %v", err)
	}
	if !strings.Contains(err.Error(), "error for adding TKG VM to vApp[dev-minimal]") {
		t.Errorf("error should keep the vendored wording, got: %v", err)
	}
}

func TestResolveTkgVMCreationSpecFailsLikeVendoredCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		in       vmCreationInput
	}{
		{"unknown template", "no-such-template", vmCreationInput{"Amsterdam", "k8s-2m-1c", testStorage}},
		{"unknown placement policy", testTemplate, vmCreationInput{"no-such-placement", "k8s-2m-1c", testStorage}},
		{"unknown sizing policy", testTemplate, vmCreationInput{"Amsterdam", "no-such-sizing", testStorage}},
		{"unknown storage profile", testTemplate, vmCreationInput{"Amsterdam", "k8s-2m-1c", "no-such-storage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeVCD(t)
			if _, err := f.vdcManager().AddNewTkgVM(testVMName, fakeVAppName, fakeCatalogName, tc.template,
				tc.in.placement, tc.in.sizing, tc.in.storage); err == nil {
				t.Fatalf("vendored AddNewTkgVM unexpectedly succeeded")
			}

			f.resetRecording()
			spec, err := resolveTkgVMCreationSpec(f.vdcManager(), testVMName, fakeCatalogName, tc.template,
				tc.in.placement, tc.in.sizing, tc.in.storage)
			if err == nil || spec != nil {
				t.Fatalf("expected an error and no spec, got spec=%v err=%v", spec, err)
			}
			if _, recompose, _ := f.recorded(); len(recompose) != 0 {
				t.Fatalf("no recompose request may be sent when a lookup fails, got %d", len(recompose))
			}
		})
	}
}

func TestResolveTkgVMCreationSpecSeesStorageProfilesAddedAfterTheFirstVDCFetch(t *testing.T) {
	f := newFakeVCD(t)
	vdcManager := f.vdcManager()
	f.storageProfiles = append(f.storageProfiles, "New Tier")

	spec, err := resolveTkgVMCreationSpec(vdcManager, testVMName, fakeCatalogName, testTemplate, "Amsterdam", "k8s-2m-1c", "New Tier")
	if err != nil {
		t.Fatalf("expected the refreshed VDC to contain the new storage profile, got: %v", err)
	}
	if spec.storageProfile == nil || spec.storageProfile.Name != "New Tier" {
		t.Fatalf("expected storage profile %q, got %+v", "New Tier", spec.storageProfile)
	}
}

func TestAddTkgVMToVAppRejectsMissingInputs(t *testing.T) {
	f := newFakeVCD(t)
	vdcManager := f.vdcManager()
	vApp, err := vdcManager.Vdc.GetVAppByName(fakeVAppName, true)
	if err != nil {
		t.Fatalf("fetch vApp from fake: %v", err)
	}
	spec := &tkgVMCreationSpec{vmName: testVMName}

	for _, tc := range []struct {
		name       string
		vdcManager *vcdsdk.VdcManager
		vApp       *govcd.VApp
		spec       *tkgVMCreationSpec
	}{
		{"nil vdc manager", nil, vApp, spec},
		{"vdc manager without client", &vcdsdk.VdcManager{}, vApp, spec},
		{"nil vApp", vdcManager, nil, spec},
		{"vApp without document", vdcManager, &govcd.VApp{}, spec},
		{"nil spec", vdcManager, vApp, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := addTkgVMToVApp(tc.vdcManager, tc.vApp, tc.spec); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	if _, err := resolveTkgVMCreationSpec(nil, testVMName, fakeCatalogName, testTemplate, "", "", ""); err == nil {
		t.Fatal("expected an error for a nil vdc manager")
	}
	if _, _, unhandled := f.recorded(); len(unhandled) != 0 {
		t.Fatalf("unexpected unhandled requests: %v", unhandled)
	}
}
