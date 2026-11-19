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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// getHistogramMetric is a test helper that observes a value and reads the metric.
func getHistogramMetric(t *testing.T, h *prometheus.HistogramVec, labels []string, value float64) *dto.Metric {
	t.Helper()
	h.WithLabelValues(labels...).Observe(value)
	observer, err := h.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("failed to get metric: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return &m
}

// The operator registers MeithealCollectors itself (cmd/meitheal-operator tests
// that); importing this package must not register anything in
// controller-runtime's registry, or every binary that imports it would export
// them.
func TestImportRegistersNothingInControllerRuntime(t *testing.T) {
	for _, c := range MeithealCollectors {
		if ctrlmetrics.Registry.Unregister(c) {
			t.Errorf("collector %T was registered in controller-runtime's registry on import", c)
		}
	}
}

func TestRecordTaskOutcomeCounter(t *testing.T) {
	// The completed counter is unconditional: every terminal task emits it,
	// keyed by the bounded label set (agent, kind, verdict, outcome).
	RecordTaskOutcome("coder", "issue-fix", "GO", "MODEL-DECIDED", 42.0, 17)

	var m dto.Metric
	obs := MeithealTaskCompletedTotal.WithLabelValues("coder", "issue-fix", "GO", "MODEL-DECIDED")
	if err := obs.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Errorf("expected completed counter >= 1, got %f", got)
	}
}

func TestRecordTaskOutcomeDuration(t *testing.T) {
	// A positive elapsedSec must land in the duration histogram.
	RecordTaskOutcome("coder", "issue-fix", "NO-GO", "ALREADY-RESOLVED", 88.0, 0)
	m := getHistogramMetric(t, MeithealTaskDurationSeconds, []string{"coder", "issue-fix", "NO-GO"}, 1.0)
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected duration sample count > 0 after a positive observation")
	}
}

func TestRecordTaskOutcomeSkipsZeroDurationAndTurns(t *testing.T) {
	// A cascade-skip / pre-report terminal task has no elapsedSec or turns:
	// the histograms must NOT observe zero, or the distribution skews low.
	RecordTaskOutcome("verifier", "verify", "GATE-PASS", "", 0, 0)

	// Read the series directly without observing (getHistogramMetric would
	// observe its own value and inflate the count).
	durObs, err := MeithealTaskDurationSeconds.GetMetricWithLabelValues("verifier", "verify", "GATE-PASS")
	if err != nil {
		t.Fatalf("failed to get duration metric: %v", err)
	}
	var dm dto.Metric
	if err := durObs.(prometheus.Metric).Write(&dm); err != nil {
		t.Fatalf("failed to write duration metric: %v", err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("zero elapsedSec must not observe duration, got %d samples", got)
	}

	turnsObs, err := MeithealTaskTurns.GetMetricWithLabelValues("verifier", "verify")
	if err != nil {
		t.Fatalf("failed to get turns metric: %v", err)
	}
	var tm dto.Metric
	if err := turnsObs.(prometheus.Metric).Write(&tm); err != nil {
		t.Fatalf("failed to write turns metric: %v", err)
	}
	if got := tm.GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("zero turns must not observe the turns histogram, got %d samples", got)
	}
}

func TestRecordTaskOutcomeTurns(t *testing.T) {
	// A positive turn count must land in the turns histogram.
	RecordTaskOutcome("coder", "issue-fix", "GO", "", 12.0, 9)
	m := getHistogramMetric(t, MeithealTaskTurns, []string{"coder", "issue-fix"}, 3.0)
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected turns sample count > 0 after a positive observation")
	}
}

func TestRecordWorkloadOutcome(t *testing.T) {
	RecordWorkloadOutcome("completed")
	RecordWorkloadOutcome("failed")

	var m dto.Metric
	if err := MeithealWorkloadCompletedTotal.WithLabelValues("completed").Write(&m); err != nil {
		t.Fatalf("failed to write completed metric: %v", err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Errorf("expected completed workload counter >= 1, got %f", got)
	}
	if err := MeithealWorkloadCompletedTotal.WithLabelValues("failed").Write(&m); err != nil {
		t.Fatalf("failed to write failed metric: %v", err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Errorf("expected failed workload counter >= 1, got %f", got)
	}
}
