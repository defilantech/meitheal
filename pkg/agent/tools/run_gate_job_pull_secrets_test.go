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

package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestParseGateImagePullSecrets(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr string
	}{
		{name: "empty is none", in: "", want: nil},
		{name: "separators only is none", in: " , ,", want: nil},
		{name: "one", in: "registry-cred", want: []string{"registry-cred"}},
		{name: "two trimmed", in: " registry-cred , ghcr.pull ", want: []string{"registry-cred", "ghcr.pull"}},
		{name: "duplicate kept once", in: "a,b,a", want: []string{"a", "b"}},
		{name: "uppercase refused", in: "Registry-Cred", wantErr: `"Registry-Cred"`},
		{name: "underscore refused", in: "ok,bad_name", wantErr: `"bad_name"`},
		{name: "too long refused", in: strings.Repeat("a", 254), wantErr: "253"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGateImagePullSecrets(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseGateImagePullSecrets(%q) err = %v, want one containing %s", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGateImagePullSecrets(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseGateImagePullSecrets(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The gate pod pulls gateProfile.image, which may sit in a private registry:
// the configured pull secrets go on the pod spec, in order (#46).
func TestRenderGateJob_ImagePullSecrets(t *testing.T) {
	in := placementTestInput()
	in.ImagePullSecrets = []string{"registry-cred", "ghcr-pull"}
	job, err := renderGateJob(in)
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	want := []corev1.LocalObjectReference{{Name: "registry-cred"}, {Name: "ghcr-pull"}}
	if got := job.Spec.Template.Spec.ImagePullSecrets; !reflect.DeepEqual(got, want) {
		t.Errorf("imagePullSecrets = %v, want %v", got, want)
	}
}

func TestRenderGateJob_NoImagePullSecretsByDefault(t *testing.T) {
	job, err := renderGateJob(placementTestInput())
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	if got := job.Spec.Template.Spec.ImagePullSecrets; got != nil {
		t.Errorf("imagePullSecrets = %v, want none when none are configured", got)
	}
}

// A config built in code, not from the flag, is checked too: a bad name is
// refused at render rather than sent to the apiserver.
func TestRenderGateJob_InvalidImagePullSecretRefused(t *testing.T) {
	in := placementTestInput()
	in.ImagePullSecrets = []string{"Not_A_Name"}
	if _, err := renderGateJob(in); err == nil || !strings.Contains(err.Error(), "Not_A_Name") {
		t.Fatalf("renderGateJob err = %v, want the bad secret name refused", err)
	}
}

// End to end through Execute: the tool's static config reaches the Job it
// creates, which is the path both the agent and `meitheal run` take.
func TestRunGateJob_ConfigImagePullSecretsReachTheJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	key := types.NamespacedName{Namespace: "meitheal-system", Name: "meitheal-gate-pull"}
	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunGateJobTool{Client: c, Cfg: RunGateJobToolConfig{
		ImagePullSecrets: []string{"registry-cred"},
		NameFn:           pinName(key.Name),
		PollInterval:     5 * time.Millisecond,
		PollTimeout:      2 * time.Second,
	}}
	res, err := tool.Execute(ctx, argsJSON(t, "defilantech/LLMKube", "meitheal/issue-46"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGatePass {
		t.Fatalf("Verdict = %s, want %s (%s)", res.Verdict, VerdictGatePass, res.Summary)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("get Job: %v", err)
	}
	want := []corev1.LocalObjectReference{{Name: "registry-cred"}}
	if got := job.Spec.Template.Spec.ImagePullSecrets; !reflect.DeepEqual(got, want) {
		t.Errorf("imagePullSecrets = %v, want %v", got, want)
	}
}

func TestBuildAll_ThreadsGateImagePullSecrets(t *testing.T) {
	for _, tool := range BuildAll(ToolDeps{GateImagePullSecrets: []string{"registry-cred"}}) {
		if gt, ok := tool.(*RunGateJobTool); ok {
			if want := []string{"registry-cred"}; !reflect.DeepEqual(gt.Cfg.ImagePullSecrets, want) {
				t.Errorf("RunGateJobTool.Cfg.ImagePullSecrets = %v, want %v", gt.Cfg.ImagePullSecrets, want)
			}
			return
		}
	}
	t.Fatal("BuildAll did not construct a *RunGateJobTool")
}
