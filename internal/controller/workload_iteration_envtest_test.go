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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent/reviewer"
)

// Reviewer NO-GO fix iteration (#946): instead of terminally failing
// the Workload, the reconciler re-dispatches the coder on the same
// branch with the review findings in payload.prompt, chains a fresh
// verify + review round behind it, and only fails once
// spec.maxReviewIterations is exhausted.
var _ = Describe("WorkloadReconciler review fix iteration (#946)", func() {
	var reconciler *WorkloadReconciler
	var recorder *events.FakeRecorder

	BeforeEach(func() {
		recorder = &events.FakeRecorder{Events: make(chan string, 16)}
		reconciler = &WorkloadReconciler{
			Client:              k8sClient,
			Scheme:              k8sClient.Scheme(),
			Recorder:            recorder,
			AllowCloudProviders: true,
		}
	})

	// markTerminal drives a child task to a terminal phase + verdict,
	// optionally attaching a structured result payload.
	markTerminal := func(name string, verdict meithealv1alpha1.AgenticTaskVerdict, resultJSON string) {
		var task meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &task)).To(Succeed())
		patch := client.MergeFrom(task.DeepCopy())
		task.Status.Phase = meithealv1alpha1.AgenticTaskPhaseSucceeded
		task.Status.Verdict = verdict
		if resultJSON != "" {
			task.Status.Result = &runtime.RawExtension{Raw: []byte(resultJSON)}
		}
		Expect(k8sClient.Status().Patch(ctx, &task, patch)).To(Succeed())
	}

	reconcile := func(wl *meithealv1alpha1.Workload) {
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})
		Expect(err).NotTo(HaveOccurred())
	}

	noGoResult := `{"schemaVersion":"meitheal.v1","kind":"review","verdict":"NO-GO",` +
		`"summary":"scope creep beyond the issue ask","extra":{"outcome":"MODEL-DECIDED","modelExtra":{"findings":` +
		`[{"severity":"blocker","area":"scope","message":"reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30",` +
		`"file":"config/auth.py","line":12,"suggestion":"revert the unrelated change"}]}}}`

	// The result an issueAsk demotion really produces: the reviewer's own
	// APPROVE summary, no findings, and the rails' markers nested under
	// extra.modelExtra where modelDecidedResult puts them. Built through
	// reviewResultRaw (workload_iteration_test.go) so the shape comes from
	// the real agent.Result rather than a hand-written string (#1641).
	inertDemotedResult := string(reviewResultRaw(
		meithealv1alpha1.AgenticTaskVerdictNoGo,
		"APPROVE: the change is minimal and matches the issue",
		map[string]any{
			"issueAskVerified": false,
			"verdictDemoted":   true,
			"verdictDemotedBy": reviewer.RailIssueAsk,
			"verdictClaimed":   string(meithealv1alpha1.AgenticTaskVerdictGo),
			"demotionReason":   demotionReasonIssueAsk,
			"findings":         []any{},
		}).Raw)

	It("re-dispatches the coder with the review feedback on NO-GO and completes when the retry converges", func() {
		wl := newWorkload("iterate-happy", meithealv1alpha1.WorkloadSpec{
			Intent:           "fix iteration happy path",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{750},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
			// maxReviewIterations unset: defaults to 1 iteration.
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		// Plan the base round, then drive it to a reviewer NO-GO.
		reconcile(wl)
		markTerminal("iterate-happy-code-750", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-happy-verify-750", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-happy-review-750-0", meithealv1alpha1.AgenticTaskVerdictNoGo, noGoResult)

		// The rollup pass must append the r1 iteration instead of failing.
		reconcile(wl)

		var code meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-happy-code-750-r1"}, &code)).To(Succeed())
		Expect(code.Spec.Kind).To(Equal(meithealv1alpha1.AgenticTaskKindIssueFix))
		Expect(code.Spec.AgentRef.Name).To(Equal("coder"),
			"no revisionCoderAgentRef: iteration falls back to the issue-fix coder Agent")
		Expect(code.Spec.Payload.Issue).To(Equal(int32(750)))
		Expect(code.Spec.Payload.Branch).To(Equal("meitheal/iterate-happy/issue-750"),
			"the retry amends the SAME branch, not a fresh one")
		Expect(code.Spec.Payload.AllowOverwrite).To(BeTrue(),
			"amending its own prior attempt needs the force-with-lease push")
		Expect(code.Spec.Payload.ReviseFromBranch).To(Equal("meitheal/iterate-happy/issue-750"),
			"the executor must restore the prior attempt from the push remote (#951)")
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("NO-GO"))
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("Do not rebuild the fix from scratch"),
			"the prompt must direct a delta on the restored attempt")
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("scope creep beyond the issue ask"))
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30"))
		Expect(code.Spec.DependsOn).To(BeEmpty())

		// No revisionCoderAgentRef: the fallback to the issue-fix coder
		// profile is the documented #951 failure mode, so it must warn.
		Expect(recorder.Events).To(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring("RevisionUnderIssueFixProfile"),
			ContainSubstring("revisionCoderAgentRef"),
		)))

		var verify meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-happy-verify-750-r1"}, &verify)).To(Succeed())
		Expect(verify.Spec.DependsOn).To(ConsistOf("iterate-happy-code-750-r1"))

		var review meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-happy-review-750-0-r1"}, &review)).To(Succeed())
		Expect(review.Spec.DependsOn).To(ConsistOf("iterate-happy-verify-750-r1"))
		Expect(review.Spec.Payload.OpenPullRequest).To(BeTrue(),
			"iteration review steps must carry the base-round openPullRequest stamp (#937)")

		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseDispatched),
			"workload must stay in flight while the fix iteration runs")
		Expect(fresh.Status.Tasks).To(HaveLen(6))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(1)))
		cond := findCondition(fresh.Status.Conditions, conditionTypeReviewIterationTriggered)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring("750"))

		// A further reconcile must NOT double-emit (idempotency by step
		// name existence, like escalation).
		reconcile(wl)
		var tasks meithealv1alpha1.AgenticTaskList
		Expect(k8sClient.List(ctx, &tasks, client.InNamespace("default"),
			client.MatchingLabels{labelWorkload: "iterate-happy"})).To(Succeed())
		Expect(tasks.Items).To(HaveLen(6))

		// Converge the retry: the superseded NO-GO round must no longer
		// pin the rollup at Failed.
		markTerminal("iterate-happy-code-750-r1", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-happy-verify-750-r1", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-happy-review-750-0-r1", meithealv1alpha1.AgenticTaskVerdictGo, "")
		reconcile(wl)

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseCompleted),
			"a converged fix iteration completes the Workload")
		Expect(fresh.Status.SucceededTasks).To(Equal(int32(3)), "only the latest round is counted")
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(0)))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(1)))
	})

	It("pairs iteration coder tasks with the revision coder Agent when configured (#951)", func() {
		wl := newWorkload("iterate-revision", meithealv1alpha1.WorkloadSpec{
			Intent:                "revision profile pairing",
			Repo:                  "defilantech/LLMKube",
			Issues:                []int32{753},
			CoderAgentRef:         &corev1.LocalObjectReference{Name: "coder"},
			RevisionCoderAgentRef: &corev1.LocalObjectReference{Name: "revision-coder"},
			VerifierAgentRef:      &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)

		// The base round still uses the issue-fix coder profile.
		var base meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-revision-code-753"}, &base)).To(Succeed())
		Expect(base.Spec.AgentRef.Name).To(Equal("coder"))
		Expect(base.Spec.Payload.ReviseFromBranch).To(BeEmpty(),
			"the base attempt has nothing to restore")

		markTerminal("iterate-revision-code-753", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-revision-verify-753", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-revision-review-753-0", meithealv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		var code meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-revision-code-753-r1"}, &code)).To(Succeed())
		Expect(code.Spec.AgentRef.Name).To(Equal("revision-coder"),
			"the fix iteration must run under the revision-tuned profile")
		Expect(code.Spec.Payload.ReviseFromBranch).To(Equal("meitheal/iterate-revision/issue-753"))

		// Verify + review keep their own refs.
		var verify meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-revision-verify-753-r1"}, &verify)).To(Succeed())
		Expect(verify.Spec.AgentRef.Name).To(Equal("gate"))

		// A configured revision profile is the intended pairing: no
		// RevisionUnderIssueFixProfile warning.
		Expect(recorder.Events).NotTo(Receive())
	})

	It("fails the Workload once the iteration budget is exhausted (today's terminal behavior)", func() {
		wl := newWorkload("iterate-exhausted", meithealv1alpha1.WorkloadSpec{
			Intent:           "fix iteration exhaustion",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{751},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)
		markTerminal("iterate-exhausted-code-751", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-exhausted-verify-751", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-exhausted-review-751-0", meithealv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		// The r1 round exists; reject it too.
		markTerminal("iterate-exhausted-code-751-r1", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-exhausted-verify-751-r1", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-exhausted-review-751-0-r1", meithealv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		// No r2: the default budget of 1 is spent.
		var r2 meithealv1alpha1.AgenticTask
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-exhausted-code-751-r2"}, &r2)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "exhausted budget must not emit another iteration")

		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseFailed),
			"a NO-GO on the last allowed iteration fails the Workload as before #946")
		completed := findCondition(fresh.Status.Conditions, conditionTypeCompleted)
		Expect(completed).NotTo(BeNil())
		Expect(completed.Reason).To(Equal("ChildrenIncomplete"))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(1)))
	})

	// #1636/#1641: the issueAsk rail rewrote the reviewer's GO into a NO-GO
	// carrying no findings. No fix iteration opens (the coder would be handed
	// an approval it cannot act on), but the demoted NO-GO still counts as
	// incomplete and fails the Workload. The suppression therefore has to be
	// recorded, or the operator sees a Failed Workload on a branch the
	// reviewer approved with nothing anywhere saying why.
	It("records the suppression when an inert issueAsk demotion blocks the iteration", func() {
		wl := newWorkload("iterate-inert", meithealv1alpha1.WorkloadSpec{
			Intent:           "inert demotion suppression",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{753},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)
		markTerminal("iterate-inert-code-753", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-inert-verify-753", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-inert-review-753-0",
			meithealv1alpha1.AgenticTaskVerdictNoGo, inertDemotedResult)
		reconcile(wl)

		var r1 meithealv1alpha1.AgenticTask
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-inert-code-753-r1"}, &r1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"an empty-handed issueAsk demotion must not open a fix iteration")

		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseFailed),
			"the demoted NO-GO still rolls the Workload to Failed; that is what needs explaining")

		suppressed := findCondition(fresh.Status.Conditions, conditionTypeReviewIterationSuppressed)
		Expect(suppressed).NotTo(BeNil(),
			"a skipped fix iteration must be reported, not silent")
		Expect(suppressed.Status).To(Equal(metav1.ConditionTrue))
		Expect(suppressed.Reason).To(Equal("InertDemotion"))
		Expect(suppressed.Message).To(ContainSubstring("review-753-0"),
			"the condition must name the review task it suppressed")
		Expect(suppressed.Message).To(ContainSubstring(demotionReasonIssueAsk),
			"the condition must carry the rail's demotionReason")

		Expect(recorder.Events).To(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring(conditionTypeReviewIterationSuppressed),
			ContainSubstring("review-753-0"),
		)))

		// Steady state: a re-reconcile must not churn the condition or
		// re-fire the event on every pass.
		before := *suppressed
		reconcile(wl)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		after := findCondition(fresh.Status.Conditions, conditionTypeReviewIterationSuppressed)
		Expect(after).NotTo(BeNil())
		Expect(after.LastTransitionTime).To(Equal(before.LastTransitionTime))
		Expect(recorder.Events).NotTo(Receive())
	})

	// #41: a NO-GO the executor marked untrusted (here the issueAsk rail's
	// non-GO path, carrying blockers about files the branch never touched)
	// must not become the coder's task list. The review is re-run once on the
	// same branch; its verdict then decides the round.
	untrustedResult := func(summary string) string {
		return string(reviewResultRaw(meithealv1alpha1.AgenticTaskVerdictNoGo, summary, map[string]any{
			"issueAskVerified": false,
			"verdictDemoted":   true,
			"verdictDemotedBy": reviewer.RailIssueAsk,
			"verdictClaimed":   string(meithealv1alpha1.AgenticTaskVerdictNoGo),
			"demotionReason":   demotionReasonIssueAsk,
			"findings": []any{map[string]any{
				"severity": "blocker", "area": "scope", "message": "the reconciler drops the retry budget",
				"file": "internal/controller/ghost.go", "line": 42,
			}},
		}).Raw)
	}

	untrustedSpec := func(issue int32) meithealv1alpha1.WorkloadSpec {
		return meithealv1alpha1.WorkloadSpec{
			Intent:           "untrusted review re-run",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{issue},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
			MaxReviewIterations: ptr.To(int32(2)),
		}
	}

	// startUntrusted plans the base round and drives it to an untrusted
	// reviewer NO-GO, then reconciles once.
	startUntrusted := func(name string, spec meithealv1alpha1.WorkloadSpec) *meithealv1alpha1.Workload {
		wl := newWorkload(name, spec)
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})
		n := spec.Issues[0]
		reconcile(wl)
		markTerminal(fmt.Sprintf("%s-code-%d", name, n), meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal(fmt.Sprintf("%s-verify-%d", name, n), meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal(fmt.Sprintf("%s-review-%d-0", name, n), meithealv1alpha1.AgenticTaskVerdictNoGo,
			untrustedResult("REQUEST-CHANGES: ghost.go drops the retry budget"))
		reconcile(wl)
		return wl
	}

	notFound := func(name string) {
		var t meithealv1alpha1.AgenticTask
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &t)
		ExpectWithOffset(1, apierrors.IsNotFound(err)).To(BeTrue(), "%s must not exist", name)
	}

	It("re-reviews an untrusted NO-GO once instead of iterating, and completes when the re-review approves (#41)", func() {
		wl := startUntrusted("untrusted-go", untrustedSpec(760))

		notFound("untrusted-go-code-760-r1")
		var rr meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "untrusted-go-review-760-0-rr"}, &rr)).
			To(Succeed(), "the untrusted review must be re-run once")
		Expect(rr.Spec.Kind).To(Equal(meithealv1alpha1.AgenticTaskKindReview))
		Expect(rr.Spec.AgentRef.Name).To(Equal("reviewer"))
		Expect(rr.Spec.Payload.Branch).To(Equal("meitheal/untrusted-go/issue-760"), "the SAME branch is re-reviewed")
		Expect(rr.Spec.Payload.OpenPullRequest).To(BeTrue())
		Expect(rr.Spec.DependsOn).To(ConsistOf("untrusted-go-verify-760"))
		Expect(rr.Labels[labelStep]).To(Equal("review-760-0-rr"))

		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseDispatched),
			"the Workload waits on the re-review instead of failing on the untrusted NO-GO")
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(0)), "a re-review is not a fix iteration")
		Expect(fresh.Status.Tasks).To(ContainElement(HaveField("Name", "untrusted-go-review-760-0-rr")))
		Expect(findCondition(fresh.Status.Conditions, conditionTypeReviewIterationTriggered)).To(BeNil())
		rerun := findCondition(fresh.Status.Conditions, conditionTypeReviewRerunTriggered)
		Expect(rerun).NotTo(BeNil())
		Expect(rerun.Status).To(Equal(metav1.ConditionTrue))
		Expect(rerun.Reason).To(Equal("UntrustedReview"))
		Expect(rerun.Message).To(ContainSubstring("review-760-0: " + demotionReasonIssueAsk))

		// Idempotent: a further reconcile neither re-emits nor iterates.
		reconcile(wl)
		var tasks meithealv1alpha1.AgenticTaskList
		Expect(k8sClient.List(ctx, &tasks, client.InNamespace("default"),
			client.MatchingLabels{labelWorkload: "untrusted-go"})).To(Succeed())
		Expect(tasks.Items).To(HaveLen(4))

		markTerminal("untrusted-go-review-760-0-rr", meithealv1alpha1.AgenticTaskVerdictGo, "")
		reconcile(wl)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseCompleted),
			"a trusted GO re-review completes the Workload; the untrusted NO-GO it replaced no longer counts")
		Expect(fresh.Status.IncompleteTasks).To(Equal(int32(0)))
		notFound("untrusted-go-code-760-r1")
	})

	It("iterates on a trusted re-review NO-GO with the re-review's feedback (#41)", func() {
		wl := startUntrusted("untrusted-nogo", untrustedSpec(761))
		markTerminal("untrusted-nogo-review-761-0-rr", meithealv1alpha1.AgenticTaskVerdictNoGo, noGoResult)
		reconcile(wl)

		var code meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "untrusted-nogo-code-761-r1"}, &code)).
			To(Succeed(), "a trusted NO-GO re-review iterates as any review does")
		Expect(code.Spec.Payload.Prompt).To(ContainSubstring("reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30"))
		Expect(code.Spec.Payload.Prompt).NotTo(ContainSubstring("ghost.go"),
			"the untrusted review's findings must never reach the coder")
	})

	It("ends the issue incomplete with ReviewUntrusted when the re-review is untrusted too (#41)", func() {
		wl := startUntrusted("untrusted-twice", untrustedSpec(762))
		markTerminal("untrusted-twice-review-762-0-rr", meithealv1alpha1.AgenticTaskVerdictNoGo,
			untrustedResult("REQUEST-CHANGES: ghost.go still drops the retry budget"))
		// Drain the events of the first pass before the one under test.
		for len(recorder.Events) > 0 {
			<-recorder.Events
		}
		reconcile(wl)

		notFound("untrusted-twice-code-762-r1")
		notFound("untrusted-twice-review-762-0-rr-rr")
		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseFailed))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(0)))
		untrusted := findCondition(fresh.Status.Conditions, conditionTypeReviewUntrusted)
		Expect(untrusted).NotTo(BeNil(), "the operator must be told the failure is the review's trustworthiness")
		Expect(untrusted.Status).To(Equal(metav1.ConditionTrue))
		Expect(untrusted.Reason).To(Equal("UntrustedAfterRereview"))
		Expect(untrusted.Message).To(ContainSubstring("review-762-0-rr: " + demotionReasonIssueAsk))
		Expect(untrusted.Message).To(ContainSubstring("untrusted"))
		Expect(untrusted.Message).NotTo(ContainSubstring("escalation"))
		Expect(recorder.Events).To(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring(conditionTypeReviewUntrusted),
			ContainSubstring("review-762-0-rr"),
		)))

		before := *untrusted
		reconcile(wl)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		after := findCondition(fresh.Status.Conditions, conditionTypeReviewUntrusted)
		Expect(after).NotTo(BeNil())
		Expect(after.LastTransitionTime).To(Equal(before.LastTransitionTime))
		Expect(recorder.Events).NotTo(Receive())
	})

	It("sends an untrusted re-review to the escalation reviewers when configured (#41)", func() {
		spec := untrustedSpec(763)
		spec.EscalationReviewerAgentRefs = []corev1.LocalObjectReference{{Name: "big-reviewer"}}
		wl := startUntrusted("untrusted-esc", spec)

		// The untrusted original must not fire escalation while its
		// re-review is pending.
		notFound("untrusted-esc-escalate-763-0")

		markTerminal("untrusted-esc-review-763-0-rr", meithealv1alpha1.AgenticTaskVerdictNoGo,
			untrustedResult("REQUEST-CHANGES: ghost.go still drops the retry budget"))
		reconcile(wl)

		var esc meithealv1alpha1.AgenticTask
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "untrusted-esc-escalate-763-0"}, &esc)).
			To(Succeed(), "an untrusted re-review routes to escalation, as a demoted GO does")
		Expect(esc.Spec.AgentRef.Name).To(Equal("big-reviewer"))
		notFound("untrusted-esc-code-763-r1")

		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		untrusted := findCondition(fresh.Status.Conditions, conditionTypeReviewUntrusted)
		Expect(untrusted).NotTo(BeNil())
		Expect(untrusted.Message).To(ContainSubstring("escalation reviewers"))
	})

	It("does not iterate when the reviewer says GO", func() {
		wl := newWorkload("iterate-go", meithealv1alpha1.WorkloadSpec{
			Intent:           "no iteration on GO",
			Repo:             "defilantech/LLMKube",
			Issues:           []int32{752},
			CoderAgentRef:    &corev1.LocalObjectReference{Name: "coder"},
			VerifierAgentRef: &corev1.LocalObjectReference{Name: "gate"},
			ReviewerAgentRefs: []corev1.LocalObjectReference{
				{Name: "reviewer"},
			},
		})
		Expect(k8sClient.Create(ctx, wl)).To(Succeed())
		DeferCleanup(func() {
			cleanupChildren(wl)
			_ = k8sClient.Delete(ctx, wl)
		})

		reconcile(wl)
		markTerminal("iterate-go-code-752", meithealv1alpha1.AgenticTaskVerdictGo, "")
		markTerminal("iterate-go-verify-752", meithealv1alpha1.AgenticTaskVerdictGatePass, "")
		markTerminal("iterate-go-review-752-0", meithealv1alpha1.AgenticTaskVerdictGo, "")
		reconcile(wl)

		var r1 meithealv1alpha1.AgenticTask
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "iterate-go-code-752-r1"}, &r1)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "GO must not trigger a fix iteration")

		var fresh meithealv1alpha1.Workload
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(wl), &fresh)).To(Succeed())
		Expect(fresh.Status.Phase).To(Equal(meithealv1alpha1.WorkloadPhaseCompleted))
		Expect(fresh.Status.ReviewIterations).To(Equal(int32(0)))
		Expect(findCondition(fresh.Status.Conditions, conditionTypeReviewIterationTriggered)).To(BeNil())
	})
})
