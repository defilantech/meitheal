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

package controller

import (
	"testing"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// TestEffectiveGateProfile pins the step-over-workload resolution the
// reconciler applies to every rendered AgenticTask: a step's own profile
// wins, an unset step falls back to the Workload default, and both unset
// leaves nil (the "go" preset, i.e. pre-feature behavior).
func TestEffectiveGateProfile(t *testing.T) {
	node := &meithealv1alpha1.GateProfile{Language: meithealv1alpha1.GateLanguageNode}
	python := &meithealv1alpha1.GateProfile{Language: meithealv1alpha1.GateLanguagePython}

	cases := []struct {
		name     string
		step     *meithealv1alpha1.GateProfile
		workload *meithealv1alpha1.GateProfile
		want     *meithealv1alpha1.GateProfile
	}{
		{"both unset -> nil (go preset)", nil, nil, nil},
		{"workload default applies when step unset", nil, node, node},
		{"step profile wins over workload default", python, node, python},
		{"step profile applies when workload unset", python, nil, python},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &meithealv1alpha1.Workload{
				Spec: meithealv1alpha1.WorkloadSpec{GateProfile: tc.workload},
			}
			step := meithealv1alpha1.PipelineStep{GateProfile: tc.step}
			if got := effectiveGateProfile(step, w); got != tc.want {
				t.Errorf("effectiveGateProfile() = %v, want %v", got, tc.want)
			}
		})
	}
}
