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
	if len(args) > 0 && args[0] == "bts-risk" {
		return runBTSRisk(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("plano-train", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inputPath := flags.String("input", "-", "Historical examples as JSON Lines. Use - for stdin.")
	outputPath := flags.String("output", "", "New model artifact path. Use - for stdout.")
	evaluatePath := flags.String("evaluate-model", "", "Score an existing model against later examples and print JSON metrics to stdout.")
	holdout := flags.Float64("holdout", 0.2, "Fraction of newest forecasts reserved for evaluation.")
	ridge := flags.Float64("ridge", 0.01, "L2 penalty on standardized feature coefficients.")
	allowRegression := flags.Bool("allow-regression", false, "Write an experimental artifact even if the backtest fails the deployment gate.")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	var evaluate bool
	var trainingFlag string
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "evaluate-model":
			evaluate = true
		case "output", "holdout", "ridge", "allow-regression":
			trainingFlag = f.Name
		}
	})
	if flags.NArg() != 0 || !evaluate && *outputPath == "" {
		return errors.New("usage: plano-train -input examples.jsonl (-output delay-model.json | -evaluate-model delay-model.json)")
	}
	var model *predict.Model
	if evaluate {
		if trainingFlag != "" {
			return fmt.Errorf("-%s cannot be used with -evaluate-model", trainingFlag)
		}
		if *evaluatePath == "" {
			return errors.New("-evaluate-model requires an artifact path")
		}
		file, err := os.Open(*evaluatePath)
		if err != nil {
			return err
		}
		model, err = predict.Load(file)
		closeErr := file.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
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
	if evaluate {
		report, err := predict.Evaluate(model, rows)
		if err != nil {
			return err
		}
		return writeMetrics(stdout, report)
	}
	model, report, err := predict.Fit(rows, predict.Options{HoldoutFraction: *holdout, Ridge: *ridge})
	if err != nil {
		return err
	}
	if err := writeMetrics(stderr, report); err != nil {
		return err
	}
	if !*allowRegression && (report.EligibleRows < 8 || report.EligibleCoverage < 0.5 || report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Operational.MeanAbsoluteMinutes ||
		report.LearnedOrFallback.MeanAbsoluteMinutes >= report.Published.MeanAbsoluteMinutes) {
		return errors.New("backtest failed the deployment gate. Need at least eight signal-bearing holdout rows, 50% learned coverage among them, and lower all-flight holdout MAE than both baselines")
	}
	return writeArtifact(*outputPath, stdout, model.Save)
}

func writeArtifact(path string, stdout io.Writer, save func(io.Writer) error) error {
	if path == "-" {
		return save(stdout)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writeErr := save(file)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func writeMetrics(output io.Writer, report any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
