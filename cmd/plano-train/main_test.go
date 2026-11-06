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
	var input bytes.Buffer
	encoder := json.NewEncoder(&input)
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i := range 40 {
		observed := start.Add(time.Duration(i) * 3 * time.Hour)
		departure := observed.Add(time.Hour)
		published := 10 + (i%7)*5
		delay := published
		if improves {
			delay = 12 + 2*published
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
