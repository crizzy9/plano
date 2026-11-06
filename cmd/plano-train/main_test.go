package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crizzy9/plano/internal/predict"
)

func exampleJSONL(t *testing.T, improves bool) []byte {
	t.Helper()
	return exampleCohortJSONL(t, improves, 40, 40)
}

func exampleCohortJSONL(t *testing.T, improves bool, totalRows, signalRows int) []byte {
	t.Helper()
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := range totalRows {
		observed := start.Add(time.Duration(i) * 3 * time.Hour)
		departure := observed.Add(time.Hour)
		published := 10 + (i%7)*5
		delay := published
		if improves {
			delay = 12 + 2*published
		}
		if i >= signalRows {
			published, delay = 0, 0
		}
		row := predict.Example{
			FlightID: fmt.Sprintf("synthetic-%d", i), ObservedAt: observed, ScheduledDeparture: departure,
			DepartedAt: departure.Add(time.Duration(delay) * time.Minute),
			Features:   predict.Features{AirlineDelayMinutes: float64(published)},
		}
		if err := encoder.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	return input.Bytes()
}

func TestTrainCLIWritesModelAndHoldoutMetrics(t *testing.T) {
	var stdout, stderr bytes.Buffer
	input := exampleJSONL(t, true)
	if err := run([]string{"-output", "-"}, bytes.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, &stderr)
	}
	model, err := predict.Load(&stdout)
	if err != nil {
		t.Fatal(err)
	}
	var report predict.Backtest
	if err := json.Unmarshal(stderr.Bytes(), &report); err != nil || report.ModelVersion != model.ID() || report.LearnedCoverage != 1 || report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Operational.MeanAbsoluteMinutes {
		t.Fatalf("metrics = %+v, %v", report, err)
	}

	output := filepath.Join(t.TempDir(), "existing.json")
	if err := os.WriteFile(output, []byte("existing artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if err := run([]string{"-output", output}, bytes.NewReader(input), &stdout, &stderr); err == nil {
		t.Fatal("trainer overwrote an existing artifact")
	}
	if raw, err := os.ReadFile(output); err != nil || string(raw) != "existing artifact" {
		t.Fatalf("existing artifact = %q, %v", raw, err)
	}
}

func TestTrainCLIFailsQualityGateWithoutWritingAnArtifact(t *testing.T) {
	var stdout, stderr bytes.Buffer
	input := exampleJSONL(t, false)
	err := run([]string{"-output", "-"}, bytes.NewReader(input), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "deployment gate") || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("gate result = stdout %q, metrics %q, error %v", &stdout, &stderr, err)
	}
	stderr.Reset()
	if err := run([]string{"-output", "-", "-allow-regression"}, bytes.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := predict.Load(&stdout); err != nil {
		t.Fatalf("experimental artifact: %v", err)
	}
}

func TestTrainCLIGatesCoverageOnSignalBearingHoldoutRows(t *testing.T) {
	const trainingRows, holdoutRows = 80, 20
	for _, eligibleRows := range []int{8, 7} {
		t.Run(fmt.Sprintf("%d eligible", eligibleRows), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			input := exampleCohortJSONL(t, true, trainingRows+holdoutRows, trainingRows+eligibleRows)
			err := run([]string{"-output", "-"}, bytes.NewReader(input), &stdout, &stderr)
			if eligibleRows == 8 {
				if err != nil {
					t.Fatalf("eligible cohort failed to deploy: %v\n%s", err, &stderr)
				}
				if _, err := predict.Load(&stdout); err != nil {
					t.Fatalf("deployment artifact: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "deployment gate") || stdout.Len() != 0 {
				t.Fatalf("low-support gate returned stdout %q and error %v", &stdout, err)
			}
			var report predict.Backtest
			if err := json.Unmarshal(stderr.Bytes(), &report); err != nil || report.HoldoutRows != holdoutRows ||
				report.EligibleRows != eligibleRows || report.EligibleCoverage != 1 ||
				report.LearnedCoverage != float64(eligibleRows)/holdoutRows ||
				report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Operational.MeanAbsoluteMinutes ||
				report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Published.MeanAbsoluteMinutes {
				t.Fatalf("metrics = %+v, %v", report, err)
			}
		})
	}
}

func TestEvaluateCLIPrintsReportWithoutWritingModel(t *testing.T) {
	input := exampleJSONL(t, true)
	artifactPath := filepath.Join(t.TempDir(), "existing.json")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-output", artifactPath}, bytes.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("fit fixture: %v\n%s", err, &stderr)
	}
	before, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	model, err := predict.Load(bytes.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if err := run([]string{"-evaluate-model", artifactPath, "-input", "-"}, bytes.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("evaluate: %v\n%s", err, &stderr)
	}
	var report predict.Evaluation
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.ModelVersion != model.ID() ||
		report.ExcludedThroughTraining != 32 || report.HoldoutRows != 8 || report.LearnedCoverage != 1 || stderr.Len() != 0 {
		t.Fatalf("stdout report = %+v, stderr = %q, error = %v", report, &stderr, err)
	}
	if after, err := os.ReadFile(artifactPath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("evaluation changed artifact: %v", err)
	}
}

func TestEvaluateCLIRejectsTrainingFlagsAndMissingArtifact(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"-evaluate-model", missing, "-output", "-"}, "-output cannot be used"},
		{[]string{"-evaluate-model", missing, "-holdout", "0.2"}, "-holdout cannot be used"},
		{[]string{"-evaluate-model", missing, "-ridge", "0.01"}, "-ridge cannot be used"},
		{[]string{"-evaluate-model", missing, "-allow-regression=false"}, "-allow-regression cannot be used"},
		{[]string{"-evaluate-model", ""}, "requires an artifact path"},
		{[]string{"-evaluate-model", missing}, "missing.json"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(test.args, strings.NewReader(""), &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), test.want) || stdout.Len() != 0 {
			t.Errorf("evaluate %v = stdout %q, error %v", test.args, &stdout, err)
		}
	}
}
