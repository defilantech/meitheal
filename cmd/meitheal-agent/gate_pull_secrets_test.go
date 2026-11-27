/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

func TestParseGateJobPolicy(t *testing.T) {
	p, err := parseGateJobPolicy("/srv/models", "batch", "registry-cred, ghcr-pull")
	if err != nil {
		t.Fatalf("parseGateJobPolicy: %v", err)
	}
	want := gateJobPolicy{
		allowedHostPaths:       []string{"/srv/models"},
		allowedPriorityClasses: []string{"batch"},
		imagePullSecrets:       []string{"registry-cred", "ghcr-pull"},
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("parseGateJobPolicy = %+v, want %+v", p, want)
	}

	if p, err := parseGateJobPolicy("", "", ""); err != nil || !reflect.DeepEqual(p, gateJobPolicy{}) {
		t.Errorf("empty flags = %+v, %v; want the zero policy", p, err)
	}

	_, err = parseGateJobPolicy("", "", "Registry_Cred")
	if err == nil || !strings.Contains(err.Error(), "--gate-image-pull-secrets") ||
		!strings.Contains(err.Error(), "Registry_Cred") {
		t.Errorf("bad secret name: err = %v, want it to name the flag and the value", err)
	}
}

// The pull secrets the agent was started with reach the gate Job the
// run_gate_job tool creates (#46). The poll is cut short by the context; the
// Job is created before polling starts.
func TestRegistryFactoryThreadsGateImagePullSecrets(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	factory := makeRegistryFactory(c, k8sfake.NewClientset(), "meitheal", "", forgeSeams{},
		gateJobPolicy{imagePullSecrets: []string{"registry-cred"}})
	ag := &meithealv1alpha1.Agent{Spec: meithealv1alpha1.AgentSpec{Tools: []string{"run_gate_job"}}}
	reg, err := factory(context.Background(), t.TempDir(), ag, false)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = reg.Dispatch(ctx, "run_gate_job", json.RawMessage(`{"repo":"o/r","branch":"b"}`))

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("meitheal")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("%d gate Jobs created, want 1", len(jobs.Items))
	}
	want := []corev1.LocalObjectReference{{Name: "registry-cred"}}
	if got := jobs.Items[0].Spec.Template.Spec.ImagePullSecrets; !reflect.DeepEqual(got, want) {
		t.Errorf("gate Job imagePullSecrets = %v, want %v", got, want)
	}
}

// The envtest gate renders from the same template and pulls with the same
// secrets.
func TestEnvtestJobRunnerCarriesImagePullSecrets(t *testing.T) {
	r, ok := makeEnvtestJobRunner(nil, "meitheal", []string{"registry-cred"}, nil).(*envtestJobRunnerImpl)
	if !ok {
		t.Fatal("makeEnvtestJobRunner did not return an *envtestJobRunnerImpl")
	}
	if want := []string{"registry-cred"}; !reflect.DeepEqual(r.tool.Cfg.ImagePullSecrets, want) {
		t.Errorf("envtest gate ImagePullSecrets = %v, want %v", r.tool.Cfg.ImagePullSecrets, want)
	}
}

// --gate-image-pull-secrets is defined on both the daemon and run-task flag
// sets, and a malformed name stops either at startup with exit code 2. Each
// case re-executes this test binary; KUBECONFIG points at an empty file, so a
// child whose flags parse fails on cluster config (exit 1) and reaches no
// cluster.
func TestGateImagePullSecretsFlag(t *testing.T) {
	if raw := os.Getenv("MEITHEAL_TEST_GATE_FLAG_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(3)
		}
		os.Args = append([]string{"meitheal-agent"}, args...)
		main()
		os.Exit(0)
	}

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		args      []string
		wantExit2 bool
	}{
		{"daemon accepts valid names", []string{"--gate-image-pull-secrets=registry-cred,ghcr-pull"}, false},
		{"daemon refuses a bad name", []string{"--gate-image-pull-secrets=Bad_Name"}, true},
		{"run-task accepts valid names",
			[]string{"run-task", "--task=t", "--gate-image-pull-secrets=registry-cred"}, false},
		{"run-task refuses a bad name",
			[]string{"run-task", "--task=t", "--gate-image-pull-secrets=Bad_Name"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.args)
			//nolint:gosec // G702: re-executes this test binary with a fixed -test.run; the args are fixed above.
			child := exec.Command(os.Args[0], "-test.run=^TestGateImagePullSecretsFlag$")
			child.Env = append(os.Environ(), "MEITHEAL_TEST_GATE_FLAG_ARGS="+string(raw), "KUBECONFIG="+kubeconfig,
				"KUBERNETES_SERVICE_HOST=", "KUBERNETES_SERVICE_PORT=")
			out, err := child.CombinedOutput()
			if strings.Contains(string(out), "flag provided but not defined") {
				t.Fatalf("args %q: flag not defined:\n%s", tc.args, out)
			}
			var exit *exec.ExitError
			gotExit2 := errors.As(err, &exit) && exit.ExitCode() == 2
			if gotExit2 != tc.wantExit2 {
				t.Errorf("args %q: exit 2 = %v, want %v (err %v):\n%s", tc.args, gotExit2, tc.wantExit2, err, out)
			}
			if tc.wantExit2 && !strings.Contains(string(out), "Bad_Name") {
				t.Errorf("args %q: output does not name the bad secret:\n%s", tc.args, out)
			}
			if !tc.wantExit2 && !strings.Contains(string(out), "kubeconfig") {
				t.Errorf("args %q: want the child to get past flag checks to kubeconfig loading:\n%s", tc.args, out)
			}
		})
	}
}
