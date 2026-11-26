# The run loop

`meitheal run` drives a queue of issues through the Meitheal pipeline
without a human in the loop for the mechanical part: it dispatches each
issue as a Workload, watches it, runs an independent gate over the branch
the coder pushed, gives the coder one feedback pass when the gate fails,
and finalizes a draft pull request when it passes. Anything that needs
judgment is **parked**: written down as a decision, announced, and left for
the maintainer, while the loop moves on to the next item.

## The queue

The queue is a YAML file. `meitheal intents` writes one (one intent per
issue, from a planner model), or you write it yourself:

```yaml
repo: example/widgets
items:
  - issue: 52
    intent: intents/52.intent.md
  - issue: 53
    intent: intents/53.intent.md
    lane: gpu
    after: [52]
```

- `lane` is `cpu` (the default) or `gpu`. A gpu item also passes the GPU
  gate from the template.
- `after` lists issues that must **finish** first. An item waits while its
  dependency is parked, and starts once that dependency is finalized,
  dropped or hand-fixed. The run refuses a queue that lists an issue twice
  or whose `after` lists form a cycle.
- One run works in one repository: answers are read from that
  repository's issues.

`--plan` plans `--issues` first with the planner flags `meitheal intents`
takes (`--repo`, `--repo-dir`, `--planner-url`, `--rules-file`,
`--gpu-labels`, `--gpu-paths`, `--trusted-commenters`, `--decision-label`
and the rest), writes the queue to `--out/queue.yaml`, and drives it. On a
restart `--plan` keeps the intents already in that queue and does not plan
a parked or finished issue again.

## The template

`--template` is a YAML file every Workload starts from:

```yaml
spec:                      # a Workload spec, minus the per-issue fields
  repo: example/widgets
  reviewerAgentRefs: [{name: example-reviewer}]
  maxReviewIterations: 2
  mcpEnabled: false
  gateProfile:             # the CPU gate, required
    language: generic
    image: example/gate:1
    commands:
      format: make fmt-check
      lint: make lint
      build: make build
      test: make test
gpuGate:                   # optional: the second gate for gpu-lane items
  language: generic
  image: example/gate-gpu:1
  commands:
    test: make test-gpu
  placement:
    nodeSelector: {kubernetes.io/hostname: gpu-node-1}
gpuNode: gpu-node-1        # checked for a serving model before each GPU gate
```

`--coder-agent` fills `spec.coderAgentRef`; a template that names a
different coder is refused. Unknown keys are an error, so a misspelt
`gpuGate` cannot drop the GPU gate silently. A gate placement may mount a
host path or ask for a priority class only when `--gate-allowed-host-paths`
or `--gate-allowed-priority-classes` allows it (both default to none).

## Lanes

`--parallel cpu=1,gpu=1` (the default) is how many items of each lane run
at once. Items in a lane start in queue order. Before each GPU gate the
loop checks that no model is being served on `gpuNode`; while one is, it
retries every 15 minutes and parks after 12 hours.

## What parks

Every park is a file `<issue>-<kind>.yaml` under `--decisions-dir`, listed
by `meitheal decisions`.

| Kind | When |
|---|---|
| `escalate` | The coder stalled; review did not converge; the gate failed again after the feedback pass; the GPU node stayed busy. |
| `needs-decision` | A task raised a concern; the planner found a decision only a maintainer can make, its answer was unreadable, or its call failed; the issue carries the `--decision-label` label. |
| `needs-split` | The planner judged the issue too big for one run. |

Every park is announced once: a one-line comment on the issue (the reason,
and the draft PR link when there is one; never logs or evidence), the
`meitheal:parked` label on the draft PR, and, when `--notify-url` (or
`$MEITHEAL_NOTIFY_URL`) is set, a JSON POST with the whole decision. The
decision file records when it was announced (`notified`), so a restart does
not announce it again.

## Answering a park

Comment on the issue, from the login given as `--answerer`, with one line:

| Command | What the loop does |
|---|---|
| `/meitheal requeue` | Removes the decision and drives the item again from preflight. A Workload still in the cluster is resumed, with its attempt count. A planner park is planned again. |
| `/meitheal decide <text>` | Plans the issue again with your text given to the planner as a settled maintainer decision. A planned issue is driven; one the planner parks again is announced as a new park. |
| `/meitheal drop` | Comments "Meitheal dropped this issue at the maintainer's request." once and stops working on the issue. The branch, the PR and the decision file stay. |

Only the first valid command from `--answerer` after the park counts;
comments from anyone else are ignored, and a command the decision does not
offer is skipped and logged once. The loop reads answers every 5 minutes,
and at start.

`meitheal decisions answer ISSUE KIND ANSWER` answers from the command
line. It also takes `hand-fix` on an `escalate` park: the item is recorded
as done and nothing is posted.

A decide answer needs the planner (`--repo-dir` and `--planner-url`), with
or without `--plan`; without it the answer waits and the log says why. A
re-plan writes the intent under `--out` and rewrites the run's queue file
(the `--queue` file, or `--out/queue.yaml` with `--plan`) so a restart
drives the new intent.

Requeue and decide resume a Workload that is still in the cluster rather
than create a new one. To start an issue over with a new intent, delete
its Workload (and the branch) before answering.

## The ledger

Each drive's end, and each answer that finishes an item, is one JSON line
in `--ledger` (default `.meitheal/ledger.jsonl`):

```json
{"issue":52,"lane":"cpu","workload":"wl-52","attempts":1,"finalStage":"done","parkKind":"","reason":"finalized","prURL":"https://github.com/example/widgets/pull/60","started":"...","ended":"..."}
```

`finalStage` is `done`, `parked`, `dropped` or `error`.

## Restarts and signals

The loop keeps no state of its own beyond the decision files, the ledger
and the queue file, so it can be stopped and started at any point:

- An item whose last ledger record is `done` or `dropped` is not driven
  again.
- An item with a decision file stays parked until it is answered.
- An item whose Workload is already in the cluster resumes it at watch;
  nothing is dispatched twice.
- An item that stopped on an `error` is tried again.

SIGINT or SIGTERM stops the loop: in-flight items stop at their next poll,
nothing is parked, no Workload is deleted, and the next start resumes them.

`--once` drives until nothing more can start, reads answers one last time,
and exits. Without it the loop keeps reading answers until it is stopped.

## Tokens

The GitHub token is read from `$GITHUB_TOKEN`, or else from
`~/.config/meitheal/github-token` on every request, so a token Secret
mounted there and refreshed by `meitheal github-app-token` is picked up
without a restart. The planner token is `$MEITHEAL_PLANNER_TOKEN`.
