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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// assistedByPrefix is the attribution line every finalized PR carries, ahead
// of the gate commands that actually ran.
const assistedByPrefix = "Assisted-by: Meitheal; DeepSeek V4.1 Flash on a three-GB10 LLMKube ring " +
	"wrote the change, Qwen3.8 Flash-Next reviewed it; gates: "

// readyMarker is hidden in the ready comment so a rerun of Finalize (a
// restarted loop going round a finished item again) does not post it twice.
const readyMarker = "<!-- meitheal:ready -->"

// parkedLabel marks a draft PR whose issue is waiting on a human.
const parkedLabel = "meitheal:parked"

// gatesFor is every gate command Verify ran for the item, in order: the CPU
// gate's, then the GPU gate's for a gpu-lane item.
func (e *clusterEffects) gatesFor(item QueueItem) []string {
	var cpu meithealv1alpha1.GateProfile
	if e.tpl.Spec.GateProfile != nil {
		cpu = *e.tpl.Spec.GateProfile
	}
	cmds := gateCommands(cpu)
	if item.Lane == "gpu" && e.tpl.GPUGate != nil {
		cmds = append(cmds, gateCommands(*e.tpl.GPUGate)...)
	}
	return cmds
}

// coderPRBody is the PR description the newest coder task authored
// (extra.modelExtra.prBody, the slot the executor itself reads), or "".
func (e *clusterEffects) coderPRBody(ctx context.Context, workload string) string {
	tasks, err := e.workloadTasks(ctx, workload)
	if err != nil {
		return ""
	}
	body := ""
	var newest *meithealv1alpha1.AgenticTask
	for i := range tasks {
		t := &tasks[i]
		if t.Spec.Kind != meithealv1alpha1.AgenticTaskKindIssueFix {
			continue
		}
		r, ok := parseTaskResult(t)
		if !ok {
			continue
		}
		pb, _ := r.Extra.ModelExtra["prBody"].(string)
		if strings.TrimSpace(pb) == "" {
			continue
		}
		// Name order breaks ties, so a revision (code-N-r1) beats code-N.
		if newest == nil || !t.CreationTimestamp.Before(&newest.CreationTimestamp) {
			newest, body = t, strings.TrimSpace(pb)
		}
	}
	return body
}

// prBody lays the PR description out in Summary and Testing sections (Dlúth
// has no PR template of its own) and ends with the Assisted-by line.
func (e *clusterEffects) prBody(ctx context.Context, item QueueItem, workload string) string {
	summary := e.coderPRBody(ctx, workload)
	if summary == "" {
		summary = fmt.Sprintf("Automated change for #%d.", item.Issue)
	}
	gates := e.gatesFor(item)
	quoted := make([]string, len(gates))
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	b.WriteString(summary)
	fmt.Fprintf(&b, "\n\nCloses #%d.\n\n## Testing\n\n", item.Issue)
	b.WriteString("Meitheal's independent gates passed on the pushed branch:\n\n")
	for i, g := range gates {
		quoted[i] = "`" + g + "`"
		b.WriteString("- " + quoted[i] + "\n")
	}
	b.WriteString("\n" + assistedByPrefix + strings.Join(quoted, ", ") + "\n")
	return b.String()
}

// Finalize rewrites the run's PR description and tells the issue the draft
// is ready. The review stage opened the PR; a clean verify with no PR to show
// for it is an error, not a silent success.
func (e *clusterEffects) Finalize(ctx context.Context, item QueueItem, workload, branch string) error {
	repo := e.repoFor(item)
	prURL, err := e.forge.PRForBranch(ctx, repo, branch)
	if err != nil {
		return fmt.Errorf("finalize issue %d: find PR for %s: %w", item.Issue, branch, err)
	}
	if prURL == "" {
		return fmt.Errorf("finalize issue %d: no open PR for %s", item.Issue, branch)
	}
	if err := e.forge.UpdatePRBody(ctx, repo, branch, e.prBody(ctx, item, workload)); err != nil {
		return fmt.Errorf("finalize issue %d: update PR body: %w", item.Issue, err)
	}
	ready := "Draft PR ready for review: " + prURL
	comments, err := e.forge.CommentsSince(ctx, repo, item.Issue, time.Time{})
	if err != nil {
		return fmt.Errorf("finalize issue %d: read comments: %w", item.Issue, err)
	}
	for _, c := range comments {
		// The same PR already announced. A different PR (after a drop and a
		// requeue) gets its own comment.
		if strings.Contains(c.Body, readyMarker) && strings.Contains(c.Body, prURL) {
			return nil
		}
	}
	if err := e.forge.CommentIssue(ctx, repo, item.Issue, ready+"\n\n"+readyMarker); err != nil {
		return fmt.Errorf("finalize issue %d: comment: %w", item.Issue, err)
	}
	return nil
}

// publicReason flattens a park reason to one line without a trailing period.
func publicReason(r string) string {
	r = strings.TrimRight(strings.Join(strings.Fields(r), " "), ".")
	if r == "" {
		return "a decision is needed"
	}
	return r
}

// Notify posts the public one-line park comment, labels the draft PR when
// there is one, and POSTs the full decision to NotifyURL when set.
//
// The public comment is built from the reason and the PR link ONLY. Evidence
// (gate logs, concern text) and options never reach it: a log can carry a
// hostname or a token, and the answer options are for the one human allowed
// to answer. Every step runs even when an earlier one fails; the errors come
// back joined for the driver to log.
func (e *clusterEffects) Notify(ctx context.Context, item QueueItem, d Decision) error {
	repo := e.repoFor(item)
	var errs []error
	prURL, branch := "", ""
	if d.Workload != "" {
		branch = taskBranch(d.Workload, d.Issue)
		u, err := e.forge.PRForBranch(ctx, repo, branch)
		if err != nil {
			errs = append(errs, fmt.Errorf("notify issue %d: find PR: %w", d.Issue, err))
		} else {
			prURL = u
		}
	}
	comment := "Meitheal parked this issue: " + publicReason(d.Reason) + "."
	if prURL != "" {
		comment += " Draft PR: " + prURL
	}
	if err := e.forge.CommentIssue(ctx, repo, d.Issue, comment); err != nil {
		errs = append(errs, fmt.Errorf("notify issue %d: comment: %w", d.Issue, err))
	}
	if prURL != "" {
		if err := e.forge.LabelPR(ctx, repo, branch, parkedLabel); err != nil {
			errs = append(errs, fmt.Errorf("notify issue %d: label PR: %w", d.Issue, err))
		}
	}
	if e.o.NotifyURL != "" {
		if err := e.postNotify(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("notify issue %d: webhook: %w", d.Issue, err))
		}
	}
	return errors.Join(errs...)
}

// postNotify sends the decision to the private webhook. Errors never carry
// the URL, which may hold a token of its own.
func (e *clusterEffects) postNotify(ctx context.Context, d Decision) error {
	payload, err := json.Marshal(struct {
		Issue    int32             `json:"issue"`
		Kind     string            `json:"kind"`
		Reason   string            `json:"reason"`
		Options  []string          `json:"options"`
		Evidence map[string]string `json:"evidence"`
	}{d.Issue, d.Kind, d.Reason, d.Options, d.Evidence})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.o.NotifyURL, bytes.NewReader(payload))
	if err != nil {
		return errors.New("bad notify URL")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("POST failed: %w", ue.Err)
		}
		return errors.New("POST failed")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("answered HTTP %d", resp.StatusCode)
	}
	return nil
}
