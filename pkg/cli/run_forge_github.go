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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/defilantech/meitheal/pkg/agent/githubissue"
)

const (
	forgeTimeout = 15 * time.Second
	// forgePageSize and forgeMaxPages cap comment pagination: 10 pages of
	// 100 is far past any issue thread the loop should read.
	forgePageSize = 100
	forgeMaxPages = 10
	forgeBodyCap  = 4 << 20
	forgeErrCap   = 512
)

// statusError is a non-success HTTP status from the API.
type statusError struct {
	Code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

type githubRunForge struct {
	apiBase string
	token   func() (string, error)
	client  *http.Client
}

// NewGitHubRunForge returns a RunForge over the GitHub REST API at apiBase
// (https://api.github.com in production). token is called on every request,
// so a rotated token is picked up without a restart.
func NewGitHubRunForge(apiBase string, token func() (string, error)) RunForge {
	return &githubRunForge{
		apiBase: strings.TrimRight(apiBase, "/"),
		token:   token,
		client:  &http.Client{Timeout: forgeTimeout},
	}
}

func splitSlug(repo string) (owner, name string, err error) {
	owner, name, err = githubissue.ParseRepo(repo)
	if err != nil {
		return "", "", fmt.Errorf("run forge: repo %q: %w", repo, err)
	}
	return owner, name, nil
}

// do performs one request and returns the body and headers. A non-2xx
// status is an error carrying a truncated body; the token is never included.
func (f *githubRunForge) do(
	ctx context.Context, method, target string, payload any, wantStatus ...int,
) ([]byte, http.Header, error) {
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("run forge: marshal: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	tok, err := f.token()
	if err != nil {
		return nil, nil, fmt.Errorf("run forge: token: %w", err)
	}
	if tok == "" {
		return nil, nil, errors.New("run forge: token func returned an empty token")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, nil, errors.New("run forge: build request failed")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("run forge: %s %s: %w", method, req.URL.Path, scrub(err, tok))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, forgeBodyCap))
	if err != nil {
		return nil, nil, fmt.Errorf("run forge: read body: %w", scrub(err, tok))
	}
	ok := len(wantStatus) == 0 && resp.StatusCode == http.StatusOK
	for _, s := range wantStatus {
		if resp.StatusCode == s {
			ok = true
		}
	}
	if !ok {
		msg := string(raw)
		if tok != "" {
			msg = strings.ReplaceAll(msg, tok, "[redacted]")
		}
		if len(msg) > forgeErrCap {
			msg = msg[:forgeErrCap]
		}
		return nil, nil, &statusError{
			Code: resp.StatusCode,
			msg:  fmt.Sprintf("run forge: %s %s: status %d: %s", method, req.URL.Path, resp.StatusCode, msg),
		}
	}
	return raw, resp.Header, nil
}

// scrub drops err's text if it somehow carries the token.
func scrub(err error, tok string) error {
	if tok != "" && strings.Contains(err.Error(), tok) {
		return errors.New("request failed (details withheld)")
	}
	return err
}

func (f *githubRunForge) repoURL(repo, format string, args ...any) (string, error) {
	owner, name, err := splitSlug(repo)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/repos/%s/%s", f.apiBase, owner, name) + fmt.Sprintf(format, args...), nil
}

type ghUser struct {
	Login string `json:"login"`
}

type ghComment struct {
	User      ghUser    `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// OpenPRForIssue lists every open PR (paginated, capped; exceeding the cap is
// an error, never a truncated answer) and returns the first whose title or
// body references the issue: "#N" as a whole reference (not #50 for #5), the
// issue's full URL, or a closing keyword form (fixes/closes/resolves #N).
func (f *githubRunForge) OpenPRForIssue(ctx context.Context, repo string, issue int32) (string, error) {
	owner, name, err := splitSlug(repo)
	if err != nil {
		return "", err
	}
	target, err := f.repoURL(repo, "/pulls?state=open&per_page=%d", forgePageSize)
	if err != nil {
		return "", err
	}
	n := strconv.Itoa(int(issue))
	ref := regexp.MustCompile(`(?i)(^|[^\w#])#` + n + `\b|` +
		regexp.QuoteMeta("https://github.com/"+owner+"/"+name+"/issues/"+n) + `\b|` +
		`\b(fix(e[sd])?|close[sd]?|resolve[sd]?)\s*:?\s+#` + n + `\b`)
	found := ""
	err = f.eachPage(ctx, target, func(raw []byte) (bool, error) {
		var prs []struct {
			HTMLURL string `json:"html_url"`
			Title   string `json:"title"`
			Body    string `json:"body"`
		}
		if err := json.Unmarshal(raw, &prs); err != nil {
			return false, fmt.Errorf("run forge: decode pulls: %w", err)
		}
		for _, pr := range prs {
			if ref.MatchString(pr.Title) || ref.MatchString(pr.Body) {
				found = pr.HTMLURL
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return "", err
	}
	return found, nil
}

// eachPage GETs target and follows rel="next" Link headers, calling fn with
// each page body until fn reports done. Exceeding forgeMaxPages is an error.
// A next link that leaves apiBase's host is refused so the token never goes
// to another origin.
func (f *githubRunForge) eachPage(ctx context.Context, target string, fn func([]byte) (bool, error)) error {
	base, err := url.Parse(f.apiBase)
	if err != nil {
		return fmt.Errorf("run forge: bad api base: %w", err)
	}
	for page := 0; target != ""; page++ {
		if page >= forgeMaxPages {
			return fmt.Errorf("run forge: more than %d pages of results", forgeMaxPages)
		}
		raw, hdr, err := f.do(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		done, err := fn(raw)
		if err != nil || done {
			return err
		}
		target = nextLink(hdr.Get("Link"))
		if target != "" {
			u, err := url.Parse(target)
			if err != nil || u.Scheme != base.Scheme || u.Host != base.Host {
				return errors.New("run forge: pagination left the API host")
			}
		}
	}
	return nil
}

func (f *githubRunForge) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	if branch == "" {
		return false, errors.New("run forge: empty branch")
	}
	segs := strings.Split(branch, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	target, err := f.repoURL(repo, "/git/ref/heads/%s", strings.Join(segs, "/"))
	if err != nil {
		return false, err
	}
	_, _, err = f.do(ctx, http.MethodGet, target, nil)
	if err == nil {
		return true, nil
	}
	var se *statusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		return false, err
	}
	// A 404 on the ref is ambiguous with a repo the token cannot see; only
	// "repo visible, ref missing" is a definite no.
	repoURL, err := f.repoURL(repo, "")
	if err != nil {
		return false, err
	}
	if _, _, err := f.do(ctx, http.MethodGet, repoURL, nil); err != nil {
		return false, fmt.Errorf("run forge: ref %s not found and repo not readable: %w", branch, err)
	}
	return false, nil
}

func (f *githubRunForge) CommentIssue(ctx context.Context, repo string, issue int32, body string) error {
	target, err := f.repoURL(repo, "/issues/%d/comments", issue)
	if err != nil {
		return err
	}
	_, _, err = f.do(ctx, http.MethodPost, target, map[string]string{"body": body}, http.StatusCreated)
	return err
}

// comments reads the issue's comment thread, following the Link header. A
// non-zero since keeps comments created at or after it (filtering is on
// created_at; the API's own since parameter is on updated_at and only narrows
// the fetch).
func (f *githubRunForge) comments(
	ctx context.Context, repo string, issue int32, since time.Time,
) ([]IssueComment, error) {
	target, err := f.repoURL(repo, "/issues/%d/comments?per_page=%d", issue, forgePageSize)
	if err != nil {
		return nil, err
	}
	if !since.IsZero() {
		target += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	var all []IssueComment
	err = f.eachPage(ctx, target, func(raw []byte) (bool, error) {
		var cs []ghComment
		if err := json.Unmarshal(raw, &cs); err != nil {
			return false, fmt.Errorf("run forge: decode comments: %w", err)
		}
		for _, c := range cs {
			if !since.IsZero() && c.CreatedAt.Before(since) {
				continue
			}
			all = append(all, IssueComment{Author: c.User.Login, Body: c.Body, Created: c.CreatedAt})
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func nextLink(h string) string {
	if m := linkNext.FindStringSubmatch(h); m != nil {
		return m[1]
	}
	return ""
}

func (f *githubRunForge) CommentsSince(
	ctx context.Context, repo string, issue int32, since time.Time,
) ([]IssueComment, error) {
	return f.comments(ctx, repo, issue, since)
}

// openPR finds the open PR whose head is branch (same-repo head), returning
// its number and URL, or 0 and "" when there is none.
func (f *githubRunForge) openPR(ctx context.Context, repo, branch string) (int, string, error) {
	if branch == "" {
		return 0, "", errors.New("run forge: empty branch")
	}
	owner, _, err := splitSlug(repo)
	if err != nil {
		return 0, "", err
	}
	target, err := f.repoURL(repo, "/pulls?state=open&head=%s", url.QueryEscape(owner+":"+branch))
	if err != nil {
		return 0, "", err
	}
	raw, _, err := f.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &prs); err != nil {
		return 0, "", fmt.Errorf("run forge: decode pulls: %w", err)
	}
	for _, pr := range prs {
		if pr.Number != 0 {
			return pr.Number, pr.HTMLURL, nil
		}
	}
	return 0, "", nil
}

func (f *githubRunForge) PRForBranch(ctx context.Context, repo, branch string) (string, error) {
	_, u, err := f.openPR(ctx, repo, branch)
	return u, err
}

func (f *githubRunForge) UpdatePRBody(ctx context.Context, repo, branch, body string) error {
	n, _, err := f.openPR(ctx, repo, branch)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("run forge: no open PR for branch %s", branch)
	}
	target, err := f.repoURL(repo, "/pulls/%d", n)
	if err != nil {
		return err
	}
	_, _, err = f.do(ctx, http.MethodPatch, target, map[string]string{"body": body})
	return err
}

func (f *githubRunForge) LabelPR(ctx context.Context, repo, branch, label string) error {
	if label == "" {
		return errors.New("run forge: empty label")
	}
	n, _, err := f.openPR(ctx, repo, branch)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("run forge: no open PR for branch %s", branch)
	}
	target, err := f.repoURL(repo, "/issues/%d/labels", n)
	if err != nil {
		return err
	}
	_, _, err = f.do(ctx, http.MethodPost, target, map[string][]string{"labels": {label}})
	return err
}

func (f *githubRunForge) FetchIssue(ctx context.Context, repo string, issue int32) (IssueDetail, error) {
	target, err := f.repoURL(repo, "/issues/%d", issue)
	if err != nil {
		return IssueDetail{}, err
	}
	raw, _, err := f.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return IssueDetail{}, err
	}
	var out struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return IssueDetail{}, fmt.Errorf("run forge: decode issue: %w", err)
	}
	d := IssueDetail{Title: out.Title, Body: out.Body}
	for _, l := range out.Labels {
		d.Labels = append(d.Labels, l.Name)
	}
	if d.Comments, err = f.comments(ctx, repo, issue, time.Time{}); err != nil {
		return IssueDetail{}, err
	}
	return d, nil
}
