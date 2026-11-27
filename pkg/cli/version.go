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
	"github.com/spf13/cobra"
)

var (
	// Version is set during build via ldflags or updated by release-please
	Version = "0.1.1" // x-release-please-version
	// GitCommit is set during build
	GitCommit = "unknown"
	// BuildDate is set during build
	BuildDate = "unknown"
)

// newVersionCommand prints the build's version information.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  `Display version, git commit, and build date information.`,
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			out := cmd.OutOrStdout()
			fprintf(out, "meitheal version %s\n", Version)
			fprintf(out, "  git commit: %s\n", GitCommit)
			fprintf(out, "  build date: %s\n", BuildDate)
		},
	}
}
