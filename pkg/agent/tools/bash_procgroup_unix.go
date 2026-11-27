//go:build !windows

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
	"syscall"
)

// setProcessGroup starts the shell as the leader of its own process group,
// so killProcessGroup can reach every descendant.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the entire process group leader-ed
// by p. Required because Cmd.Wait blocks on `awaitGoroutines` until
// io.Copy sees EOF on the inherited stdout/stderr pipes, and those
// pipes do not close until every descendant holding them exits. A
// backgrounded grandchild (sleep, docker daemon, kubectl proxy, find
// /, etc.) inherits the shell's pipes; killing only the shell leaves
// the pipes open. Negative pid in syscall.Kill addresses the process
// group, which (combined with Setpgid: true on the shell) catches the
// whole subtree.
//
// nil p means the process never started; nothing to kill.
//
// Any error is ignored at the call site -- this runs from Cmd.Cancel
// on context expiry, where the goal is best-effort cleanup. If
// signaling the group fails (ESRCH because the leader already exited
// cleanly, EPERM in some sandboxed containers), the WaitDelay timeout
// will still force pipe closure as a fallback.
func killProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	// Negative pid in syscall.Kill targets the process group with that
	// pgid. Combined with SysProcAttr.Setpgid: true on the leader,
	// pgid == leader's pid.
	return syscall.Kill(-p.Pid, syscall.SIGKILL)
}
