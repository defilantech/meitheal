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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newForgeServer(t *testing.T, h http.HandlerFunc) (RunForge, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewGitHubRunForge(srv.URL, func() (string, error) { return "tok", nil }), srv
}

func TestRunForge_CommentIssue(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.Method+" "+r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	if err := f.CommentIssue(context.Background(), "o/r", 5, "hello"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /repos/o/r/issues/5/comments" || gotAuth != "Bearer tok" || !strings.Contains(gotBody, `"hello"`) {
		t.Fatalf("got %q %q %q", gotPath, gotAuth, gotBody)
	}
}

func TestRunForge_CommentsSince(t *testing.T) {
	var gotSince string
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since")
		_, _ = w.Write([]byte(`[
		 {"user":{"login":"a"},"body":"old","created_at":"2026-01-01T00:00:00Z"},
		 {"user":{"login":"Defilan"},"body":"/meitheal requeue","created_at":"2026-01-03T00:00:00Z"}]`))
	})
	since := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	cs, err := f.CommentsSince(context.Background(), "o/r", 5, since)
	if err != nil {
		t.Fatal(err)
	}
	if gotSince != "2026-01-02T00:00:00Z" {
		t.Fatalf("since = %q", gotSince)
	}
	if len(cs) != 1 || cs[0].Author != "Defilan" || cs[0].Body != "/meitheal requeue" {
		t.Fatalf("comments = %+v", cs)
	}
}

func TestRunForge_PRForBranch(t *testing.T) {
	var q map[string][]string
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" {
			t.Errorf("path %s", r.URL.Path)
		}
		q = r.URL.Query()
		if r.URL.Query().Get("head") == "o:none" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"number":9,"html_url":"https://github.com/o/r/pull/9"}]`))
	})
	u, err := f.PRForBranch(context.Background(), "o/r", "b")
	if err != nil || u != "https://github.com/o/r/pull/9" {
		t.Fatalf("got %q, %v", u, err)
	}
	if q["head"][0] != "o:b" || q["state"][0] != "open" {
		t.Fatalf("query %v", q)
	}
	u, err = f.PRForBranch(context.Background(), "o/r", "none")
	if err != nil || u != "" {
		t.Fatalf("no PR: got %q, %v", u, err)
	}
}

func TestRunForge_BranchExists(t *testing.T) {
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r":
			_, _ = w.Write([]byte(`{}`))
		case "/repos/o/r/git/ref/heads/b":
			_, _ = w.Write([]byte(`{}`))
		case "/repos/o/r/git/ref/heads/boom":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if ok, err := f.BranchExists(context.Background(), "o/r", "b"); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	if ok, err := f.BranchExists(context.Background(), "o/r", "gone"); err != nil || ok {
		t.Fatalf("404: %v %v", ok, err)
	}
	if ok, err := f.BranchExists(context.Background(), "o/r", "boom"); err == nil || ok {
		t.Fatalf("500 must error: %v %v", ok, err)
	}
}

func TestRunForge_OpenPRForIssue(t *testing.T) {
	cases := []struct {
		name, title, body, want string
	}{
		{"hash", "fix", "Closes #5.", "m"},
		{"not #50", "fix #50", "", ""},
		{"url", "x", "see https://github.com/o/r/issues/5", "m"},
		{"fixes", "Fixes #5", "", "m"},
		{"resolved lower", "x", "resolved #5", "m"},
		{"other issue url", "x", "https://github.com/o/r/issues/50", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/o/r/pulls" || r.URL.Query().Get("state") != "open" ||
					r.URL.Query().Get("per_page") != "100" {
					t.Errorf("bad request %s", r.URL)
				}
				b, _ := json.Marshal([]map[string]string{{"html_url": "m", "title": c.title, "body": c.body}})
				_, _ = w.Write(b)
			})
			u, err := f.OpenPRForIssue(context.Background(), "o/r", 5)
			if err != nil || u != c.want {
				t.Fatalf("got %q, %v want %q", u, err, c.want)
			}
		})
	}
}

func TestRunForge_OpenPRForIssue_SecondPage(t *testing.T) {
	var srvURL string
	f, srv := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"html_url":"p2","title":"x","body":"Fixes #5"}]`))
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?state=open&per_page=100&page=2>; rel="next"`, srvURL))
		_, _ = w.Write([]byte(`[{"html_url":"p1","title":"unrelated","body":""}]`))
	})
	srvURL = srv.URL
	u, err := f.OpenPRForIssue(context.Background(), "o/r", 5)
	if err != nil || u != "p2" {
		t.Fatalf("got %q, %v", u, err)
	}
}

func TestRunForge_OpenPRForIssue_PageCapAndForbidden(t *testing.T) {
	var srvURL string
	f, srv := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?state=open&per_page=100>; rel="next"`, srvURL))
		_, _ = w.Write([]byte(`[]`))
	})
	srvURL = srv.URL
	if u, err := f.OpenPRForIssue(context.Background(), "o/r", 5); err == nil || u != "" {
		t.Fatalf("page cap must error, got %q, %v", u, err)
	}
	g, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if u, err := g.OpenPRForIssue(context.Background(), "o/r", 5); err == nil || u != "" {
		t.Fatalf("403 must error, got %q, %v", u, err)
	}
}

