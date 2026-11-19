# Contributing to Meitheal

Thank you for your interest in contributing to Meitheal. Meitheal runs
coder, verifier and reviewer agents as Kubernetes workloads on local models,
and turns their work into pull requests against your repositories.

Meitheal started as Foreman inside defilantech/LLMKube and moved to this
repository with its history. Issue and pull request numbers in older commits
and code comments refer to defilantech/LLMKube.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [How Can I Contribute?](#how-can-i-contribute)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [AI-Assisted and Agent Contributions](#ai-assisted-and-agent-contributions)
- [Pull Request Process](#pull-request-process)
- [Coding Standards](#coding-standards)
- [Community](#community)

## Code of Conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md). Please be
respectful and constructive in all interactions.

**Expected Behavior:**
- Use welcoming and inclusive language
- Be respectful of differing viewpoints and experiences
- Gracefully accept constructive criticism
- Focus on what is best for the community
- Show empathy towards other community members

**Unacceptable Behavior:**
- Harassment, discriminatory language, or personal attacks
- Trolling, insulting/derogatory comments
- Public or private harassment
- Publishing others' private information without permission

## How Can I Contribute?

### Reporting Bugs

Before creating bug reports, please check existing issues to avoid duplicates.

**Good bug reports include:**
- Clear, descriptive title
- Exact steps to reproduce the problem
- Expected vs actual behavior
- Meitheal version (`meitheal version`, or the chart and image tags you run)
- Kubernetes version (`kubectl version`)
- Where the agent runs (in-cluster, or a Linux or macOS host) and what serves
  the model (an LLMKube InferenceService or an OpenAI-compatible endpoint)
- Relevant logs from the meitheal-operator and meitheal-agent
- The Agent, AgenticTask or Workload YAML involved (with secrets removed)

**Use the bug report template** when creating issues.

### Suggesting Features

We track feature requests via GitHub Issues with the `enhancement` label.

**Good feature requests include:**
- Clear use case and problem statement
- Proposed solution (if you have one)
- Alternatives you've considered
- Impact on existing functionality
- Examples of similar features in other projects

### First-Time Contributors

Look for issues labeled:
- `good-first-issue` - Small, well-defined tasks
- `help-wanted` - Larger tasks where we need help
- `documentation` - Documentation improvements

## Development Setup

### Prerequisites

**Required:**
- **Go 1.27.4+** (the version `go.mod` pins): install from [golang.org](https://golang.org/dl/)
- **Docker**: for building images
- **kubectl** and a Kubernetes cluster (kind is enough) for running Meitheal
- **helm** and the `helm-unittest` plugin, for `make test-chart`

**Optional:**
- **LLMKube**, when your Agents use LLMKube InferenceServices. Agents can also
  use any OpenAI-compatible endpoint.

### Clone and Build

```bash
# Fork the repository on GitHub first, then:
git clone git@github.com:YOUR_USERNAME/meitheal.git
cd meitheal

# Install dependencies
go mod download

# Generate CRDs, RBAC and webhook manifests
make manifests

# Build bin/meitheal, bin/meitheal-operator and bin/meitheal-agent
make build

# Run tests (downloads envtest binaries into ./bin on first run)
make test
```

### Running in a Cluster

```bash
# Build the operator and agent images, then push them where your cluster
# can pull them (a kind cluster can use `kind load docker-image` instead)
make docker-build \
  MEITHEAL_OPERATOR_IMG=<registry>/meitheal-operator:dev \
  MEITHEAL_AGENT_IMG=<registry>/meitheal-agent:dev

# Install the chart from your checkout with those images
helm upgrade --install meitheal charts/meitheal \
  --namespace meitheal-system --create-namespace \
  --set operator.image.repository=<registry>/meitheal-operator \
  --set operator.image.tag=dev \
  --set agent.image.repository=<registry>/meitheal-agent \
  --set agent.image.tag=dev

# Check the logs
kubectl logs -n meitheal-system deploy/meitheal-operator -f
kubectl logs -n meitheal-system deploy/meitheal-agent -f
```

`.github/workflows/e2e.yml` is a worked example: it builds both images, loads
them into a kind cluster, installs the chart with a stub-mode agent and runs an
AgenticTask to `Succeeded`.

## Making Changes

### Branching Strategy

- `main` - Stable, production-ready code
- `feat/*` - New features
- `fix/*` - Bug fixes
- `docs/*` - Documentation changes
- `refactor/*` - Code refactoring, no behavior change
- `chore/*` - Deps, CI, tooling

The branch-name and commit conventions are normative in
[AGENTS.md](AGENTS.md); this section is a summary.

**Example:**
```bash
git checkout -b feat/workload-retry-budget
```

### Commit Messages

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <subject>

<body>

<footer>
```

The full prefix table, including `perf`, `test`, and the breaking-change `!`
form, is normative in [AGENTS.md](AGENTS.md).

**Examples:**
```
feat(controller): surface the gate verdict on the Workload status

Adds a condition that carries the last GATE-PASS or GATE-FAIL so a
Workload shows why it stopped without reading the AgenticTasks.

Closes #42
```

```
fix(agent): retry a transient clone failure once

A single network error during clone failed the whole task. The
agent now retries once before reporting the failure.

Fixes #55
```

### Code Changes

1. **Write tests** for new functionality
2. **Update documentation** (README, docs/, code comments)
3. **Run linters**: `make lint`
4. **Run tests**: `make test`
5. **Test manually** in a real cluster

### Testing Guidelines

```bash
# Unit and envtest suites
make test

# The envtest suites again under a second seed
make test-envtest

# Helm chart lint and unit tests
make test-chart

# Lint, and lint under both GOOS values the binaries ship for
make lint
make lint-all

# Verify generated manifests are up to date
make manifests generate chart-crds
git diff --exit-code  # Should be no changes
```

**Writing Tests:**
- Use table-driven tests for multiple cases
- Test both success and error paths
- Mock external dependencies (Kubernetes API, model endpoints, GitHub)
- Test CRD validation and defaulting
- Verify metrics are properly exported
- **Assert on side effects, not return values, and make every new assertion
  bite.** The rule, and how to prove an assertion bites, are in
  [AGENTS.md](AGENTS.md) under "Definition of done".

## AI-Assisted and Agent Contributions

Meitheal is an agentic coding harness, and the people who build it use AI
coding agents heavily. So we welcome AI-assisted contributions. We hold them
to one standard, the same one we hold our own agents to: human
accountability, not where the code came from.

If any part of your contribution (code, tests, commit messages, PR text) was
generated or substantially assisted by an AI tool, the following apply.

**A human is accountable.**

- A human signs off every commit with the DCO (`git commit -s`). The sign-off
  means a person takes responsibility for understanding, testing, and
  maintaining the change, however it was produced. Bot-only sign-offs are not
  accepted. On pull requests opened by the project's own agent, a maintainer
  adds the sign-off before approving; see "The maintainer-run agent workflow"
  below.
- A human, not an agent, owns the review conversation. Replies to review
  feedback come from you, not from an autonomous agent posting on your behalf.

**Disclose the assistance.**

- State in the PR description which tool was used, what it generated, and what
  you verified before submitting. One line is enough, for example:
  `Assisted-by: <tool> (generated the tests; I reviewed and ran make test)`.
- Put the disclosure in the PR body, not the commit message. We keep commit
  messages free of attribution trailers (see `AGENTS.md`).

**Same bar as any other PR.**

- Tests must exercise real behavior. A test that still passes when the code
  under test is broken is not coverage.
- The change must match the scope of the issue it claims to fix: no unrelated
  edits, no placeholder or fabricated content.
- Run the checks locally first: `make test`, `make lint` and `make lint-all`.

**Unsolicited and drive-by PRs.**

- We may close unsolicited AI-generated PRs that were not preceded by an issue
  or discussion, especially low-effort ones targeting `good-first-issue`
  labels. To work an issue, comment on it first so a human can confirm it is a
  good fit and not already in progress.

**The maintainer-run agent workflow.**

Work an agent does for the maintainers is authored by `defilan-agent[bot]`,
the project's GitHub App, and pushed to `agent/<topic>` branches in
`defilantech/meitheal` itself. Human contributors open pull requests from
their own forks and sign off their own commits with `git commit -s`. The
bot's commits carry no sign-off: the agent never adds a `Signed-off-by` line.
Before approving an agent PR, a human maintainer adds the DCO sign-off with
the `signoff-pr` helper (a `git rebase --signoff` followed by a
`--force-with-lease` push of the PR branch). Commit messages stay
conventional and free of attribution trailers, and the PR description keeps
the `Assisted-by:` disclosure line.

This is not an anti-AI policy. It is a "stand behind your work" policy. We
require that a person stay in the loop and on the hook.

## Pull Request Process

### Before Submitting

- [ ] Code passes all tests (`make test`)
- [ ] Linter passes (`make lint`)
- [ ] Cross-GOOS lint passes (`make lint-all`); it catches `//go:build`-tagged
  files, such as the agent's darwin-only code, that the host's default `GOOS`
  would silently skip
- [ ] Chart tests pass if `charts/` changed (`make test-chart`)
- [ ] Generated files are committed (`make manifests generate chart-crds`
  leaves no diff)
- [ ] Manually tested in a Kubernetes cluster
- [ ] Documentation updated (if adding features)
- [ ] Commit messages follow conventions
- [ ] Branch is up-to-date with `main`
- [ ] No hardcoded credentials or project IDs

### Submitting a PR

1. **Push your branch** to your fork
2. **Open a Pull Request** against `main`
3. **Fill out the PR template** completely
4. **Link related issues** (e.g., "Closes #42")
5. **Request review** from maintainers

### PR Title Format

**Important:** PR titles are used for automated changelog generation and
version bumps via [Release Please](https://github.com/googleapis/release-please).
Since we use **squash merging**, your PR title becomes the commit message on
`main`.

```
<type>: <description>
```

| Type | Description | Version Bump |
|------|-------------|--------------|
| `feat:` | New feature | Minor (0.x.0) |
| `fix:` | Bug fix | Patch (0.0.x) |
| `docs:` | Documentation only | None |
| `ci:` | CI/CD changes | None |
| `chore:` | Maintenance tasks | None |
| `refactor:` | Code refactoring | None |
| `test:` | Adding tests | None |

**Examples:**
- `feat: add a retry budget to Workloads`
- `fix: keep a FleetNode Ready across a missed heartbeat`
- `docs: add a troubleshooting section to the verifier runbook`
- `feat(helm): add image digest support for production deployments`

**Scopes** (optional): use parentheses to specify the area of change:
`feat(cli):`, `fix(controller):`, `docs(helm):`, etc.

### Review Process

1. **Automated checks** run (tests, linting, chart checks, DCO)
2. **Maintainer review** (usually within 2-3 days)
3. **Address feedback** by pushing new commits
4. **Approval and merge** (squash-merge to `main`)

**What reviewers look for:**
- Code quality and readability
- Test coverage
- Performance implications
- Backward compatibility
- Documentation completeness

## Coding Standards

### Go Code Style

- Follow [Effective Go](https://golang.org/doc/effective_go.html)
- Use `gofmt` and `golangci-lint`
- Keep functions small and focused
- Add comments for exported types/functions
- Use descriptive variable names

### CRD Design

- Follow [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- Use `+kubebuilder` markers for validation
- Provide meaningful status conditions
- Add working examples in `examples/`

### Documentation

- **Code comments**: Explain *why*, not *what*
- **README and docs/ updates**: For user-facing changes
- **Architecture docs**: For design decisions (`docs/proposals/`)
- **Examples**: Working YAML manifests

## Community

- **GitHub Issues**: Bug reports, feature requests
- **Pull requests**: Code, tests and docs

Contributors are listed in the release notes.

## Development Workflow Example

Here's a complete workflow for adding a field to the Agent API:

```bash
# 1. Create feature branch
git checkout -b feat/agent-turn-budget

# 2. Make changes
vim api/v1alpha1/agent_types.go
vim internal/controller/agent_validation.go

# 3. Regenerate CRDs, DeepCopy and the chart's CRDs
make manifests generate chart-crds

# 4. Write tests
vim internal/controller/agent_validation_test.go

# 5. Run tests and lint
make test
make lint-all
make test-chart

# 6. Update documentation
vim docs/README.md

# 7. Commit
git add .
git commit -s -m "feat(api): add a per-Agent turn budget

Caps the agent loop's turns per task so a runaway model stops
instead of burning the task deadline.

Closes #123"

# 8. Push and create PR
git push origin feat/agent-turn-budget
# Open PR on GitHub
```

## Questions?

If you're stuck or have questions:
- Check existing documentation (README, docs/, code comments)
- Search closed issues and PRs
- Ping maintainers in your PR

## License

By contributing to Meitheal, you agree that your contributions will be
licensed under the Apache License 2.0.
