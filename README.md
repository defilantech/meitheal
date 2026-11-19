# Meitheal

<p align="center">
  <img src="docs/images/meitheal-logo-icon.svg" alt="Meitheal" width="128" height="128" />
</p>

Meitheal is a Kubernetes-native harness for agentic coding. It dispatches
coder, verifier and reviewer agents across a fleet of nodes, runs each agent
through a native Go function-calling loop, gates the result with the same
checks your CI runs, and opens pull requests against your GitHub
repositories. In Irish a meitheal is a working party, such as a team of
reapers or turf-cutters.

Agents run on models you host. An Agent's model is either an
InferenceService served by [LLMKube](https://github.com/defilantech/LLMKube)
or any OpenAI-compatible endpoint.

Meitheal started as Foreman inside LLMKube and moved to this repository with
its git history. Issue and pull request numbers in the imported history refer
to defilantech/LLMKube.

## What it installs

- The `meitheal.dev` API group: Agent, AgenticTask, AgentRelease, FleetNode,
  ModelProfile and Workload.
- `meitheal-operator`, the controllers and validating webhooks for those
  kinds.
- `meitheal-agent`, the per-node worker that claims tasks and runs them. It
  runs in-cluster from the Helm chart, or natively on macOS (launchd) and
  Linux (systemd) hosts.
- The `meitheal` CLI: `dispatch`, `slice`, `run`, `decisions`, `audit export`
  and `version`.

## Install

The chart is published to GHCR as an OCI artifact:

```bash
helm install meitheal oci://ghcr.io/defilantech/charts/meitheal \
  --namespace meitheal-system --create-namespace
```

When your Agents use LLMKube InferenceServices, install LLMKube first; the
operator reads the `inference.llmkube.dev` CRDs that LLMKube's chart
installs. The two charts have no Helm dependency on each other.

The CLI and the native agent ship as release archives on the
[releases page](https://github.com/defilantech/meitheal/releases). To install
the agent on a Mac from a checkout, run `make install-agent` (see
[the M3 runbook](docs/runbook-m3.md)).

The [docs](docs/README.md) cover the pipeline, the gate checks, the remote
model coder and the node runbooks.

## Migrating from Foreman

The new API group means new CRDs, so Meitheal installs beside an existing
Foreman release instead of upgrading it, and the resources you keep are
copied across. The order matters: the `foreman` chart templates its six CRDs
without `helm.sh/resource-policy: keep`, so `helm uninstall` of that release
deletes the CRDs and every Agent, AgenticTask, AgentRelease, FleetNode,
ModelProfile and Workload with them, and annotating the live CRDs does not
stop it. Follow the [migration guide](docs/migrating-from-foreman.md): it
takes the CRDs out of the Foreman release first, then maps every renamed
image, chart, CLI command, metric and agent identifier and walks through the
move step by step.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md). Security
reports follow [SECURITY.md](SECURITY.md).

## License

Apache 2.0. See [LICENSE](LICENSE).
