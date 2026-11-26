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

// NewRootCommand creates the root command for the meitheal CLI: operate the
// Meitheal agentic-coding harness. The commands that were `llmkube foreman
// <command>` and `llmkube audit` in LLMKube sit directly under it, with no
// group level in between.
func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meitheal",
		Short: "Operate the Meitheal agentic-coding harness",
		Long: `Operate the Meitheal agentic-coding harness.

Meitheal runs coder and reviewer agents as AgenticTasks against a fleet of
heterogeneous executors. These subcommands create and watch those tasks.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newDispatchCommand())
	cmd.AddCommand(newSliceCommand())
	cmd.AddCommand(newRunCommand())
	cmd.AddCommand(newDecisionsCommand())
	cmd.AddCommand(NewAuditCommand())
	cmd.AddCommand(newGitHubAppTokenCommand())
	cmd.AddCommand(newVersionCommand())
	return cmd
}
