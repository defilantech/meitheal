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
	"time"
)

// IssueComment is one comment on an issue.
type IssueComment struct {
	Author  string
	Body    string
	Created time.Time
}

// IssueDetail is an issue as the planner reads it: title, body, labels and
// the full comment thread.
type IssueDetail struct {
	Title    string
	Body     string
	Labels   []string
	Comments []IssueComment
}

// RunForge is every forge call the run loop makes. The embedded
// PreflightProbe carries its fail-closed contract: any query that could not
// be answered, including a bad repo slug, is an error, never a zero value.
// The same holds for every method here. The one deliberate empty result is
// PRForBranch, which returns "" with a nil error when no open PR exists.
type RunForge interface {
	PreflightProbe
	CommentIssue(ctx context.Context, repo string, issue int32, body string) error
	CommentsSince(ctx context.Context, repo string, issue int32, since time.Time) ([]IssueComment, error)
	// PRForBranch returns the URL of the open PR whose head is branch, or ""
	// with a nil error when there is none.
	PRForBranch(ctx context.Context, repo, branch string) (url string, err error)
	// UpdatePRBody replaces the body of the open PR for branch. It is an
	// error when no open PR exists.
	UpdatePRBody(ctx context.Context, repo, branch, body string) error
	// LabelPR adds label to the open PR for branch. It is an error when no
	// open PR exists.
	LabelPR(ctx context.Context, repo, branch, label string) error
	FetchIssue(ctx context.Context, repo string, issue int32) (IssueDetail, error)
}
