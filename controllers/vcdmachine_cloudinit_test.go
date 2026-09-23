/*
   Copyright 2021 VMware, Inc.
   SPDX-License-Identifier: Apache-2.0
*/

package controllers

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// sampleJinjaConfig stands in for the cloud-config CABPK renders for a KubeadmConfig: a plain
// runcmd list, extracted and re-embedded verbatim by MergeJinjaToCloudInitScript.
const sampleJinjaConfig = `
runcmd:
- apt-get -qq update > /var/log/apt-update.log.1 2>&1
- kubeadm join --config /run/kubeadm/kubeadm-join-config.yaml  && echo success > /run/cluster-api/bootstrap-success.complete
`

// TestCloudInitTemplateRendersValidYAMLAndBash renders cloud_init.tmpl the same way production
// does (MergeJinjaToCloudInitScript, whose own caller immediately yaml.Unmarshal's the result)
// and additionally checks the embedded customization script with `bash -n`. The template's
// output is not type-checked by the Go compiler -- a bad edit only shows up at bootstrap time,
// against every machine in every cluster -- so this exists to catch that class of mistake here
// instead.
func TestCloudInitTemplateRendersValidYAMLAndBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available on PATH; skipping syntax check")
	}

	cases := []struct {
		name  string
		input CloudInitScriptInput
	}{
		{name: "worker, no proxy", input: CloudInitScriptInput{MachineName: "test-worker"}},
		{name: "control plane, no proxy", input: CloudInitScriptInput{ControlPlane: true, MachineName: "test-cp"}},
		{
			name: "worker with proxy",
			input: CloudInitScriptInput{
				MachineName: "test-worker-proxy",
				HTTPProxy:   "http://proxy:3128",
				HTTPSProxy:  "http://proxy:3128",
				NoProxy:     "localhost",
			},
		},
		{
			name: "resized control plane with proxy",
			input: CloudInitScriptInput{
				ResizedControlPlane: true,
				MachineName:         "test-resized-cp-proxy",
				HTTPProxy:           "http://proxy:3128",
				HTTPSProxy:          "http://proxy:3128",
				NoProxy:             "localhost",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := MergeJinjaToCloudInitScript(tc.input, sampleJinjaConfig)
			if err != nil {
				t.Fatalf("MergeJinjaToCloudInitScript failed: %v", err)
			}

			// This is exactly what the real caller does with the result: if this fails,
			// so does every VM boot in production.
			var doc map[string]interface{}
			if err := yaml.Unmarshal(rendered, &doc); err != nil {
				t.Fatalf("rendered cloud-init is not valid YAML: %v\n--- rendered ---\n%s", err, rendered)
			}

			writeFiles, ok := doc["write_files"].([]interface{})
			if !ok {
				t.Fatalf("expected write_files to be a list, got %T", doc["write_files"])
			}

			script := findCustomizationScript(t, writeFiles)

			checkBashSyntax(t, script)

			for _, want := range []string{
				"guestinfo.post_customization_script_execution_status",
				"guestinfo.post_customization_script_execution_failure_reason",
				"guestinfo.post_customization_cloud_init_output",
				"/root/kubeadm.out",
				"/root/kubeadm.err",
				"PIPESTATUS",
				"kubeadm join",
			} {
				if !strings.Contains(script, want) {
					t.Errorf("rendered script is missing expected content %q", want)
				}
			}
		})
	}
}

// findCustomizationScript locates the control_plane.sh/node.sh entry among the rendered
// write_files and returns its content, failing the test if it cannot be found.
func findCustomizationScript(t *testing.T, writeFiles []interface{}) string {
	t.Helper()
	for _, wf := range writeFiles {
		m, ok := wf.(map[string]interface{})
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		if strings.Contains(content, "post_customization_script_execution_status") {
			return content
		}
	}
	t.Fatal("did not find the customization script among rendered write_files")
	return ""
}

// checkBashSyntax fails the test if script is not syntactically valid bash, without executing
// any of it.
func checkBashSyntax(t *testing.T, script string) {
	t.Helper()
	f, err := os.CreateTemp("", "cloudinit-*.sh")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("failed to write script to temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp file: %v", err)
	}

	out, err := exec.Command("bash", "-n", f.Name()).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n reported a syntax error: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}
