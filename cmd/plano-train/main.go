package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/crizzy9/plano/internal/predict"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("plano-train", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inputPath := flags.String("input", "-", "Historical examples as JSON Lines. Use - for stdin.")
	outputPath := flags.String("output", "", "New model artifact path. Use - for stdout.")
	holdout := flags.Float64("holdout", 0.2, "Fraction of newest forecasts reserved for evaluation.")
	ridge := flags.Float64("ridge", 0.01, "L2 penalty on standardized feature coefficients.")
	allowRegression := flags.Bool("allow-regression", false, "Write an experimental artifact even if the backtest fails the deployment gate.")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *outputPath == "" || flags.NArg() != 0 {
		return errors.New("usage: plano-train -input examples.jsonl -output delay-model.json")
	}
	input := stdin
	if *inputPath != "-" {
		file, err := os.Open(*inputPath)
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	}
	rows, err := predict.ReadExamples(input)
	if err != nil {
		return err
	}
	model, report, err := predict.Fit(rows, predict.Options{HoldoutFraction: *holdout, Ridge: *ridge})
	if err != nil {
		return err
	}
	metrics := json.NewEncoder(stderr)
	metrics.SetIndent("", "  ")
	if err := metrics.Encode(report); err != nil {
		return err
	}
	if !*allowRegression && (report.LearnedCoverage < 0.5 || report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Operational.MeanAbsoluteMinutes ||
		report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Published.MeanAbsoluteMinutes) {
		return errors.New("backtest failed the deployment gate: need at least 50% learned coverage and lower holdout MAE than both baselines")
	}
	if *outputPath == "-" {
		return model.Save(stdout)
	}
	file, err := os.OpenFile(*outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writeErr := model.Save(file)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(*outputPath)
		return err
	}
	return nil
}
