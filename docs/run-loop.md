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
  dependency is parked, or was skipped at preflight, and starts once that
  dependency is finalized, dropped or hand-fixed, or preflight found its
  issue closed. The run refuses a queue that lists an issue twice
  or whose `after` lists form a cycle.
- One run works in one repository: answers are read from that
  repository's issues. Decision files and ledger records name their
  repository, and a run ignores those of another. A decision from before
  they did is announced and answered only when its issue is in the queue.

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
assistedBy: "Assisted-by: Meitheal; example-coder wrote the change, example-reviewer reviewed it"
```

`assistedBy` is the attribution line every finalized PR ends with, used as
written, with `; gates:` and the gate commands that ran appended. Without
it the line is `Assisted-by: Meitheal, with self-hosted models`.

`--gate-deadline-seconds` (default 1800) is the wall-clock limit of one
gate Job. `--gate-image-pull-secrets=name1,name2` names Secrets in
`--namespace` that the gate Jobs pull their image with, for a gate image in
a private registry; without it the pull uses the namespace's `default`
ServiceAccount.

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

`--gpu-paths` (the planner flag) also guards the cpu lane. Before gating a
cpu-lane branch, verify lists the files it changes against `main` (through
the compare API; a listing that fails, or reaches the API's 300-file limit,
is an error). When one starts with a GPU path, prefixes matched as the
planner matches them, verify fails without running a gate, with the
evidence "this change touches GPU code and needs the GPU gate", and the item
is verified as gpu-lane from then on: the feedback pass runs the CPU gate
and then the GPU gate, and the finalized PR lists both. A run with
`--gpu-paths` needs a template with `gpuGate`.

## What parks

Every park is a file `<issue>-<kind>.yaml` under `--decisions-dir`, listed
by `meitheal decisions`.

| Kind | When |
|---|---|
| `escalate` | The coder stalled; the Workload failed (see below); the gate failed again after the feedback pass; the GPU node stayed busy; the loop lost track of the issue's run; the loop kept failing on the issue. |
| `needs-decision` | A task raised a concern (one the loop cannot read, or with no text, counts); the planner found a decision only a maintainer can make, its answer was unreadable, or its call failed; the issue carries the `--decision-label` label. |
| `needs-split` | The planner judged the issue too big for one run. |

A failed Workload's reason comes from its tasks' outcomes, judged by the
newest round (fix iteration):

| The newest round | Reason |
|---|---|
| The fix iteration's coder answered NO-GO | review returned NO-GO in round *k* and the coder could not address it |
| The fix iteration's coder failed some other way | review returned NO-GO in round *k* and the coder's fix attempt failed |
| A reviewer said NO-GO, and the Workload's `maxReviewIterations` was used up | review returned NO-GO after *k* rounds (iteration cap) |
| A reviewer said NO-GO below the cap, and no fix iteration followed | review returned NO-GO in round *k* and no fix iteration followed |
| The gate returned `GATE-FAIL` or `GATE-ERROR` | the gate returned GATE-FAIL on the coder's first attempt (or: on the coder's fix for review round *k*) |
| The reviewer of a fix iteration returned no verdict | review returned NO-GO in round *k* and the next review did not return a verdict |
| The first coder failed or declined, or nothing above applies | the run failed before a review verdict |

A coder-escalation attempt (`code-<n>-esc`), once it exists, is the round
judged, and its reasons say "on the escalated coder's attempt". The
decision's evidence carries the latest reviewer summary (`review`), the
coder's last summary (`coder`) and, for a gate failure, the gate's (`gate`),
each capped at 2 KiB; none of them reaches the issue comment.

Every park is announced once: a one-line comment on the issue (the reason,
and the draft PR link when there is one; never logs or evidence), the
`meitheal:parked` label on the draft PR (taken off again when the PR is
finalized), and, when `--notify-url` (or
`$MEITHEAL_NOTIFY_URL`) is set, a JSON POST with the whole decision. The
decision file records each notice that went out (`notices`) and when all of
them had (`notified`). A notice that failed is retried on every answer
cycle without repeating the ones that went out, and the comment is not
posted again when this park's line is already on the issue (from up to two
minutes before the park opened, to allow for clock skew).

## Answering a park

Comment on the issue, from the login given as `--answerer`, with one line:

| Command | What the loop does |
|---|---|
| `/meitheal requeue` | Starts a new round with the item's intent. A planner park is planned again first. |
| `/meitheal decide <text>` | Plans the issue again with your text given to the planner as a settled maintainer decision, then starts a new round with the new intent. One the planner parks again is announced as a new park. |
| `/meitheal drop` | Comments "Meitheal dropped this issue at the maintainer's request." once and stops working on the issue. The branch, the PR and the decision file stay. |

Only the first valid command from `--answerer` after the park counts (from
up to two minutes before it, for clock skew, but never the command the loop
last acted on for that issue); comments from anyone else are ignored, and a
command the decision does not offer is skipped and logged once. The loop reads answers every 5 minutes,
and at start.

`meitheal decisions answer ISSUE KIND ANSWER` answers from the command
line. It also takes `hand-fix` on an `escalate` park: the item is recorded
as done and nothing is posted.

A decide answer needs the planner (`--repo-dir` and `--planner-url`), with
or without `--plan`; without it the answer waits and the log says why. A
re-plan writes the intent under `--out` and rewrites the run's queue file
(the `--queue` file, or `--out/queue.yaml` with `--plan`) so a restart
drives the new intent.

### Rounds

A round is one Workload's worth of work on an issue: the first attempt and
at most one feedback pass. Only a maintainer's `requeue` or `decide` starts
a new round. It deletes the issue's Workload if one is still there (the
branch and the draft PR stay), creates it again as attempt 1, revising the
branch when one was pushed so the coder continues the same draft PR, and
drives it. When the issue has a round of the loop's (its own Workload, or
its own `meitheal/<workload>/issue-<n>` branch), the new round skips
preflight, because the open draft PR and branch preflight would find are
the loop's own. Without one, as for a planner park that was never
dispatched, preflight runs as usual: an open PR someone else made skips the
issue, recorded as `skipped`. The answered decision is removed once the
round has a Workload or a preflight skip, so a restart in between acts on
the answer again, and a drive that fails before then is retried as the new
round. When that drive fails a third time, the answer is used up and the
issue parks `escalate` ("the loop kept failing on this issue") for a fresh
answer.

## The ledger

Each drive's end, and each answer that finishes an item, is one JSON line
in `--ledger` (default `.meitheal/ledger.jsonl`):

```json
{"repo":"example/widgets","issue":52,"lane":"cpu","workload":"wl-52","attempts":1,"finalStage":"done","parkKind":"","reason":"finalized","prURL":"https://github.com/example/widgets/pull/60","started":"...","ended":"..."}
```

`finalStage` is `done` (finalized or hand-fixed), `skipped` (preflight
found an open PR, or the issue closed; the reason says which), `parked`,
`dropped` or `error` (the drive stopped on an error; it is tried again). A last line cut short by a crash is ignored, and
dropped by the next append.

## Restarts and signals

The loop keeps no state of its own beyond the decision files, the ledger
and the queue file, so it can be stopped and started at any point:

- An item whose last ledger record is `done` or `dropped` is not driven
  again.
- An item with a decision file stays parked until it is answered.
- An item whose Workload is already in the cluster resumes it at watch;
  nothing is dispatched twice. Only a Workload that runs the issue in the
  run's repository counts: one under the issue's name that runs it in
  another repository is an error naming both, and is never resumed,
  deleted or recreated.
- An item whose own task branch (`meitheal/wl-<n>/issue-<n>`) exists with
  no Workload of the loop's behind it parks `escalate` with "the loop lost
  track of this issue's run", and is announced. That is what is left after
  a feedback pass's old Workload was deleted and the new one never
  created, or after a Workload was deleted by hand. `/meitheal requeue`
  starts a new round on the branch and its draft PR.
- An item whose issue is closed is skipped at preflight.
- An item that was `skipped` goes through preflight again.
- A drive that stops on an `error` is tried again on the next answer cycle
  (every 5 minutes, or straight away with `--once`); a drive that gets to
  an outcome resets the count, and the third error in a row parks the item
  `escalate` with "the loop kept failing on this issue" (the error is in the
  decision's evidence, never on the issue). The count is kept per process,
  so a restart starts it again.

SIGINT or SIGTERM stops the loop: in-flight items stop at their next poll,
nothing is parked, the stop deletes no Workload, and the next start resumes
them. A feedback pass stopped while its old Workload was being deleted
resumes too, as the feedback pass, if that Workload is still going away at
the next start; if it is already gone, only the branch is left and the item
parks as a lost run.

`--once` drives until nothing more can start, reads answers one last time,
and exits. Without it the loop keeps reading answers until it is stopped.

## Tokens

The GitHub token is read from `$GITHUB_TOKEN`, or else from
`~/.config/meitheal/github-token` on every request, so a token Secret
mounted there and refreshed by `meitheal github-app-token` is picked up
without a restart. `meitheal intents` reads it the same way. The planner
token is `$MEITHEAL_PLANNER_TOKEN`.

Gate Jobs run the branch's own build and tests, so their pods mount no
service account token and get no Service environment variables.
