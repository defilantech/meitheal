package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A model that omits the optional extra field must still yield a non-nil
// Extra map. Every verdict rail records its evidence, or its inability to
// run, into the terminal extra, and every rail nil-guards rather than
// panics, so a nil map silently disables ALL of them at once: the reviewer
// on wl-1607-live-review-nolayout confabulated a GO and published
// result.extra = [outcome, transcriptRef, turnCount] with not one rail
// marker, the exact starved-extra signature of #1570. Observability must
// not be hostage to the model's submit verbosity.
func TestSubmitResult_OmittedExtraIsInitialized(t *testing.T) {
	res, err := SubmitResultTool{}.Execute(context.Background(),
		json.RawMessage(`{"verdict":"GO","summary":"s","commit_message":"c"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Extra == nil {
		t.Fatal("Extra must be initialized when the model omits it; nil silently disables every verdict rail")
	}
}

// A model that does send extra keeps them verbatim.
func TestSubmitResult_ProvidedExtraSurvives(t *testing.T) {
	res, err := SubmitResultTool{}.Execute(context.Background(),
		json.RawMessage(`{"verdict":"NO-GO","summary":"s","commit_message":"c","extra":{"prBody":"x"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := res.Extra["prBody"].(string); got != "x" {
		t.Fatalf("provided extra must survive, got %v", res.Extra)
	}
}

func TestSubmitResult_ConcernsSurfacedInExtra(t *testing.T) {
	res, err := SubmitResultTool{}.Execute(context.Background(), json.RawMessage(
		`{"verdict":"GO","summary":"s","commit_message":"c","extra":{"prBody":"x"},`+
			`"concerns":[{"kind":"novelty","text":"mirrors a kernel layout","files":["src/a.rs","src/b.rs"]},`+
			`{"kind":"novelty","text":"second"}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := res.Extra["concerns"].([]Concern)
	if !ok || len(got) != 2 {
		t.Fatalf("extra[concerns] = %#v, want 2 Concern entries", res.Extra["concerns"])
	}
	if got[0].Kind != "novelty" || got[0].Text != "mirrors a kernel layout" ||
		len(got[0].Files) != 2 || got[0].Files[1] != "src/b.rs" || got[1].Files != nil {
		t.Fatalf("concerns not preserved: %#v", got)
	}
	if res.Extra["prBody"] != "x" {
		t.Fatalf("existing extra key lost: %v", res.Extra)
	}
	raw, _ := json.Marshal(res.Extra)
	wantJSON := `"concerns":[{"kind":"novelty","text":"mirrors a kernel layout","files":["src/a.rs","src/b.rs"]}`
	if !strings.Contains(string(raw), wantJSON) {
		t.Fatalf("unexpected JSON shape: %s", raw)
	}
}

func TestSubmitResult_UnknownConcernKindRejected(t *testing.T) {
	_, err := SubmitResultTool{}.Execute(context.Background(), json.RawMessage(
		`{"verdict":"GO","summary":"s","commit_message":"c","concerns":[{"kind":"style","text":"t"}]}`))
	if err == nil {
		t.Fatal("expected error for unknown concern kind")
	}
}

func TestSubmitResult_NoConcerns_ExtraUnchanged(t *testing.T) {
	for _, in := range []string{
		`{"verdict":"GO","summary":"s","commit_message":"c"}`,
		`{"verdict":"GO","summary":"s","commit_message":"c","concerns":[]}`,
	} {
		res, err := SubmitResultTool{}.Execute(context.Background(), json.RawMessage(in))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, present := res.Extra["concerns"]; present || len(res.Extra) != 0 {
			t.Fatalf("extra must be unchanged without concerns, got %v", res.Extra)
		}
	}
}

func TestSubmitResult_SchemaMentionsNovelty(t *testing.T) {
	s := string(SubmitResultTool{}.Schema().Parameters)
	if !strings.Contains(s, "Set a concern of kind novelty if the change would copy code, text or design "+
		"from another inference engine or kernel library.") {
		t.Fatalf("schema lacks novelty description: %s", s)
	}
}
