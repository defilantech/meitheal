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
	"testing"
	"time"
)

func TestParseAnswer(t *testing.T) {
	cases := []struct {
		name, body, cmd, text string
		ok                    bool
	}{
		{"requeue", "/meitheal requeue", "requeue", "", true},
		{"decide", "/meitheal decide use the window from #46", "decide", "use the window from #46", true},
		{"drop", "/meitheal drop", "drop", "", true},
		{"padded", "  /meitheal drop  ", "drop", "", true},
		{"prefixed prose", "please /meitheal requeue", "", "", false},
		{"unknown", "/meitheal frobnicate", "", "", false},
		{"requeue with trailing text", "/meitheal requeue now", "", "", false},
		{"decide without text", "/meitheal decide", "", "", false},
		{"decide blank text", "/meitheal decide   ", "", "", false},
		{"second line", "thanks\n/meitheal drop\n", "drop", "", true},
		{"crlf", "/meitheal requeue\r\n", "requeue", "", true},
		{"first valid wins", "/meitheal nope\n/meitheal drop\n/meitheal requeue", "drop", "", true},
		{"empty", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, text, ok := ParseAnswer(tc.body)
			if cmd != tc.cmd || text != tc.text || ok != tc.ok {
				t.Errorf("ParseAnswer(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.body, cmd, text, ok, tc.cmd, tc.text, tc.ok)
			}
		})
	}
}

// answersForge serves canned comments per issue and honours since, as the
// real client does, and records what it was asked for.
type answersForge struct {
	*fakeRunForge
	byIssue map[int32][]IssueComment
	since   map[int32]time.Time
}

