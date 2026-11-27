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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/defilantech/meitheal/pkg/agent/repo"
)

// A reviewer opens its PR long after the clone: a token rotated on disk in
// between must be the one authToken hands the code host, and a token that can
// no longer be resolved is an error, not an empty token.
func TestAuthToken_DynamicAuthGivesRotatedToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GITHUB_TOKEN", "")
	dir := filepath.Join(home, ".config", "meitheal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "github-token")
	if err := os.WriteFile(path, []byte("before-rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := repo.NewAuth("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = auth.Close() }()
	if err := os.WriteFile(path, []byte("after-rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := authToken(auth); err != nil || got != "after-rotation" {
		t.Fatalf("authToken = %q, %v; want the rotated token", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := authToken(auth); err == nil {
		t.Fatalf("authToken with no token on disk = %q, want an error", got)
	}
	if got, err := authToken(nil); err != nil || got != "" {
		t.Fatalf("authToken(nil) = %q, %v; want empty and no error", got, err)
	}
}
