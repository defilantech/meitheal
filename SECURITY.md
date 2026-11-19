# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |

## Reporting a Vulnerability

We take security seriously. If you discover a security vulnerability in
Meitheal, please report it responsibly.

### How to Report

1. **Do NOT open a public GitHub issue** for security vulnerabilities.
2. **Preferred:** use GitHub's [private vulnerability reporting](https://github.com/defilantech/meitheal/security/advisories/new). Reports stay private, are tracked in one place, and let us publish an advisory once a fix ships.
3. **Alternative:** email **contact@defilan.com**.

### What to Include

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

### Response Timeline

- **Acknowledgment**: Within 48 hours
- **Initial Assessment**: Within 7 days
- **Resolution Target**: Within 30 days for critical issues

### Scope

This security policy applies to:
- The meitheal-operator
- The meitheal-agent (in-cluster on Linux, and native on Linux and macOS hosts)
- The `meitheal` CLI
- The Helm chart
- Container images published to GHCR

### Out of Scope

- Third-party dependencies (report to upstream)
- LLM model vulnerabilities (report to model providers)
- The model servers Agents call, including LLMKube (report to those projects)

## Trust model

Meitheal turns custom resources into code changes made by a model with tools,
so write access to its resources is powerful. Grant it the way you would grant
access to the credentials and hosts the agents use.

- **AgenticTasks and Workloads.** Creating one makes an Agent clone a
  repository, run a model-driven loop whose tools include a shell in the task
  workspace, and push branches with the git credentials the meitheal-agent was
  given. In the default `InProcess` execution mode that loop runs in the
  meitheal-agent itself (its pod, or the Linux or macOS host it runs on
  natively); in `Job` mode it runs in a coder Job pod. A task names
  its Agent with `spec.agentRef`, which resolves in the task's own namespace.
- **Agents.** An Agent's `spec.execution.image` and
  `spec.execution.serviceAccountName` become the image and ServiceAccount of
  the coder Jobs the meitheal-agent submits, so write access to Agents is
  equivalent to creating pods with that ServiceAccount in the namespace those
  Jobs run in. A `cloud-proxy` Agent names an endpoint
  (`spec.providerConfig.baseURL`) and a Secret key
  (`spec.providerConfig.apiKeySecretRef`, resolved in the task's namespace);
  the agent sends that Secret's value as a bearer token to that endpoint with
  every model request. Grant `create`, `update` and `patch` on Agents only to
  principals you would also trust with those Secrets.
- **Credentials come from Secrets.** The meitheal-agent reads its GitHub token
  from the Secret named by the chart's `agent.githubToken.secretName`, and
  coder Jobs get theirs from `coder.gitCredentialsSecret`. The chart's
  meitheal-agent ClusterRole and the coder Job Role both grant `get` on
  Secrets, without a `resourceNames` restriction.
- **Cloud providers are a kill switch, not a default.** The operator's
  `--allow-cloud-providers` flag (chart value `meitheal.allowCloudProviders`)
  defaults to `true`. Set it to `false` on air-gapped or compliance-restricted
  clusters: the Workload controller then drops every step whose Agent uses a
  non-local provider and reports a `CloudReviewersSuppressed` condition. It
  does not apply to an AgenticTask created outside a Workload.

## Verifying releases

Release images are signed with cosign and carry SLSA build provenance and SBOM
attestations, and the `.tar.gz` release archives carry SLSA build provenance,
all made by the `release-please.yml` workflow on `main`. The darwin binaries
are code-signed and, in releases built with the signing secrets, notarized by
Apple. [Verifying Meitheal releases](docs/security/verifying-releases.md) has
the commands that check each of these.

## Security Best Practices

When deploying Meitheal:

1. **Use RBAC**: Restrict who can create Agents, AgenticTasks and Workloads (see [Trust model](#trust-model))
2. **Scope credentials**: Give the agent a GitHub token limited to the repositories it works on, and keep model API keys in their own Secrets
3. **Network Policies**: Enable the chart's `networkPolicies` and isolate the agents from workloads they do not need to reach
4. **Cloud providers**: Set `meitheal.allowCloudProviders: false` where no request may leave the cluster
5. **Resource Limits**: Always set CPU/memory limits to prevent DoS
6. **Image Verification**: Verify release images before you deploy them (see [Verifying releases](#verifying-releases)), and use image digests in production Helm values