func (f *answersForge) CommentsSince(
	_ context.Context, _ string, issue int32, since time.Time,
) ([]IssueComment, error) {
	if f.since == nil {
		f.since = map[int32]time.Time{}
	}
	f.since[issue] = since
	var out []IssueComment
	for _, c := range f.byIssue[issue] {
		if !c.Created.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

var answersT0 = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func parkForAnswers(t *testing.T, dir string, opened time.Time) {
	t.Helper()
	if _, err := ParkDecision(dir, Decision{
		Issue: 7, Kind: "needs-decision", Opened: opened,
		Options: []string{"requeue", "decide", "drop"},
	}); err != nil {
		t.Fatal(err)
	}
}

func newAnswersForge(c map[int32][]IssueComment) *answersForge {
	return &answersForge{fakeRunForge: newFakeRunForge(), byIssue: c}
}

func at(m int) time.Time { return answersT0.Add(time.Duration(m) * time.Minute) }

func answerOf(t *testing.T, dir string) string {
	t.Helper()
	ds, err := ListDecisions(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Issue == 7 {
			return d.Answer
		}
	}
	t.Fatal("no decision for 7")
	return ""
}

func TestPollAnswers_OnlyAnswererCounts(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	f := newAnswersForge(map[int32][]IssueComment{7: {
		{Author: "someone-else", Body: "/meitheal requeue", Created: at(1)},
		{Author: "", Body: "/meitheal requeue", Created: at(2)},
	}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || answerOf(t, dir) != "" {
		t.Fatalf("answered %v, want nothing from a non-answerer or empty author", got)
	}
	// The answerer matches case-insensitively.
	f.byIssue[7] = append(f.byIssue[7], IssueComment{Author: "defilan", Body: "/meitheal drop", Created: at(3)})
	got, _, err = PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 1 || got[0].Answer != "drop" {
		t.Fatalf("got %v, %v, want one drop answer", got, err)
	}
}

func TestPollAnswers_EmptyAnswererMatchesNobody(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	f := newAnswersForge(map[int32][]IssueComment{7: {{Author: "", Body: "/meitheal drop", Created: at(1)}}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v, want nothing", got, err)
	}
}

func TestPollAnswers_FirstValidCommandWins(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	f := newAnswersForge(map[int32][]IssueComment{7: {
		{Author: "Defilan", Body: "hmm\n/meitheal bogus\n/meitheal decide use #46's window", Created: at(1)},
		{Author: "Defilan", Body: "/meitheal drop", Created: at(2)},
	}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Answer != "decide: use #46's window" {
		t.Fatalf("got %v, want the first valid command", got)
	}
	if a := answerOf(t, dir); a != "decide: use #46's window" {
		t.Errorf("stored answer = %q", a)
	}
}

func TestPollAnswers_FirstValidCommandWinsRegardlessOfOrder(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	f := newAnswersForge(map[int32][]IssueComment{7: {
		{Author: "Defilan", Body: "/meitheal drop", Created: at(5)},
		{Author: "Defilan", Body: "/meitheal requeue", Created: at(1)},
	}})
	got, _, _ := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if len(got) != 1 || got[0].Answer != "requeue" {
		t.Fatalf("got %v, want the earliest comment's command", got)
	}
}

func TestPollAnswers_CommentsBeforeParkIgnored(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	f := newAnswersForge(map[int32][]IssueComment{7: {
		{Author: "Defilan", Body: "/meitheal drop", Created: at(-5)},
	}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v, want the pre-park comment ignored", got, err)
	}
	if !f.since[7].Equal(answersT0) {
		t.Errorf("asked since %v, want the park time %v", f.since[7], answersT0)
	}
	// A forge that ignores since must not leak old comments either.
	g := &sinceBlindForge{answersForge: f}
	got, _, _ = PollAnswers(context.Background(), g, dir, "o/r", "Defilan")
	if len(got) != 0 {
		t.Fatalf("got %v from a forge that ignores since", got)
	}
}

type sinceBlindForge struct{ *answersForge }

func (f *sinceBlindForge) CommentsSince(_ context.Context, _ string, issue int32, _ time.Time) ([]IssueComment, error) {
	return f.byIssue[issue], nil
}

func TestPollAnswers_ZeroOpenedSkipped(t *testing.T) {
	dir := t.TempDir()
	// ParkDecision stamps Opened, so write the zero-Opened file by hand.
	if err := writeDecisionFile(decisionPath(dir, 8, "needs-decision"),
		[]byte("issue: 8\nkind: needs-decision\noptions: [requeue, decide, drop]\n")); err != nil {
		t.Fatal(err)
	}
	f := newAnswersForge(map[int32][]IssueComment{8: {{Author: "Defilan", Body: "/meitheal drop", Created: at(1)}}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v, want skipped", got, err)
	}
	if _, asked := f.since[8]; asked {
		t.Error("read comments for a decision with no park time")
	}
}

func TestPollAnswers_AlreadyAnsweredSkipped(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	if err := AnswerDecision(dir, 7, "needs-decision", "requeue"); err != nil {
		t.Fatal(err)
	}
	f := newAnswersForge(map[int32][]IssueComment{7: {{Author: "Defilan", Body: "/meitheal drop", Created: at(1)}}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v, want skipped", got, err)
	}
	if _, asked := f.since[7]; asked {
		t.Error("read comments for an answered decision")
	}
	if a := answerOf(t, dir); a != "requeue" {
		t.Errorf("answer = %q, want unchanged requeue", a)
	}
}

func TestPollAnswers_PostsNothing(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	f := newAnswersForge(map[int32][]IssueComment{7: {{Author: "Defilan", Body: "/meitheal drop", Created: at(1)}}})
	if _, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan"); err != nil {
		t.Fatal(err)
	}
	if len(f.comments) != 0 {
		t.Errorf("posted %v, want nothing", f.comments)
	}
}

func TestAnswerDecision_DecideFreeText(t *testing.T) {
	dir := t.TempDir()
	parkForAnswers(t, dir, answersT0)
	if err := AnswerDecision(dir, 7, "needs-decision", "decide: use the window"); err != nil {
		t.Fatalf("decide with text: %v", err)
	}
	if a := answerOf(t, dir); a != "decide: use the window" {
		t.Errorf("answer = %q", a)
	}
	for _, bad := range []string{"decide:", "decide:   ", "decidex: y", "frob: y"} {
		d2 := t.TempDir()
		parkForAnswers(t, d2, answersT0)
		if err := AnswerDecision(d2, 7, "needs-decision", bad); err == nil {
			t.Errorf("AnswerDecision(%q) = nil, want an error", bad)
		}
	}
	// decide must be an offered option.
	d3 := t.TempDir()
	odd := Decision{Issue: 9, Kind: "adjudicate", Options: []string{"accept", "revise"}}
	if _, err := ParkDecision(d3, odd); err != nil {
		t.Fatal(err)
	}
	if err := AnswerDecision(d3, 9, "adjudicate", "decide: x"); err == nil {
		t.Error("decide accepted although not among the options")
	}
}

func TestPollAnswers_RejectedCommandSkippedThenValidOneWins(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{
		Issue: 7, Kind: "needs-decision", Opened: answersT0, Options: []string{"requeue", "drop"},
	}); err != nil {
		t.Fatal(err)
	}
	f := newAnswersForge(map[int32][]IssueComment{7: {
		{Author: "Defilan", Body: "/meitheal decide x", Created: at(1)},
		{Author: "Defilan", Body: "/meitheal drop", Created: at(2)},
	}})
	got, rej, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 1 || got[0].Answer != "drop" {
		t.Fatalf("got %v, %v, want the later valid drop", got, err)
	}
	r := RejectedAnswer{}
	if len(rej) == 1 {
		r = rej[0]
	}
	if r.Issue != 7 || r.Command != "decide" || !r.Created.Equal(at(1)) || r.Reason == "" {
		t.Fatalf("rejected = %+v, want the decide at minute 1", rej)
	}
}

func TestPollAnswers_OnlyRejectedStaysUnansweredWithNilError(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{
		Issue: 7, Kind: "needs-decision", Opened: answersT0, Options: []string{"requeue", "drop"},
	}); err != nil {
		t.Fatal(err)
	}
	f := newAnswersForge(map[int32][]IssueComment{7: {{Author: "Defilan", Body: "/meitheal decide x", Created: at(1)}}})
	got, rej, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 0 || len(rej) != 1 || answerOf(t, dir) != "" {
		t.Fatalf("got %v, rej %v, err %v, want unanswered, one rejection, nil error", got, rej, err)
	}
}

type failingIssueForge struct {
	*answersForge
	failIssue int32
}

func (f *failingIssueForge) CommentsSince(
	ctx context.Context, repo string, issue int32, since time.Time,
) ([]IssueComment, error) {
	if issue == f.failIssue {
		return nil, context.DeadlineExceeded
	}
	return f.answersForge.CommentsSince(ctx, repo, issue, since)
}

func TestPollAnswers_ForgeErrorDoesNotStopOtherDecisions(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int32{5, 7} {
		if _, err := ParkDecision(dir, Decision{
			Issue: n, Kind: "needs-decision", Opened: answersT0, Options: []string{"requeue", "decide", "drop"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	base := newAnswersForge(map[int32][]IssueComment{
		7: {{Author: "Defilan", Body: "/meitheal drop", Created: at(1)}},
	})
	f := &failingIssueForge{answersForge: base, failIssue: 5}
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err == nil {
		t.Error("want the forge error returned")
	}
	if len(got) != 1 || got[0].Issue != 7 {
		t.Fatalf("got %v, want issue 7 answered", got)
	}
}

// A3: a decision written for another repository is never answered from
// this repository's issue of the same number.
func TestPollAnswers_IgnoresAnotherRepositorysDecision(t *testing.T) {
	dir := t.TempDir()
	if _, err := ParkDecision(dir, Decision{
		Repo: "o/other", Issue: 7, Kind: "needs-decision", Opened: answersT0,
		Options: []string{"requeue", "decide", "drop"},
	}); err != nil {
		t.Fatal(err)
	}
	f := newAnswersForge(map[int32][]IssueComment{7: {{Author: "Defilan", Body: "/meitheal drop", Created: at(1)}}})
	got, _, err := PollAnswers(context.Background(), f, dir, "o/r", "Defilan")
	if err != nil || len(got) != 0 || answerOf(t, dir) != "" {
		t.Fatalf("answered = %+v, %v; want the other repository's decision left alone", got, err)
	}
	if _, asked := f.since[7]; asked {
		t.Fatal("polled comments for another repository's decision")
	}
}