func TestRunForge_BranchExists_AmbiguousNotFound(t *testing.T) {
	for _, tc := range []struct {
		repoStatus int
		wantErr    bool
	}{{http.StatusNotFound, true}, {http.StatusOK, false}} {
		f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/o/r" {
				w.WriteHeader(tc.repoStatus)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		ok, err := f.BranchExists(context.Background(), "o/r", "meitheal/wl/x")
		if ok || (err != nil) != tc.wantErr {
			t.Errorf("repo %d: got %v, %v", tc.repoStatus, ok, err)
		}
	}
}

func TestRunForge_EmptyTokenErrors(t *testing.T) {
	f := NewGitHubRunForge("http://127.0.0.1:1", func() (string, error) { return "", nil })
	if _, err := f.PRForBranch(context.Background(), "o/r", "b"); err == nil {
		t.Error("empty token must error")
	}
}

func TestRunForge_CommentAtSinceKept(t *testing.T) {
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"user":{"login":"a"},"body":"at","created_at":"2026-01-02T00:00:00Z"}]`))
	})
	cs, err := f.CommentsSince(context.Background(), "o/r", 5, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || len(cs) != 1 {
		t.Fatalf("got %+v, %v", cs, err)
	}
}

func TestRunForge_UpdatePRBodyAndLabelPR(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	bodies := map[string]string{}
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		key := r.Method + " " + r.URL.Path
		calls = append(calls, key)
		bodies[key] = string(b)
		mu.Unlock()
		switch key {
		case "GET /repos/o/r/pulls":
			_, _ = w.Write([]byte(`[{"number":9,"html_url":"u"}]`))
		case "PATCH /repos/o/r/pulls/9":
			_, _ = w.Write([]byte(`{}`))
		case "POST /repos/o/r/issues/9/labels":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected %s", key)
		}
	})
	ctx := context.Background()
	if err := f.UpdatePRBody(ctx, "o/r", "b", "new body"); err != nil {
		t.Fatal(err)
	}
	if err := f.LabelPR(ctx, "o/r", "b", "needs-decision"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies["PATCH /repos/o/r/pulls/9"], `"body":"new body"`) {
		t.Errorf("patch body %q", bodies["PATCH /repos/o/r/pulls/9"])
	}
	if !strings.Contains(bodies["POST /repos/o/r/issues/9/labels"], `["needs-decision"]`) {
		t.Errorf("label body %q", bodies["POST /repos/o/r/issues/9/labels"])
	}
}

func TestRunForge_PRMutationsErrorWithoutPR(t *testing.T) {
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must not mutate, got %s", r.Method)
		}
		_, _ = w.Write([]byte(`[]`))
	})
	if err := f.UpdatePRBody(context.Background(), "o/r", "b", "x"); err == nil {
		t.Error("UpdatePRBody with no PR must error")
	}
	if err := f.LabelPR(context.Background(), "o/r", "b", "x"); err == nil {
		t.Error("LabelPR with no PR must error")
	}
}

func TestRunForge_FetchIssue(t *testing.T) {
	var srvURL string
	f, srv := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/issues/5":
			_, _ = w.Write([]byte(`{"title":"T","body":"B","state":"closed","labels":[{"name":"gpu"},{"name":"bug"}]}`))
		case r.URL.Path == "/repos/o/r/issues/5/comments" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/5/comments?page=2>; rel="next"`, srvURL))
			_, _ = w.Write([]byte(`[{"user":{"login":"a"},"body":"one","created_at":"2026-01-01T00:00:00Z"}]`))
		case r.URL.Path == "/repos/o/r/issues/5/comments":
			_, _ = w.Write([]byte(`[{"user":{"login":"b"},"body":"two","created_at":"2026-01-02T00:00:00Z"}]`))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	srvURL = srv.URL
	d, err := f.FetchIssue(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "T" || d.Body != "B" || d.State != "closed" || len(d.Labels) != 2 || d.Labels[0] != "gpu" {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.Comments) != 2 || d.Comments[1].Body != "two" {
		t.Fatalf("comments (pagination) = %+v", d.Comments)
	}
}

