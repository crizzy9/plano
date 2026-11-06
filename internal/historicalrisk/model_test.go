package historicalrisk

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

const testHeader = "\ufeff\"FlightDate\",Origin,Dest,CRSDepTime,DepDelay,Cancelled,ArrDelay,\n"

func TestFitEstimateAndEvaluateBTSDepartureRisk(t *testing.T) {
	var training strings.Builder
	training.WriteString(testHeader)
	for range 200 {
		training.WriteString("2026-06-01,JFK,SFO,2400,15.00,0.00,-30,\n")
		training.WriteString("2026-06-01,JFK,SFO,0900,-3.00,0.00,120,\n")
	}
	training.WriteString("2026-06-02,JFK,SFO,0930,300,1.00,0,\n")
	training.WriteString("2026-06-02,JFK,SFO,0930,,0.00,0,\n")
	training.WriteString("2026-06-02,JFK,SFO,0930,NaN,0.00,0,\n")
	training.WriteString("2026-06-02,JFK,SFO,2460,30,0.00,0,\n")
	published := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC)
	model, err := Fit(strings.NewReader(training.String()), published)
	if err != nil {
		t.Fatal(err)
	}
	var artifact bytes.Buffer
	if err := model.Save(&artifact); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(bytes.NewReader(artifact.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip bytes.Buffer
	if err := loaded.Save(&roundTrip); err != nil || !bytes.Equal(artifact.Bytes(), roundTrip.Bytes()) || model.id != loaded.id {
		t.Fatalf("round trip changed artifact or identity, err=%v", err)
	}

	local := time.FixedZone("JFK fixture", -4*60*60)
	asOf := published.AddDate(0, 0, 1)
	departure := time.Date(2026, time.August, 13, 0, 30, 0, 0, local)
	estimate, ok := loaded.Estimate("JFK", "SFO", departure, asOf)
	if !ok || math.Abs(estimate.Probability-5.0/6) > 1e-12 || estimate.SampleSize != 200 ||
		estimate.TrainingThrough != "2026-06-01" || estimate.ModelVersion != model.id {
		t.Fatalf("midnight estimate = %+v, supported=%v", estimate, ok)
	}
	for _, tc := range []struct {
		name, destination string
		departure, asOf   time.Time
	}{
		{"unknown route", "LHR", departure, asOf},
		{"publication day", "SFO", departure, asOf.Add(-time.Nanosecond)},
		{"local publication day", "SFO", departure.Add(-time.Hour), asOf},
		{"sparse window", "SFO", departure.Add(3 * time.Hour), asOf},
		{"already departed", "SFO", departure, departure},
		{"expired", "SFO", departure.AddDate(1, 0, 0), asOf.AddDate(1, 0, 0)},
		{"departure beyond expiration", "SFO", departure.AddDate(1, 0, 0), asOf},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := loaded.Estimate("JFK", tc.destination, tc.departure, tc.asOf); ok {
				t.Fatal("unsupported flight received an estimate")
			}
		})
	}

	var holdout strings.Builder
	holdout.WriteString(testHeader)
	holdout.WriteString("2026-08-12,JFK,SFO,0900,200,0,0,\n")
	holdout.WriteString("2027-08-13,JFK,SFO,0900,200,0,0,\n")
	for range 200 {
		holdout.WriteString("2026-08-13,JFK,SFO,0000,30,0,0,\n")
		holdout.WriteString("2026-08-14,JFK,SFO,0900,0,0,0,\n")
	}
	holdout.WriteString("2026-08-15,JFK,LHR,0900,20,0,0,\n")
	evaluation, err := Evaluate(loaded, strings.NewReader(holdout.String()))
	if err != nil || evaluation.TrainingRows != 400 || evaluation.HoldoutRows != 401 || evaluation.LearnedRows != 400 ||
		evaluation.ExcludedThroughPublication != 1 || evaluation.ExcludedAfterExpiration != 1 ||
		evaluation.HoldoutFrom != "2026-08-13" || evaluation.HoldoutThrough != "2026-08-15" ||
		evaluation.LearnedOrFallbackBrier >= evaluation.AirportOnlyBrier || evaluation.LearnedOrFallbackBrier >= evaluation.GlobalBrier {
		t.Fatalf("evaluation = %+v, err=%v", evaluation, err)
	}
}

func TestBTSReaderRejectsOversizedAndInvalidRecords(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"missing header", "FlightDate,Origin,Dest\n", "missing"},
		{"malformed date", testHeader + "2026-06-31,JFK,SFO,0900,0,0,0,\n", "invalid flight date"},
		{"oversized quoted field", testHeader + fmt.Sprintf("2026-06-01,JFK,SFO,0900,0,0,\"%s\",\n", strings.Repeat("x\n", maxRecordBytes/2)), "record exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := readFlights(strings.NewReader(tc.input), func(flightRow) error { return nil }); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
