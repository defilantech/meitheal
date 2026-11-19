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
	"strings"
	"testing"
)

// The root carries every command that moved here from LLMKube directly, with
// no group level in between.
func TestRootCommand_RegistersMovedCommands(t *testing.T) {
	root := NewRootCommand()
	if root.Name() != "meitheal" {
		t.Fatalf("root command is %q, want meitheal", root.Name())
	}
	paths := [][]string{
		{"dispatch"},
		{"slice"},
		{"run"},
		{"decisions"},
		{"decisions", "answer"},
		{"audit"},
		{"audit", "export"},
		{"version"},
	}
	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			c, _, err := root.Find(path)
			if err != nil {
				t.Fatalf("meitheal %s: %v", strings.Join(path, " "), err)
			}
			if want := path[len(path)-1]; c.Name() != want {
				t.Errorf("meitheal %s resolved to %q, want %q", strings.Join(path, " "), c.Name(), want)
			}
		})
	}
}

func TestVersionCommand_PrintsVersion(t *testing.T) {
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version: %v", err)
	}
	if want := "meitheal version " + Version + "\n"; !strings.HasPrefix(out.String(), want) {
		t.Errorf("version output = %q, want prefix %q", out.String(), want)
	}
}
