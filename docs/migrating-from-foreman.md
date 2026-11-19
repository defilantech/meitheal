# Migrating from Foreman

Meitheal started as Foreman, an add-on chart inside LLMKube, and moved to
this repository with its git history. The rename changes the API group, the
images, the chart, the CLI, the metrics and the identity of the native agent.
Because the API group changed, Meitheal's CRDs are new CRDs: Meitheal installs
beside an existing Foreman release instead of upgrading it, and the resources
you want to keep are copied across.

The Meitheal images and chart are published from Meitheal's first release,
0.1.0, onward. Check the
[releases page](https://github.com/defilantech/meitheal/releases) before you
start: until a release is listed there, nothing under the Meitheal names can
be pulled, and a Foreman install should stay as it is.

Nothing published for Foreman is removed. The `llmkube-foreman-*` images and
the `foreman` chart stay where they are, so a Foreman release keeps working
while you move. LLMKube itself does not change: Agents that use LLMKube
InferenceServices keep using them, and LLMKube stays installed.

## What was renamed

| Foreman | Meitheal |
| ------- | -------- |
| API group and label prefix `foreman.llmkube.dev` | `meitheal.dev` |
| Images `ghcr.io/defilantech/llmkube-foreman-{operator,agent,agent-coder}` | `ghcr.io/defilantech/meitheal-{operator,agent,agent-coder}` |
| Chart `oci://ghcr.io/defilantech/charts/foreman` (also `llmkube/foreman` in the LLMKube Helm repository) | `oci://ghcr.io/defilantech/charts/meitheal`, with versions starting at 0.1.0 |
| Release `foreman` in namespace `foreman-system`, as the Foreman docs install it | Release `meitheal` in namespace `meitheal-system` |
| Chart values block `foreman:` | `meitheal:` |
| Coder git Secret default `foreman-git-credentials` | `meitheal-git-credentials` |
| Coder ServiceAccount default `foreman-coder` | `meitheal-coder` |
| CLI `llmkube foreman <command>`, `llmkube audit export` | `meitheal <command>`, `meitheal audit export` |
| Metrics `foreman_*` | `meitheal_*` |
| `agentKind: foreman-agent` on FleetNodes and AgentReleases | `agentKind: meitheal-agent` |
| macOS signing identifier `dev.llmkube.foreman-agent` | `dev.meitheal.agent` |
| macOS launchd label `com.llmkube.foreman-agent` | `dev.meitheal.agent` |
| macOS install root `~/Library/Application Support/llmkube/foreman-agent` | `~/Library/Application Support/meitheal/meitheal-agent` |
| Make targets `install-foreman-agent`, `uninstall-foreman-agent` (LLMKube repository) | `install-agent`, `uninstall-agent` (this repository) |
| Linux user unit `llmkube-foreman-agent.service` | `meitheal-agent.service` |
| Branches `foreman/<workload>/issue-<N>` | `meitheal/<workload>/issue-<N>` |

Dashboards and alerts outside the chart that query `foreman_*` metrics need
the new names. The chart's own Grafana dashboard and PrometheusRule already
use them.

## Before you start

You need `kubectl` with rights to annotate and delete CRDs, Helm 3.13 or
later (for `helm get metadata`), and `jq`.

The commands below assume the Foreman release is `foreman` in
`foreman-system`, as the Foreman docs installed it. `helm list
--all-namespaces` shows yours: look for the chart `foreman-<version>`, and
adjust the names if they differ.

## Move in this order

The order matters because the `foreman` chart templates its six CRDs without
`helm.sh/resource-policy: keep`. Deleting a CRD deletes every resource of
that kind, so a plain `helm uninstall foreman` takes every Agent, AgenticTask,
AgentRelease, FleetNode, ModelProfile and Workload with it. Steps 1 and 2 take
the CRDs out of Helm's hands first, so no later step can remove them by
accident.

Annotating the live CRDs is not enough on its own. `helm uninstall` reads the
resource policy from the manifest stored with the release, not from the
objects in the cluster. `helm upgrade` is different: before it deletes
something that has dropped out of the chart, it reads the live object and
skips the delete when that object carries the keep policy. The first two
steps use that.

### 1. Annotate the Foreman CRDs

Before you change anything, note what the six CRDs hold, so you can compare
after step 2:

```bash
kubectl get crd | grep foreman.llmkube.dev
kubectl get agents.foreman.llmkube.dev,agentictasks.foreman.llmkube.dev,agentreleases.foreman.llmkube.dev,fleetnodes.foreman.llmkube.dev,modelprofiles.foreman.llmkube.dev,workloads.foreman.llmkube.dev \
  --all-namespaces --no-headers -o custom-columns=KIND:.kind,NAMESPACE:.metadata.namespace,NAME:.metadata.name \
  | sort > foreman-before.txt
```

Then give each CRD Helm's keep policy:

```bash
for crd in agents agentictasks agentreleases fleetnodes modelprofiles workloads; do
  kubectl annotate crd "${crd}.foreman.llmkube.dev" helm.sh/resource-policy=keep
done
```

### 2. Take the CRDs out of the Foreman release

Upgrade the release to the same chart at the same version, with its CRDs
switched off. Helm drops the six CRDs from the release and, because of the
annotation, leaves them in the cluster:

```bash
FOREMAN_CHART_VERSION="$(helm get metadata foreman --namespace foreman-system -o json | jq -r .version)"
helm upgrade foreman oci://ghcr.io/defilantech/charts/foreman \
  --namespace foreman-system \
  --version "$FOREMAN_CHART_VERSION" \
  --reuse-values --set crds.install=false
```

If you installed from the LLMKube Helm repository, `llmkube/foreman` works in
place of the OCI reference: what matters is the same chart at the same
version. Every published `foreman` chart has the `crds.install` value.

Confirm that the release no longer owns a CRD, that all six CRDs are still
there, and that every resource in them survived:

```bash
helm get manifest foreman --namespace foreman-system | grep -c 'kind: CustomResourceDefinition'   # expect 0
kubectl get crd | grep foreman.llmkube.dev                                                    # expect six rows
kubectl get agents.foreman.llmkube.dev,agentictasks.foreman.llmkube.dev,agentreleases.foreman.llmkube.dev,fleetnodes.foreman.llmkube.dev,modelprofiles.foreman.llmkube.dev,workloads.foreman.llmkube.dev \
  --all-namespaces --no-headers -o custom-columns=KIND:.kind,NAMESPACE:.metadata.namespace,NAME:.metadata.name \
  | sort > foreman-after.txt
diff foreman-before.txt foreman-after.txt
```

`diff` should print no line that starts with `<`: nothing that existed before
is gone. Lines starting with `>` are fine; they are AgenticTasks a running
Workload created in the meantime.

The upgrade changes nothing else in the release, so Foreman keeps running.
From here, `helm uninstall foreman` can no longer delete the CRDs; only an
explicit `kubectl delete crd` in step 6 does.

### 3. Install the `meitheal` chart as a new release

Start from the values the Foreman release uses:

```bash
helm get values foreman --namespace foreman-system -o yaml > meitheal-values.yaml
```

In `meitheal-values.yaml`, rename a top-level `foreman:` block to `meitheal:`,
drop any `operator.image` or `agent.image` override that points at an
`llmkube-foreman-*` image, so the chart's own Meitheal images apply, and
remove a `namespace:` value if you set one. Every other value keeps its
name. The file also carries the `crds.install: false`
that step 2 set; the install command below turns it back on.

Secrets do not follow a release into a new namespace. Create the namespace
and copy across the Secrets the Foreman release reads: the GitHub token named
by `agent.githubToken.secretName` (and by each `agents.<pool>.githubToken`),
and the coder git Secret, whose default name changed. For the coder Secret:

```bash
kubectl create namespace meitheal-system
kubectl get secret foreman-git-credentials --namespace foreman-system -o json \
  | jq '{apiVersion, kind, type, data, metadata: {name: "meitheal-git-credentials", namespace: "meitheal-system"}}' \
  | kubectl create -f -
```

Copy the GitHub token Secret, and any other Secret the release reads, the
same way, with its own name in the `jq` filter. Then install, with the CRDs
switched back on:

```bash
helm install meitheal oci://ghcr.io/defilantech/charts/meitheal \
  --namespace meitheal-system \
  --values meitheal-values.yaml \
  --set crds.install=true
```

The Meitheal chart installs its CRDs with `helm.sh/resource-policy: keep`
already in the release manifest, so uninstalling it later leaves them, and
the resources in them, in place.

The Meitheal operator and agents run beside Foreman's. Each one works only on
its own API group, so nothing is claimed twice.

### 4. Copy the durable resources

Agents, AgentReleases, FleetNodes and ModelProfiles describe your fleet and
outlive any one task. Export them, change their `apiVersion` to
`meitheal.dev/v1alpha1`, and apply them:

```bash
kubectl get \
  agents.foreman.llmkube.dev,agentreleases.foreman.llmkube.dev,fleetnodes.foreman.llmkube.dev,modelprofiles.foreman.llmkube.dev \
  --all-namespaces -o json > foreman-durable.json

jq '
  def rekey: with_entries(.key |= sub("^foreman\\.llmkube\\.dev/"; "meitheal.dev/"));
  .items |= map(
    select(.kind != "AgentRelease" or .spec.agentKind != "foreman-agent")
    | .apiVersion = "meitheal.dev/v1alpha1"
    | if .kind == "AgentRelease" then .spec.approved = false else . end
    | del(.status, .metadata.uid, .metadata.resourceVersion, .metadata.generation,
          .metadata.creationTimestamp, .metadata.managedFields, .metadata.ownerReferences,
          .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"])
    | if .metadata.labels then .metadata.labels |= rekey else . end
    | if .metadata.annotations then .metadata.annotations |= rekey else . end
  )
' foreman-durable.json > meitheal-durable.json
```

The filter changes the API group, renames label and annotation keys from the
`foreman.llmkube.dev/` prefix to `meitheal.dev/`, and strips the fields the
API server sets. It leaves out every AgentRelease with
`agentKind: foreman-agent`: Meitheal no longer accepts that kind, and those
releases roll out Foreman binaries. Publish a new AgentRelease for a
`meitheal-agent` version when you want one. The AgentReleases it does copy
arrive with `spec.approved: false`, so each waits in `AwaitingApproval`
instead of starting a rollout on its own; approve one again only if you still
want it.

Job-mode Agents name an image and a ServiceAccount under `spec.execution`.
List them:

```bash
jq -r '.items[] | select(.kind == "Agent" and .spec.execution != null)
  | "\(.metadata.namespace)/\(.metadata.name)  image=\(.spec.execution.image // "-")  serviceAccountName=\(.spec.execution.serviceAccountName // "-")"' \
  meitheal-durable.json
```

Edit `meitheal-durable.json` so an image of
`ghcr.io/defilantech/llmkube-foreman-agent-coder` becomes
`ghcr.io/defilantech/meitheal-agent-coder` at the Meitheal version you
installed (version numbers restart at 0.1.0, so keep no old tag), and a
`serviceAccountName` of `foreman-coder` becomes `meitheal-coder`. Then check
the result against the new CRDs and webhooks, and apply it:

```bash
kubectl apply --dry-run=server -f meitheal-durable.json
kubectl apply -f meitheal-durable.json
```

The FleetNodes worth carrying over are those of native agents on macOS and
Linux hosts: a reinstalled agent with the same node name adopts its FleetNode
and keeps the labels you added. An agent in the cluster registers a new
FleetNode named after its pod.

From here on, create new Workloads as `meitheal.dev/v1alpha1` resources, and
leave the Foreman ones to finish.

### 5. Let in-flight AgenticTasks and Workloads finish

Foreman's operator and agents carry on with the work they already hold.
Watch it drain:

```bash
kubectl get workloads.foreman.llmkube.dev,agentictasks.foreman.llmkube.dev --all-namespaces \
  -o custom-columns=KIND:.kind,NAMESPACE:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase
```

Move on when every Workload is `Completed` or `Failed` and every AgenticTask
is `Succeeded` or `Failed`. This is also the time to reinstall native agents
(below), one host at a time, once the host has no task in flight.

### 6. Uninstall Foreman, then delete the old CRDs

```bash
helm uninstall foreman --namespace foreman-system
```

Step 2 took the CRDs out of the release, so this removes Foreman's operator,
agents, webhook and RBAC and nothing else. Deleting the CRDs removes the
finished Foreman Workloads and AgenticTasks with them. If you want that
history, save it first:

```bash
kubectl get workloads.foreman.llmkube.dev,agentictasks.foreman.llmkube.dev \
  --all-namespaces -o yaml > foreman-history.yaml

for crd in agents agentictasks agentreleases fleetnodes modelprofiles workloads; do
  kubectl delete crd "${crd}.foreman.llmkube.dev"
done
```

The copied Secrets now live in `meitheal-system`. Delete the
`foreman-system` namespace once nothing else uses it.

## Reinstall native agents

Agents that run natively on macOS (launchd) or Linux (systemd) hosts sit
outside the chart, so they move by hand. The Meitheal agent has a new
signing identifier and a new launchd label or unit name, so it installs
beside the Foreman agent instead of over it. Remove the old one first, after
the host's in-flight tasks finish, so the host never runs two task loops at
once.

### macOS

From an LLMKube checkout that still has the Foreman targets:

```bash
make uninstall-foreman-agent
```

Without one, the same thing by hand:

```bash
launchctl bootout "gui/$(id -u)/com.llmkube.foreman-agent"
rm -f ~/Library/LaunchAgents/com.llmkube.foreman-agent.plist
```

Then, from a checkout of this repository:

```bash
make install-agent MEITHEAL_INSTALLED_MODELS=<model>
```

`install-agent` builds the agent, stages it under
`~/Library/Application Support/meitheal/meitheal-agent` and starts it as
`dev.meitheal.agent`. It names the FleetNode after `hostname -s`, as the
Foreman target did; pass `MEITHEAL_NODE_NAME=<name>` to keep a different name
so the agent adopts the FleetNode you copied in step 4. If you had edited
flags in `~/Library/LaunchAgents/com.llmkube.foreman-agent.plist`, carry them
into `~/Library/LaunchAgents/dev.meitheal.agent.plist` and restart:

```bash
launchctl kickstart -k "gui/$(id -u)/dev.meitheal.agent"
```

macOS treats a binary with a new signing identifier as a different program,
so a permission prompt you accepted for the Foreman agent, such as Local
Network access, may appear again. The uninstall leaves
`~/Library/Application Support/llmkube/foreman-agent` in place for rollback;
delete it when you no longer need it.

### Linux

Stop and disable the Foreman user unit:

```bash
systemctl --user disable --now llmkube-foreman-agent
```

Then install `deployment/linux/meitheal-agent.service` as its header comment
describes. Its `ExecStart` runs
`~/.local/share/meitheal/meitheal-agent/current/meitheal-agent`, where the
Foreman unit ran `~/.local/share/llmkube/foreman-agent/current/foreman-agent`.
Carry your flags over from the old unit's `ExecStart` before you enable the
new one.

## The CLI

`llmkube foreman <command>` is now `meitheal <command>`, and
`llmkube audit export` is `meitheal audit export`. The `meitheal` CLI ships
as a release archive on the
[releases page](https://github.com/defilantech/meitheal/releases).
