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
	"reflect"
	"strings"
	"testing"
)

// A malformed Secret name is a typo on the command line: refused by flag name
// before the template, the token or the cluster is looked at.
func TestRunCommand_RefusesABadGateImagePullSecret(t *testing.T) {
	queue, _ := runQueueFixture(t)
	_, err := meithealExec(t, "run", "--queue", queue, "--coder-agent", "coder-metal",
		"--gate-image-pull-secrets", "registry-cred,Bad_Name")
	if err == nil || !strings.Contains(err.Error(), "--gate-image-pull-secrets") ||
		!strings.Contains(err.Error(), "Bad_Name") {
		t.Errorf("err = %v, want the bad name refused by flag name", err)
	}
}

// The run loop's own gate Jobs pull with the secrets it was given (#46).
func TestRunGateToolConfig(t *testing.T) {
	opts := &runOptions{
		namespace:           "work",
		gateHostPaths:       "/srv/models",
		gatePriorityClasses: "batch",
		gateDeadline:        900,
	}
	cfg := runGateToolConfig(opts, []string{"registry-cred"}, nil)
	if cfg.Namespace != "work" || cfg.ActiveDeadlineSeconds != 900 {
		t.Errorf("namespace/deadline = %q/%d, want work/900", cfg.Namespace, cfg.ActiveDeadlineSeconds)
	}
	if !reflect.DeepEqual(cfg.AllowedHostPaths, []string{"/srv/models"}) ||
		!reflect.DeepEqual(cfg.AllowedPriorityClasses, []string{"batch"}) {
		t.Errorf("allowlists = %v / %v", cfg.AllowedHostPaths, cfg.AllowedPriorityClasses)
	}
	if want := []string{"registry-cred"}; !reflect.DeepEqual(cfg.ImagePullSecrets, want) {
		t.Errorf("ImagePullSecrets = %v, want %v", cfg.ImagePullSecrets, want)
	}
}
