# AGENTS.md

Meitheal is a Kubernetes-native control plane for agentic coding workloads:
it runs coder, verifier and reviewer agents as AgenticTasks across a fleet of
nodes (FleetNodes), on models served by LLMKube InferenceServices or any
OpenAI-compatible endpoint. Go 1.27, built with controller-runtime /
Kubebuilder.

This file is the source of truth for the engineering conventions that agents and
contributors share. The human onboarding narrative lives in
[CONTRIBUTING.md](CONTRIBUTING.md).

Meitheal started as Foreman inside defilantech/LLMKube and moved here with its
history. Issue and pull request numbers in older commits and code comments
(`#1491`, `(#620)` and so on) refer to defilantech/LLMKube.

## Contents

- [Setup](#setup)
- [Commands](#commands)
- [Definition of done](#definition-of-done)
- [Code style](#code-style)
- [Testing](#testing)
- [Commits](#commits)
- [Branches and pull requests](#branches-and-pull-requests)
- [Project layout](#project-layout)
- [Kept in sync](#kept-in-sync)
- [Security-sensitive invariants](#security-sensitive-invariants)
- [Do not](#do-not)

## Setup

- Go 1.27.4+ (pinned in `go.mod`).
- `make test` downloads the envtest binaries (Kubernetes 1.37) into `./bin`
  on first run; no cluster is needed for unit tests.
- `make test-chart` needs `helm` and the `helm-unittest` plugin.
- `make docker-build` needs Docker.
- The kind e2e test runs in CI (`.github/workflows/e2e.yml`); it builds the
  images, installs the chart with a stub-mode agent and runs one AgenticTask
  to `Succeeded`.

## Commands

| Task                                     | Command                       |
|------------------------------------------|-------------------------------|
| Build the CLI, operator and agent        | `make build`                  |
| Build the agent with version ldflags     | `make build-agent-versioned`  |
| Unit tests (envtest, all packages)       | `make test`                   |
| envtest suites only (second seed)        | `make test-envtest`           |
| Shell script tests                       | `make test-scripts`           |
| Helm chart lint + unit tests             | `make test-chart`             |
| Format                                   | `make fmt`                    |
| Vet                                      | `make vet`                    |
| Lint (golangci-lint)                     | `make lint`                   |
| Lint under `GOOS=darwin` and `GOOS=linux` | `make lint-all`              |
| Lint and fix                             | `make lint-fix`               |
| Verify the lint configuration            | `make lint-config`            |
| Regenerate CRDs / RBAC / webhooks        | `make manifests`              |
| Regenerate DeepCopy methods              | `make generate`               |
| Regenerate CRDs into the Helm chart      | `make chart-crds`             |
| Sync / check the reviewer prompts        | `make sync-reviewer-prompts` / `make check-reviewer-prompts` |
| Operator and agent images                | `make docker-build`           |
| Coder Job image                          | `make docker-build-agent-coder` |
| Third-party license tree                 | `make third-party-licenses`   |
| Install / remove the macOS agent         | `make install-agent` / `make uninstall-agent` |

## Definition of done

A change is not done until all of these hold.

1. The pre-commit gate passes: `make fmt`, `make vet`, `make lint`, `make test`.
2. The pre-push gate passes: `make lint-all`. If `charts/` changed, run
   `make test-chart`. If CRD types in `api/v1alpha1/` changed, run
   `make manifests`, `make generate` and `make chart-crds`, then `git status`
   must be clean. Uncommitted generated files mean a step was skipped, and
   the CI CRD sync check will fail.
3. Every behavior change has a test that fails without it. Before adding an
   assertion, name the production edit that would make it fail. If you cannot,
   the assertion does not bite. Paste the failure you saw, not a paraphrase.
4. The tests assert on side effects, not return values. A controller test must
   check the world changed the way the spec requires (an object exists, a file
   is on disk, a condition carries the cause), not merely that the code
   returned what it returns.
5. You can name every consumer of anything you touched (a resource, an
   endpoint, a port, a wire payload, a command-line contract) and what must stay
   true for it. A change that fixes one path while silently breaking a consumer
   is not done.
6. The PR description matches the code. No claim in it that a test or the diff
   does not back.

## Code style

- Match the surrounding code: naming, error handling, comment density, layout.
  Do not introduce a different style for new code.
- Idiomatic Go: `gofmt`, wrapped errors (`fmt.Errorf("...: %w", err)`), table-driven tests.
- A new Go file takes its header from `hack/boilerplate.go.txt`, not from a
  neighboring file.
- Comments explain *why*, not *what*. Do not add docstrings or decorative
  comments to hit a quota.
- Controller code follows controller-runtime conventions: reconcilers are
  idempotent, garbage collection uses owner references, state is surfaced
  through status conditions.

## Testing

- Every behavior change needs a test. A bug fix gets a regression test that
  fails before the fix and passes after.
- Unit tests use `envtest` (controller-runtime); they run with `make test` and
  need no live cluster. The controller specs in `internal/controller` run
  under one Ginkgo suite (`suite_test.go`) that starts the control plane once.
  The suite reads `GINKGO_SEED`, so `make test GINKGO_SEED=<n>` replays a
  failing spec order; CI runs `make test-envtest` again under a second seed.
- Darwin-only code and tests (`_darwin.go`, `_darwin_test.go`, for example
  `pkg/agent/capability_darwin.go`) build and run only with `GOOS=darwin`.
  `make lint-all` type-checks them on any host; run their tests on a Mac when
  you change them.
- Assert observable behavior, not internal implementation detail.
- envtest runs as cluster-admin, so it cannot catch a missing
  `+kubebuilder:rbac` marker, and nothing generates the chart's Roles. After a
  change that adds an API call (a new resource, verb, or API group), add the
  marker, run `make manifests`, and add the same rule by hand to the chart
  template for the identity that makes the call:
  `charts/meitheal/templates/operator-rbac.yaml`, `agent-rbac.yaml` or
  `coder-rbac.yaml`.
- Do not weaken or delete a test to make a change pass. If a test is genuinely
  wrong, fix it and say why.
- Do not test a contract against a double built from the consumer's own type. A
  stub that constructs its payload with the same type the code decodes pins
  nothing: a rename of a wire field leaves it green. Feed the real producer's
  bytes, or assert both sides of the contract.

## Commits

This repo uses conventional commit prefixes so `release-please` can generate
changelogs and version bumps. Every commit needs one:

| Prefix      | Use for                                   | Version bump |
|-------------|-------------------------------------------|--------------|
| `feat:`     | New feature, CRD field, CLI command       | minor        |
| `fix:`      | Bug fix, correctness improvement          | patch        |
| `perf:`     | Performance improvement                   | patch        |
| `docs:`     | Documentation only                        | patch        |
| `chore:`    | Deps, CI, tooling (hidden from changelog) | none         |
| `test:`     | Test-only change (hidden)                 | none         |
| `refactor:` | Refactor, no behavior change (hidden)     | none         |

Use `feat!:` / `fix!:` for breaking changes.

- Human contributors sign off every commit: `git commit -s`. A human is
  accountable for every commit, however it was produced (DCO is enforced by
  CI; bot-only sign-offs are not accepted).
- Work authored by `defilan-agent[bot]`, the project's GitHub App, carries no
  `Signed-off-by` line; the agent never signs off its own commits. Before
  approving an agent PR, a maintainer adds the sign-off to the PR branch with
  the `signoff-pr` helper: a `git rebase --signoff` followed by a
  `--force-with-lease` push.
- Subject says *what* changed; body says *why*. No implementation play-by-play.
- Keep commit messages conventional and free of attribution trailers
  (`Co-Authored-By`, `AI-Agent`, `Assisted-by`, etc.). Disclose AI assistance
  with the `Assisted-by:` line in the PR description instead, per
  `CONTRIBUTING.md` ("AI-Assisted and Agent Contributions").
- One logical change per commit.

## Branches and pull requests

- Human contributions go through fork-based PRs. Branch from an up-to-date
  `main`.
- Branch names: `feat/<slug>`, `fix/<slug>`, `docs/<slug>`, `refactor/<slug>`,
  `chore/<slug>`.
- Work an agent does for the maintainers is the exception: it is authored by
  `defilan-agent[bot]`, the project's GitHub App, on `agent/<topic>` branches
  in `defilantech/meitheal` itself, and the bot pushes there, never to a fork.
- PRs follow `.github/PULL_REQUEST_TEMPLATE.md` (What / Why / How / Checklist)
  and reference the issue with `Fixes #N`.
- Do not push to `main`. Do not force-push shared branches; the one exception
  is a maintainer adding the DCO sign-off to an agent PR branch with
  `signoff-pr` (see Commits).

## Project layout

| Path                       | Contents                                          |
|----------------------------|---------------------------------------------------|
| `api/v1alpha1/`            | `meitheal.dev` CRD types: Agent, AgenticTask, AgentRelease, FleetNode, ModelProfile, Workload (regenerate after editing) |
| `cmd/meitheal/`            | The `meitheal` CLI entry point                    |
| `cmd/meitheal-operator/`   | The operator (controllers and webhooks)           |
| `cmd/meitheal-agent/`      | The per-node agent (claims and runs tasks)        |
| `cmd/hunkcheck/`           | Per-hunk mutation coverage pass the gate Job runs |
| `cmd/sync-reviewer-prompts/` | Syncs `config/system-prompts/reviewer.md` into the reviewer Agents |
| `cmd/machoplist/`          | Embeds the Info.plist in darwin release binaries  |
| `internal/controller/`     | Workload, AgenticTask, FleetNode, Agent, AgentRelease reconcilers |
| `internal/webhook/`        | Agent and AgenticTask validating webhooks         |
| `internal/metrics/`        | Prometheus metrics (`meitheal_*`)                 |
| `internal/machoplist/`     | Mach-O Info.plist embedding and release identity tests |
| `pkg/agent/`               | Agent core: executors, gates, tools, review       |
| `pkg/audit/`, `pkg/archive/` | Run audit records and terminal-task archives    |
| `pkg/slicer/`              | Cross-slice drift check for sliced changes        |
| `pkg/cli/`                 | Cobra CLI commands                                |
| `pkg/selfupdate/`, `pkg/datadir/` | Agent self-update and its install root     |
| `config/`                  | Generated CRDs, RBAC and webhooks; sample Agents and system prompts |
| `charts/meitheal/`         | Helm chart (CRDs synced via `make chart-crds`)    |
| `deployment/`              | macOS launchd plist and Linux systemd unit for the agent |
| `docs/`                    | User docs; `docs/proposals/` holds design history |
| `examples/`                | Sample manifests                                  |
| `hack/`                    | Boilerplate and release helper scripts            |
| `scripts/`                 | CRD sync and helper scripts with their tests      |

## Kept in sync

Some files must change together; a test or check fails when they drift.

- `config/system-prompts/reviewer.md` and the `spec.systemPrompt` of every
  `config/agents/*-reviewer.yaml`: regenerate with
  `make sync-reviewer-prompts`, check with `make check-reviewer-prompts`.
- CRD types and their generated output: `make manifests generate chart-crds`
  rewrites `config/crd/bases`, `config/rbac`, `config/webhook`,
  `api/v1alpha1/zz_generated.deepcopy.go` and
  `charts/meitheal/templates/crds`.
- The scan gate's built-in targets (`pkg/agent/tools/scan_targets.go`) mirror
  the target repository's release scan script. A copy of that script and its
  Dockerfiles lives in `pkg/agent/tools/testdata/llmkube`, and
  `scan_targets_parity_test.go` compares the two; refresh both together.

## Security-sensitive invariants

- **Enforcement code is adversarial.** Anything between a request and a
  decision (an allow, a verdict, a dispatch) is driven by input someone else
  controls: task payloads, model output, repository contents. For every guard,
  fallback, default, or early return, name the input that reaches it and
  whether that input is controlled by someone other than the operator. A guard
  whose fallback an untrusted input can trigger needs a test at the path that
  input takes, not only a unit case for the helper.
- **Contracts that cross a process boundary are duplicated.** The agent and
  the operator, the agent and the Jobs it submits, and the gate Job template
  and the code that parses its log are separate processes that agree on a
  format. Test them with the real producer's bytes or type, never with a
  double built from the consumer side.
- **A ClusterIP is not a network boundary.** Any new Service port, endpoint, or
  admin surface is reachable by every pod in the cluster once it exists. State
  the default posture in the change, and scope it (NetworkPolicy, an internal
  Service, auth) or say why not.
- **A privileged syscall in a container needs runtime reasoning, not just a
  rendered-spec test.** Do not run an init/main container that performs a
  privileged syscall (`chown`, `chmod` of an unowned file, `mount`, etc.)
  non-root with `capabilities.add` when the syscall runs in an exec'd child
  (`command: ["sh", "-c", "chown ..."]`). A non-root process loses its
  capabilities across `execve` unless the runtime sets ambient capabilities,
  which is runtime-dependent (it works on kind but not on every cluster, so
  envtest and CI will pass while real users hit `EPERM`). Either run the
  container as root (uid 0, still `Drop: [ALL]` + the minimal caps + no
  privilege escalation), or do the syscall directly in the container's
  entrypoint (a small helper binary) so the caps are not cleared.

## Do not

- Do not hand-edit generated files (`zz_generated.*`, CRD YAML under `config/`
  and `charts/`). Change the source and regenerate.
- Do not edit `bin/`; it holds local build and tool artifacts, not source.
- Do not commit secrets, kubeconfigs, or `.env` files.
- Do not skip the pre-commit checks or the CRD sync step.
