package grounding

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates parent dirs then writes content to path. Fails the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("writeFile mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile write: %v", err)
	}
}

func TestLoadGroundTruth_MetricsAndCLI(t *testing.T) {
	gt, err := LoadGroundTruth("testdata/crd-bases", "testdata/metrics", "testdata/cmd")
	if err != nil {
		t.Fatal(err)
	}
	if !gt.Metrics["llmkube_inferenceservice_phase"] {
		t.Errorf("missing metric; have %v", gt.Metrics)
	}
	if !gt.CLICmds["deploy"] {
		t.Errorf("missing cli command; have %v", gt.CLICmds)
	}
}

// The metric scan is repo-wide, not limited to internal/metrics: a metric a
// package defines elsewhere (LLMKube's Metal agent keeps its own in
// pkg/agent) must be in the ground truth, else the gate would false-positive
// the docs that name it.
func TestLoadGroundTruth_MetricsRepoWide(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "internal/metrics/metrics.go"),
		"package metrics\n\nvar a = prometheus.NewCounter(prometheus.CounterOpts{Name: \"llmkube_reconcile_total\"})\n")
	writeFile(t, filepath.Join(root, "pkg/agent/metrics.go"), "package agent\n\n"+
		"var b = prometheus.NewGauge(prometheus.GaugeOpts{Name: \"llmkube_metal_agent_apple_power_gpu_watts\"})\n")
	gt, err := LoadGroundTruth("", root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"llmkube_reconcile_total", "llmkube_metal_agent_apple_power_gpu_watts"} {
		if !gt.Metrics[name] {
			t.Errorf("repo-wide metric scan missed %s; have %v", name, gt.Metrics)
		}
	}
}

func TestLoadGroundTruth_FromCRDBases(t *testing.T) {
	gt, err := LoadGroundTruth("testdata/crd-bases", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !gt.Groups["inference.llmkube.dev"] {
		t.Errorf("missing group inference.llmkube.dev; have %v", gt.Groups)
	}
	if !gt.Kinds["InferenceService"] {
		t.Errorf("missing kind InferenceService")
	}
	if !gt.SpecFields["InferenceService"]["modelRef"] || !gt.SpecFields["InferenceService"]["turboQuantBits"] {
		t.Errorf("missing spec fields; have %v", gt.SpecFields["InferenceService"])
	}
	if gt.SpecFields["InferenceService"]["bogusField"] {
		t.Errorf("invented field should not be present")
	}
}

func TestLoadGroundTruth_ScansChartResourceNames(t *testing.T) {
	dir := t.TempDir()
	// write a chart template with a Service metadata.name
	writeFile(t, filepath.Join(dir, "charts/llmkube/templates/metrics-service.yaml"),
		"apiVersion: v1\nkind: Service\nmetadata:\n  name: llmkube-controller-manager-metrics-service\n")
	gt, err := LoadGroundTruth("", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !gt.ChartResourceNames["llmkube-controller-manager-metrics-service"] {
		t.Fatal("chart Service name should be in ground truth")
	}
}