func TestRunForge_PaginationCapAndForeignHost(t *testing.T) {
	var srvURL string
	f, srv := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/5/comments>; rel="next"`, srvURL))
		_, _ = w.Write([]byte(`[]`))
	})
	srvURL = srv.URL
	if _, err := f.CommentsSince(context.Background(), "o/r", 5, time.Time{}); err == nil {
		t.Error("endless pagination must hit the cap and error")
	}
	g, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<http://evil.example/x>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := g.CommentsSince(context.Background(), "o/r", 5, time.Time{}); err == nil {
		t.Error("a next link on another host must be refused")
	}
}

func TestRunForge_TokenResolvedPerRequest(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	toks := []string{"t1", "t2"}
	i := 0
	f := NewGitHubRunForge(srv.URL, func() (string, error) { t := toks[i]; i++; return t, nil })
	for range toks {
		if _, err := f.PRForBranch(context.Background(), "o/r", "b"); err != nil {
			t.Fatal(err)
		}
	}
	if len(auths) != 2 || auths[0] != "Bearer t1" || auths[1] != "Bearer t2" {
		t.Fatalf("auths = %v", auths)
	}
}

func TestRunForge_ErrorsFailClosed(t *testing.T) {
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom tok`))
	})
	ctx := context.Background()
	u, err := f.OpenPRForIssue(ctx, "o/r", 5)
	if err == nil || u != "" {
		t.Fatalf("500 must error: %q %v", u, err)
	}
	if strings.Contains(err.Error(), "tok") {
		t.Errorf("token leaked: %v", err)
	}
	if _, err := f.PRForBranch(ctx, "o/r", "b"); err == nil {
		t.Error("PRForBranch 500 must error")
	}
	if _, err := f.FetchIssue(ctx, "o/r", 5); err == nil {
		t.Error("FetchIssue 500 must error")
	}
}

func TestRunForge_BadSlugAndTokenErrors(t *testing.T) {
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a bad slug must not reach the network: %s", r.URL)
	})
	ctx := context.Background()
	for _, slug := range []string{"", "nope", "a b/c", "../x"} {
		if _, err := f.OpenPRForIssue(ctx, slug, 1); err == nil {
			t.Errorf("OpenPRForIssue(%q) must error", slug)
		}
		if _, err := f.BranchExists(ctx, slug, "b"); err == nil {
			t.Errorf("BranchExists(%q) must error", slug)
		}
		if _, err := f.PRForBranch(ctx, slug, "b"); err == nil {
			t.Errorf("PRForBranch(%q) must error", slug)
		}
		if _, err := f.FetchIssue(ctx, slug, 1); err == nil {
			t.Errorf("FetchIssue(%q) must error", slug)
		}
		if err := f.CommentIssue(ctx, slug, 1, "x"); err == nil {
			t.Errorf("CommentIssue(%q) must error", slug)
		}
	}
	bad := NewGitHubRunForge("http://127.0.0.1:1", func() (string, error) { return "", fmt.Errorf("no token") })
	if _, err := bad.PRForBranch(ctx, "o/r", "b"); err == nil {
		t.Error("token error must surface")
	}
}

