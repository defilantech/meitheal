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
	f, _ := newForgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Errorf("path %s", r.URL.Path)
		}
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "repo:o/r") || !strings.Contains(q, "is:pr") || !strings.Contains(q, "is:open") {
			t.Errorf("q = %q", q)
		}
		_, _ = w.Write([]byte(`{"items":[
		 {"html_url":"https://github.com/o/r/pull/50","title":"fix #50","body":""},
		 {"html_url":"https://github.com/o/r/pull/9","title":"fix","body":"Closes #5."}]}`))
	})
	u, err := f.OpenPRForIssue(context.Background(), "o/r", 5)
	if err != nil || u != "https://github.com/o/r/pull/9" {
		t.Fatalf("got %q, %v (must skip #50)", u, err)
	}
	if u, err = f.OpenPRForIssue(context.Background(), "o/r", 7); err != nil || u != "" {
		t.Fatalf("none: %q %v", u, err)
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
			_, _ = w.Write([]byte(`{"title":"T","body":"B","labels":[{"name":"gpu"},{"name":"bug"}]}`))
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
	if d.Title != "T" || d.Body != "B" || len(d.Labels) != 2 || d.Labels[0] != "gpu" {
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
	_ = json.Marshal
}
