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

// PollAnswers reads the issue comments of every unanswered decision and
// records the first valid command from answerer made at or after the park.
// It returns the decisions it newly answered and never posts to the forge.
// A failure on one decision does not stop the others; the errors come back
// joined alongside whatever was answered.
func PollAnswers(ctx context.Context, f RunForge, dir, repo, answerer string) ([]Decision, error) {
	decisions, listErr := ListDecisions(dir)
	errs := []error{listErr}
	var answered []Decision
	if answerer == "" {
		return nil, errors.Join(errs...)
	}
	for _, d := range decisions {
		// Without a park time there is no way to tell this park's comments
		// from the issue's whole history.
		if d.Answer != "" || d.Opened.IsZero() {
			continue
		}
		comments, err := f.CommentsSince(ctx, repo, d.Issue, d.Opened)
		if err != nil {
			errs = append(errs, fmt.Errorf("comments for issue %d: %w", d.Issue, err))
			continue
		}
		sort.SliceStable(comments, func(i, j int) bool { return comments[i].Created.Before(comments[j].Created) })
		for _, c := range comments {
			// CommentsSince is inclusive, so this only guards a forge that
			// ignores since.
			if c.Author == "" || !strings.EqualFold(c.Author, answerer) || c.Created.Before(d.Opened) {
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
			if err := AnswerDecision(dir, d.Issue, d.Kind, answer); err != nil {
				errs = append(errs, fmt.Errorf("answer issue %d: %w", d.Issue, err))
			} else {
				d.Answer = answer
				answered = append(answered, d)
			}
			break
		}
	}
	return answered, errors.Join(errs...)
}
