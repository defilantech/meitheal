//go:build windows

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

package tools

import (
	"os"
	"os/exec"
)

// setProcessGroup does nothing on Windows, which has no Unix process
// groups. The bash tool runs agent sandboxes on Linux and macOS; on
// Windows it only has to build, because the meitheal CLI links it.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup kills only the shell on Windows. A grandchild that
// keeps the output pipes open is still cut off by the command's
// WaitDelay.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
