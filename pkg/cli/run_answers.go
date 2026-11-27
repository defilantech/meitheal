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
	"fmt"
	"sort"
	"strings"
	"time"
)

const answerPrefix = "/meitheal "

// ParseAnswer finds the first answer command in a comment body. A command is a
// whole line, after trimming: "/meitheal requeue", "/meitheal drop", or
// "/meitheal decide TEXT" with non-empty TEXT. A command that is merely
// mentioned mid-sentence is not one.
func ParseAnswer(body string) (cmd, text string, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		rest, found := strings.CutPrefix(line, answerPrefix)
		if !found {
			continue
		}
		rest = strings.TrimSpace(rest)
		switch {
		case rest == "requeue" || rest == "drop":
			return rest, "", true
		case strings.HasPrefix(rest, "decide "):
			if t := strings.TrimSpace(strings.TrimPrefix(rest, "decide ")); t != "" {
				return "decide", t, true
			}
		}
	}
	return "", "", false
}

// RejectedAnswer is a well-formed command from the answerer that the decision
// cannot take, for instance `decide` on a decision whose options lack it.
type RejectedAnswer struct {
	Issue   int32
	Kind    string
	Created time.Time
	Command string
	Reason  string
}

// PollAnswers reads the issue comments of every unanswered decision and
// records the first valid command from answerer made at or after the park
// (allowing parkCommentSkew of clock skew).
// A command the decision rejects is not valid for it: it is skipped, reported
// in rejected, and later comments are still tried. It returns the decisions it
// newly answered and never posts to the forge. err is reserved for forge and
// I/O failures, so one stale comment cannot fail every poll; a failure on one
// decision does not stop the others, and the errors come back joined alongside
// whatever was answered.
//
// Only decisions of repo are polled: one written for another repository
// names an issue number that means something else here. A decision from
// before decisions recorded their repository is polled too; the run loop,
// which knows its queue, narrows that further.
func PollAnswers(
	ctx context.Context, f RunForge, dir, repo, answerer string,
) (answered []Decision, rejected []RejectedAnswer, err error) {
	return pollAnswers(ctx, f, dir, repo, answerer, func(d Decision) bool { return d.Repo == "" || d.Repo == repo }, nil)
}

// pollAnswers is PollAnswers for the run loop: owns narrows the decisions
// polled, and a comment made at or before consumed[issue] (the answer the
// loop last acted on for the issue) is never taken again. Without it the
// clock-skew allowance would let a park opened within parkCommentSkew of
// that answer take it a second time.
func pollAnswers(
	ctx context.Context, f RunForge, dir, repo, answerer string, owns func(Decision) bool,
	consumed map[int32]time.Time,
) (answered []Decision, rejected []RejectedAnswer, err error) {
	decisions, listErr := ListDecisions(dir)
	errs := []error{listErr}
	if answerer == "" {
		return nil, nil, errors.Join(errs...)
	}
	for _, d := range decisions {
		// Without a park time there is no way to tell this park's comments
		// from the issue's whole history.
		if d.Answer != "" || d.Opened.IsZero() || !owns(d) {
			continue
		}
		// The park's open time is this host's clock and a comment's is the
		// forge's, so an answer is looked for from parkCommentSkew before
		// it, as park comments are.
		since := d.Opened.Add(-parkCommentSkew)
		comments, cerr := f.CommentsSince(ctx, repo, d.Issue, since)
		if cerr != nil {
			errs = append(errs, fmt.Errorf("comments for issue %d: %w", d.Issue, cerr))
			continue
		}
		sort.SliceStable(comments, func(i, j int) bool { return comments[i].Created.Before(comments[j].Created) })
		for _, c := range comments {
			// CommentsSince is inclusive, so this only guards a forge that
			// ignores since.
			if c.Author == "" || !strings.EqualFold(c.Author, answerer) || c.Created.Before(since) {
				continue
			}
			if last, ok := consumed[d.Issue]; ok && !c.Created.After(last) {
				continue
			}
			cmd, text, ok := ParseAnswer(c.Body)
			if !ok {
				continue
			}
			answer := cmd
			if cmd == "decide" {
				answer = "decide: " + text
			}
			aerr := AnswerDecision(dir, d.Issue, d.Kind, answer)
			if errors.Is(aerr, errAnswerNotOffered) {
				rejected = append(rejected, RejectedAnswer{
					Issue: d.Issue, Kind: d.Kind, Created: c.Created, Command: cmd, Reason: aerr.Error(),
				})
				continue
			}
			if aerr != nil {
				errs = append(errs, fmt.Errorf("answer issue %d: %w", d.Issue, aerr))
				break
			}
			d.Answer = answer
			d.answeredBy = c.Created
			answered = append(answered, d)
			break
		}
	}
	return answered, rejected, errors.Join(errs...)
}
