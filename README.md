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
Foreman release instead of upgrading it. The `foreman` chart templates its
six CRDs without `helm.sh/resource-policy: keep`, so `helm uninstall` of that
release deletes the CRDs and every Agent, AgenticTask, AgentRelease,
FleetNode, ModelProfile and Workload with them. Move in this order:

1. Annotate the six `foreman.llmkube.dev` CRDs so Helm keeps them:

   ```bash
   for crd in agents agentictasks agentreleases fleetnodes modelprofiles workloads; do
     kubectl annotate crd "${crd}.foreman.llmkube.dev" helm.sh/resource-policy=keep
   done
   ```

2. Install the `meitheal` chart as a new release (see [Install](#install)).
3. Export the durable resources (Agent, AgentRelease, FleetNode,
   ModelProfile), change their `apiVersion` to `meitheal.dev/v1alpha1`, and
   apply them.
4. Let in-flight AgenticTasks and Workloads finish.
5. Uninstall the `foreman` release, then delete the old CRDs.

Other renames to account for:

| Foreman | Meitheal |
| ------- | -------- |
| API group and label prefix `foreman.llmkube.dev` | `meitheal.dev` |
| Images `ghcr.io/defilantech/llmkube-foreman-{operator,agent,agent-coder}` | `ghcr.io/defilantech/meitheal-{operator,agent,agent-coder}` |
| Chart `oci://ghcr.io/defilantech/charts/foreman` | `oci://ghcr.io/defilantech/charts/meitheal` (versions start at 0.1.0) |
| CLI `llmkube foreman <command>`, `llmkube audit export` | `meitheal <command>`, `meitheal audit export` |
| Metrics `foreman_*` | `meitheal_*` |
| macOS signing identifier `dev.llmkube.foreman-agent`, launchd label `com.llmkube.foreman-agent` | `dev.meitheal.agent` for both |
| Agent install root `~/Library/Application Support/llmkube/foreman-agent` | `~/Library/Application Support/meitheal/meitheal-agent` |
| Branches `foreman/<workload>/issue-<N>` | `meitheal/<workload>/issue-<N>` |

A macOS agent has to be reinstalled under the new label: run
`make uninstall-foreman-agent` from an LLMKube checkout that still has it,
then `make install-agent` from this repository. Dashboards or alerts outside
the chart that query `foreman_*` metrics need the new names.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md). Security
reports follow [SECURITY.md](SECURITY.md).

## License

Apache 2.0. See [LICENSE](LICENSE).