// ChangedFiles reads the compare API from the merge base, follows its
// pagination, lists a renamed file under both names, and fails closed: an
// error status, endless pages, and a list at the API's 300-file cap (which
// may be cut short) are all errors.
func TestRunForge_ChangedFiles(t *testing.T) {
	var srvURL, gotPath string
	f, srv := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`{"files":[{"filename":"src/b.rs"}]}`))
			return
		}
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/compare/x?page=2>; rel="next"`, srvURL))
		_, _ = w.Write([]byte(`{"files":[{"filename":"src/a.rs"},` +
			`{"filename":"crates/new/k.rs","previous_filename":"crates/gpu/k.rs"}]}`))
	})
	srvURL = srv.URL
	files, err := f.ChangedFiles(context.Background(), "o/r", "main", "meitheal/wl-5/issue-5")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/a.rs", "crates/new/k.rs", "crates/gpu/k.rs", "src/b.rs"}; strings.Join(files, ",") !=
		strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", files, want)
	}
	if want := "/repos/o/r/compare/main...meitheal/wl-5/issue-5"; gotPath != want {
		t.Errorf("first request path = %q, want %q", gotPath, want)
	}

	down, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if files, err := down.ChangedFiles(context.Background(), "o/r", "main", "b"); err == nil || files != nil {
		t.Errorf("404 compare = %v, %v; want an error", files, err)
	}
	var endlessURL string
	endless, esrv := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/compare/x>; rel="next"`, endlessURL))
		_, _ = w.Write([]byte(`{"files":[]}`))
	})
	endlessURL = esrv.URL
	if files, err := endless.ChangedFiles(context.Background(), "o/r", "main", "b"); err == nil || files != nil {
		t.Errorf("endless pagination = %v, %v; want the page cap error", files, err)
	}
	full, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString(`{"files":[`)
		for i := range 300 {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"filename":"f%d"}`, i)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	})
	if files, err := full.ChangedFiles(context.Background(), "o/r", "main", "b"); err == nil || files != nil {
		t.Errorf("300 files = %d files, %v; want an error (the list may be cut short)", len(files), err)
	}
	if _, err := f.ChangedFiles(context.Background(), "o/r", "", "b"); err == nil {
		t.Error("empty base must error")
	}
}

// UnlabelPR deletes the label from the branch's open PR; a 404 (the PR does
// not carry it) is success, any other failure is not, and no open PR is an
// error.
func TestRunForge_UnlabelPR(t *testing.T) {
	for _, tc := range []struct {
		status  int
		wantErr bool
	}{{http.StatusOK, false}, {http.StatusNotFound, false}, {http.StatusForbidden, true}} {
		var deleted string
		f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.Method + " " + r.URL.Path {
			case "GET /repos/o/r/pulls":
				_, _ = w.Write([]byte(`[{"number":9,"html_url":"u"}]`))
			case "DELETE /repos/o/r/issues/9/labels/meitheal:parked":
				deleted = r.URL.Path
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`[]`))
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
		})
		err := f.UnlabelPR(context.Background(), "o/r", "b", "meitheal:parked")
		if (err != nil) != tc.wantErr || deleted == "" {
			t.Errorf("status %d: err = %v, deleted %q; want error %v", tc.status, err, deleted, tc.wantErr)
		}
	}
	none, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("must not mutate, got %s", r.Method)
		}
		_, _ = w.Write([]byte(`[]`))
	})
	if err := none.UnlabelPR(context.Background(), "o/r", "b", "meitheal:parked"); err == nil {
		t.Error("UnlabelPR with no PR must error")
	}
}

// An issue the API returns without a state is an unanswered query, not an
// open issue.
func TestRunForge_FetchIssueWithoutStateErrors(t *testing.T) {
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"title":"T","body":"B"}`))
	})
	if _, err := f.FetchIssue(context.Background(), "o/r", 5); err == nil {
		t.Fatal("want an error for an issue without a state")
	}
}
