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

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent/tools"
)

const gateJobName = "meitheal-gate-test"

func gateClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(effScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&batchv1.Job{}).
		Build()
}

// finishJob waits for the gate Job to appear, records it, and marks it
// succeeded or failed so the tool's poll returns.
func finishJob(ctx context.Context, c client.Client, ok bool, seen chan<- *batchv1.Job) {
	key := types.NamespacedName{Namespace: effNS, Name: gateJobName}
	for ctx.Err() == nil {
		var job batchv1.Job
		if err := c.Get(ctx, key, &job); err == nil {
			seen <- job.DeepCopy()
			if ok {
				job.Status.Succeeded = 1
			} else {
				job.Status.Failed = 1
			}
			_ = c.Status().Update(ctx, &job)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func gateCfg(logTail string) tools.RunGateJobToolConfig {
	return tools.RunGateJobToolConfig{
		Namespace:              effNS,
		PollInterval:           time.Millisecond,
		PollTimeout:            5 * time.Second,
		NameFn:                 func(string) string { return gateJobName },
		LogTailFn:              func(context.Context, string, string) string { return logTail },
		AllowedHostPaths:       []string{"/var/lib/dluth"},
		AllowedPriorityClasses: []string{"gate-gpu"},
	}
}

// The production runner drives run_gate_job on its generic path with the
// profile's commands in format, lint, build, test order, and gets the
// placement to the Job through the context only: run_gate_job rejects a
// placement in its arguments, so a placed Job that renders at all proves the
// channel.
func TestGateRunner_GenericCommandsInOrderWithPlacementFromContext(t *testing.T) {
	c := gateClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen := make(chan *batchv1.Job, 1)
	go finishJob(ctx, c, true, seen)

	profile := meithealv1alpha1.GateProfile{
		Language: meithealv1alpha1.GateLanguageGeneric,
		Image:    "dluth-cuda:1",
		Commands: meithealv1alpha1.GateCommands{
			Test:   "cargo test -p dluth-cuda && bash tools/mutants.sh all",
			Format: "cargo fmt --all -- --check",
			Build:  "cargo build --release",
			Lint:   "cargo clippy --all-targets -- -D warnings",
		},
		Placement: &meithealv1alpha1.GatePlacement{
			NodeSelector:      map[string]string{"kubernetes.io/hostname": gpuNode},
			PriorityClassName: "gate-gpu",
			Volumes: []corev1.Volume{{Name: "models", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/dluth/models"}}}},
			VolumeMounts: []corev1.VolumeMount{{Name: "models", MountPath: "/models", ReadOnly: true}},
		},
	}
	clean, log, err := NewGateRunner(c, gateCfg("all four passed")).Run(ctx, effRepo, "meitheal/wl-42/issue-42", profile)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !clean || log != "all four passed" {
		t.Fatalf("Run = %v, %q; want clean with the log tail", clean, log)
	}
	job := <-seen
	pod := job.Spec.Template.Spec
	if got := pod.NodeSelector["kubernetes.io/hostname"]; got != gpuNode {
		t.Errorf("nodeSelector = %v, want the profile's placement", pod.NodeSelector)
	}
	if pod.PriorityClassName != "gate-gpu" {
		t.Errorf("priorityClassName = %q, want the allowed class", pod.PriorityClassName)
	}
	if pod.Containers[0].Image != "dluth-cuda:1" {
		t.Errorf("image = %q", pod.Containers[0].Image)
	}
	script := strings.Join(pod.Containers[0].Args, "\n")
	order := []string{
		"cargo fmt --all -- --check",
		"cargo clippy --all-targets -- -D warnings",
		"cargo build --release",
		"cargo test -p dluth-cuda && bash tools/mutants.sh all",
	}
	last := -1
	for _, cmd := range order {
		i := strings.Index(script, cmd)
		if i < 0 {
			t.Fatalf("gate script missing %q:\n%s", cmd, script)
		}
		if i < last {
			t.Fatalf("%q out of order in:\n%s", cmd, script)
		}
		last = i
	}
	if strings.Contains(script, "make ") {
		t.Errorf("generic gate fell back to make targets:\n%s", script)
	}
}

func TestGateRunner_NoPlacementLeavesJobUnplaced(t *testing.T) {
	c := gateClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen := make(chan *batchv1.Job, 1)
	go finishJob(ctx, c, true, seen)
	profile := meithealv1alpha1.GateProfile{
		Language: meithealv1alpha1.GateLanguageGeneric,
		Commands: meithealv1alpha1.GateCommands{Test: "cargo test"},
	}
	if _, _, err := NewGateRunner(c, gateCfg("")).Run(ctx, effRepo, "b", profile); err != nil {
		t.Fatal(err)
	}
	if job := <-seen; len(job.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatalf("nodeSelector = %v, want none", job.Spec.Template.Spec.NodeSelector)
	}
}

func TestGateRunner_FailIsDirtyWithLog(t *testing.T) {
	c := gateClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen := make(chan *batchv1.Job, 1)
	go finishJob(ctx, c, false, seen)
	profile := meithealv1alpha1.GateProfile{
		Language: meithealv1alpha1.GateLanguageGeneric,
		Commands: meithealv1alpha1.GateCommands{Test: "cargo test"},
	}
	clean, log, err := NewGateRunner(c, gateCfg("test foo ... FAILED")).Run(ctx, effRepo, "b", profile)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if clean || log != "test foo ... FAILED" {
		t.Fatalf("Run = %v, %q; want dirty with the log tail", clean, log)
	}
}

// A placement outside the allowlists never renders a Job: run_gate_job says
// GATE-ERROR, which is the gate not running, so it is an error and never a
// verdict on the branch.
func TestGateRunner_GateErrorIsAnError(t *testing.T) {
	c := gateClient(t)
	profile := meithealv1alpha1.GateProfile{
		Language:  meithealv1alpha1.GateLanguageGeneric,
		Commands:  meithealv1alpha1.GateCommands{Test: "cargo test"},
		Placement: &meithealv1alpha1.GatePlacement{PriorityClassName: "system-node-critical"},
	}
	clean, _, err := NewGateRunner(c, gateCfg("")).Run(context.Background(), effRepo, "b", profile)
	if err == nil || clean {
		t.Fatalf("Run = %v, %v; want an error for a disallowed placement", clean, err)
	}
}

func TestGateRunner_NoCommandsIsAnError(t *testing.T) {
	c := gateClient(t)
	profile := meithealv1alpha1.GateProfile{Language: meithealv1alpha1.GateLanguageGeneric}
	if _, _, err := NewGateRunner(c, gateCfg("")).Run(context.Background(), effRepo, "b", profile); err == nil {
		t.Fatal("want an error for a profile with no commands")
	}
}

func TestGPUBusy_DetectsInferencePodsOnTheNode(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		busy bool
	}{
		{"inference service pod running on the node",
			inferencePod("a", gpuNode, map[string]string{LLMKubeInferenceServiceLabel: "qwen"}, corev1.PodRunning), true},
		{"multi-node worker (no service label) on the node",
			inferencePod("b", gpuNode, map[string]string{LLMKubeMultiNodeGroupLabel: "ring"}, corev1.PodRunning), true},
		{"pending inference pod bound to the node",
			inferencePod("c", gpuNode, map[string]string{LLMKubeInferenceServiceLabel: "qwen"}, corev1.PodPending), true},
		{"inference pod on another node",
			inferencePod("d", "cpu-box", map[string]string{LLMKubeInferenceServiceLabel: "qwen"}, corev1.PodRunning), false},
		{"finished inference pod on the node",
			inferencePod("e", gpuNode, map[string]string{LLMKubeInferenceServiceLabel: "qwen"}, corev1.PodSucceeded), false},
		{"unrelated pod on the node",
			inferencePod("f", gpuNode, map[string]string{"app": "node-exporter"}, corev1.PodRunning), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := gateClient(t, tc.pod)
			err := gpuBusy(context.Background(), c, gpuNode)
			if tc.busy && !errors.Is(err, ErrGPUBusy) {
				t.Fatalf("err = %v, want ErrGPUBusy", err)
			}
			if !tc.busy && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}
