package agent

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// A terminal submit_result that carries concerns must surface them at the
// top level of the task result's Extra (status.result.extra.concerns), for
// both the coder GO path and the model-decided (reviewer) path, while every
// existing Extra key stays put.
func TestSubmitResultConcernsReachResultExtra(t *testing.T) {
	// pkg/agent/tools imports pkg/agent, so the terminal is built by hand
	// here with the exact shape SubmitResultTool.Execute emits (pinned in
	// tools.TestSubmitResult_ConcernsSurfacedInExtra): extra["concerns"].
	want := []map[string]any{{"kind": "novelty", "text": "copied layout", "files": []string{"a.rs"}}}
	res := &ToolResult{Terminal: true, Verdict: "GO", Summary: "s", CommitMessage: "c",
		Extra: map[string]any{"prBody": "b", "concerns": want}}
	lr := &LoopResult{Terminal: res, Turns: 3}
	e := &NativeAgentLoopExecutor{}
	tref := corev1.ObjectReference{Name: "t"}

	results := map[string]*Result{
		"coder": e.goResult(time.Now(), tref, lr, "br", "sha"),
		"reviewer": e.modelDecidedResult(context.Background(), time.Now(), tref, lr,
			meithealv1alpha1.AgenticTaskVerdictGo, "", ""),
	}
	for name, r := range results {
		got, _ := r.Extra["concerns"].([]map[string]any)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Extra[concerns] = %#v, want %#v", name, r.Extra["concerns"], want)
		}
		if me, _ := r.Extra["modelExtra"].(map[string]any); me["prBody"] != "b" {
			t.Errorf("%s: modelExtra lost prBody: %v", name, r.Extra["modelExtra"])
		}
	}

	// No concerns: no top-level key, byte-for-byte as before.
	lr2 := &LoopResult{Terminal: &ToolResult{Terminal: true, Verdict: "GO", Extra: map[string]any{}}}
	if _, ok := e.goResult(time.Now(), tref, lr2, "b", "s").Extra["concerns"]; ok {
		t.Error("goResult must not add concerns when none were raised")
	}
	if _, ok := e.modelDecidedResult(context.Background(), time.Now(), tref, lr2,
		meithealv1alpha1.AgenticTaskVerdictGo, "", "").Extra["concerns"]; ok {
		t.Error("modelDecidedResult must not add concerns when none were raised")
	}
}
