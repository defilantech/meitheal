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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent"
	"github.com/defilantech/meitheal/pkg/agent/tools"
)

// GateRunner runs one independent gate over a pushed branch. clean is the
// gate's verdict and log its output tail; err means the gate did not run (or
// did not finish), which is never a verdict on the branch.
type GateRunner interface {
	Run(ctx context.Context, repo, branch string, profile meithealv1alpha1.GateProfile) (clean bool, log string, err error)
}

// ErrGPUBusy reports that the GPU gate's node is serving a model, so a GPU
// gate there would fight the inference ring for the device.
var ErrGPUBusy = errors.New("gpu node is serving a model")

// The labels LLMKube's InferenceService controller puts on the pods it runs.
// A serving pod carries inference.llmkube.dev/service=<isvc> (set in
// internal/controller/deployment_builder.go); a multi-node worker drops that
// label so the Service does not select it and keeps
// inference.llmkube.dev/multinode-group instead (internal/controller/
// multinode.go, LabelMultiNodeGroup). A ring is exactly the multi-node case,
// so both count.
const (
	LLMKubeInferenceServiceLabel = "inference.llmkube.dev/service"
	LLMKubeMultiNodeGroupLabel   = "inference.llmkube.dev/multinode-group"
)

// gpuBusy returns ErrGPUBusy when an LLMKube inference pod that has not
// finished is bound to node. A Pending pod already bound there counts: it is
// about to load a model onto the same device.
func gpuBusy(ctx context.Context, c client.Client, node string) error {
	for _, label := range []string{LLMKubeInferenceServiceLabel, LLMKubeMultiNodeGroupLabel} {
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.HasLabels{label}); err != nil {
			return fmt.Errorf("list inference pods for gpu node %s: %w", node, err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Spec.NodeName != node {
				continue
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			return ErrGPUBusy
		}
	}
	return nil
}

// clusterGateRunner is the production GateRunner: run_gate_job on its
// generic path, the same tool the deterministic verify Agent uses, so the
// run loop's independent gate renders the same Job a Workload's own verify
// step would.
type clusterGateRunner struct {
	tool *tools.RunGateJobTool
}

// NewGateRunner returns a GateRunner that submits gate Jobs with c. cfg is
// the tool's static configuration; its AllowedHostPaths and
// AllowedPriorityClasses bound what a profile's placement may ask for, and
// are what the run command fills from its flags.
func NewGateRunner(c client.Client, cfg tools.RunGateJobToolConfig) GateRunner {
	return &clusterGateRunner{tool: &tools.RunGateJobTool{Client: c, Cfg: cfg}}
}

// gateCommands is the profile's resolved commands in the order the gate runs
// them: format, lint, build, test. Empty ones are skipped.
func gateCommands(p meithealv1alpha1.GateProfile) []string {
	r := p.Resolve()
	var out []string
	for _, c := range []string{r.Format, r.Lint, r.Build, r.Test} {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// gateTaskName turns a branch into the DNS-safe name run_gate_job puts into
// the Job name and its labels.
func gateTaskName(branch string) string {
	s := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(branch), "-"), "-")
	if len(s) > 30 {
		s = strings.Trim(s[:30], "-")
	}
	if s == "" {
		s = "run"
	}
	return s
}

func (r *clusterGateRunner) Run(
	ctx context.Context, repo, branch string, profile meithealv1alpha1.GateProfile,
) (bool, string, error) {
	cmds := gateCommands(profile)
	if len(cmds) == 0 {
		return false, "", fmt.Errorf("gate for %s: the profile resolves to no commands", branch)
	}
	ns := r.tool.Cfg.Namespace
	// No "placement" key, ever: run_gate_job refuses one in its arguments.
	// The profile's placement travels in the context, the trusted channel.
	args, err := json.Marshal(map[string]any{
		"repo":     repo,
		"branch":   branch,
		"image":    profile.Resolve().Image,
		"generic":  true,
		"commands": cmds,
		"taskRef":  map[string]string{"namespace": ns, "name": gateTaskName(branch)},
	})
	if err != nil {
		return false, "", err
	}
	res, err := r.tool.Execute(agent.WithGatePlacement(ctx, profile.Placement), args)
	if err != nil {
		return false, "", fmt.Errorf("gate for %s: %w", branch, err)
	}
	log, _ := res.Extra["logTail"].(string)
	switch res.Verdict {
	case tools.VerdictGatePass:
		return true, log, nil
	case tools.VerdictGateFail:
		return false, log, nil
	}
	// GATE-ERROR: the gate never judged the branch. The summary names why;
	// the log tail is left out of an error that ends up in the loop's log.
	return false, "", fmt.Errorf("gate for %s did not run to a verdict (%s): %s", branch, res.Verdict, res.Summary)
}
