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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// Meitheal task metrics (#1491): per-Agent verdict rates, task duration, and
	// turn counts. Task outcomes live only on AgenticTask/Workload CRs, which are
	// deleted with their Workload on every retry, so the operator emits them at
	// terminal-phase transition to survive CR deletion by construction. Labels
	// are bounded (Agent names, task kinds, verdict/outcome enums) — never task
	// names — so cardinality stays trivial.

	// MeithealTaskCompletedTotal counts tasks reaching a terminal phase, by the
	// Agent that ran them, the task kind, the verdict, and the machine outcome
	// carried in result.extra.outcome ("" when the run has none).
	MeithealTaskCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "meitheal_task_completed_total",
			Help: "Total number of Meitheal tasks reaching a terminal phase, by agent, kind, verdict, and outcome.",
		},
		[]string{"agent", "kind", "verdict", "outcome"},
	)

	// MeithealTaskDurationSeconds measures wall-clock task duration, from the
	// executor's result.elapsedSec. Buckets cover the 2700s task timeout.
	MeithealTaskDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "meitheal_task_duration_seconds",
			Help:    "Wall-clock duration of a Meitheal task, from the executor's result.elapsedSec.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s to ~4096s
		},
		[]string{"agent", "kind", "verdict"},
	)

	// MeithealTaskTurns measures agent-loop turns consumed, from
	// result.extra.turnCount.
	MeithealTaskTurns = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "meitheal_task_turns",
			Help:    "Number of agent-loop turns a Meitheal task used, from result.extra.turnCount.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10), // 1 to ~512
		},
		[]string{"agent", "kind"},
	)

	// MeithealWorkloadCompletedTotal counts workloads reaching a terminal phase,
	// by outcome (completed / failed), derived from status.phase. The operator
	// does not observe the PR state a Workload produced (the ephemeral agent
	// opens the PR via githubpr.EnsurePR), so the split is by terminal phase,
	// not by whether a PR landed.
	MeithealWorkloadCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "meitheal_workload_completed_total",
			Help: "Total number of Meitheal workloads reaching a terminal phase, by outcome.",
		},
		[]string{"outcome"},
	)

	// MeithealArchiveFailuresTotal counts bundles that could not be written.
	// Archival is deliberately non-fatal, so this counter is the only signal
	// that it is broken: without it, a misconfigured or full volume looks
	// exactly like a cluster that has had no terminal tasks.
	MeithealArchiveFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "meitheal_archive_failures_total",
			Help: "Total task-archive bundles that failed to write, by reason.",
		},
		[]string{"reason"},
	)
)

// MeithealCollectors are the metrics the meitheal-operator records. It registers
// them in controller-runtime's registry, which its manager serves.
var MeithealCollectors = []prometheus.Collector{
	MeithealTaskCompletedTotal,
	MeithealTaskDurationSeconds,
	MeithealTaskTurns,
	MeithealWorkloadCompletedTotal,
	MeithealArchiveFailuresTotal,
}

// RecordTaskOutcome emits the operator-side counters and histograms for one
// terminal Meitheal task (#1491). The operator observes every terminal-phase
// transition regardless of execution mode, so these series survive CR deletion
// by construction — the per-Agent verdict rates the audit ConfigMap can only
// reconstruct after the fact.
//
// The caller extracts the bounded label values (agent name, kind, verdict, and
// the machine outcome carried at result.extra.outcome) from the terminal
// AgenticTask and passes them here. The task NAME is deliberately never a
// label: it is unbounded and would break cardinality.
//
// The histograms observe only when the executor reported a value (elapsedSec
// and turns are both 0/absent on a cascade-skip or a run that died before
// reporting), so a missing value does not skew the distribution.
func RecordTaskOutcome(agent, kind, verdict, outcome string, elapsedSec float64, turns int) {
	MeithealTaskCompletedTotal.WithLabelValues(agent, kind, verdict, outcome).Inc()
	if elapsedSec > 0 {
		MeithealTaskDurationSeconds.WithLabelValues(agent, kind, verdict).Observe(elapsedSec)
	}
	if turns > 0 {
		MeithealTaskTurns.WithLabelValues(agent, kind).Observe(float64(turns))
	}
}

// RecordWorkloadOutcome emits the operator-side counter for one terminal
// Meitheal Workload (#1491). outcome is the bounded terminal bucket the
// Workload rollup classified it into, derived from status.phase: "completed"
// or "failed". Counters are monotonically increasing; a Workload that
// re-reaches the same terminal phase across reconciles is emitted once per
// terminal transition by the caller, which guards on phase change.
func RecordWorkloadOutcome(outcome string) {
	MeithealWorkloadCompletedTotal.WithLabelValues(outcome).Inc()
}

// RecordArchiveFailure counts one failed archive write. Reason is a bounded
// classification (for example "transcript_read", "write"), never an error
// string: error text is unbounded and would break cardinality.
func RecordArchiveFailure(reason string) {
	MeithealArchiveFailuresTotal.WithLabelValues(reason).Inc()
}
