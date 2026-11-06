package main

import (
	"archive/zip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crizzy9/plano/internal/historicalrisk"
)

const maxBTSCSVBytes = 512 << 20

func runBTSRisk(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("plano-train bts-risk", flag.ContinueOnError)
	flags.SetOutput(stderr)
	training := flags.String("training", "", "BTS training CSV or monthly ZIP archive.")
	published := flags.String("published-on", "", "Training archive publication date (YYYY-MM-DD).")
	holdout := flags.String("holdout", "", "Later BTS holdout CSV or monthly ZIP archive.")
	output := flags.String("output", "", "New historical risk artifact path. Use - for stdout.")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *training == "" || *holdout == "" || *output == "" {
		return errors.New("usage: plano-train bts-risk -training june.zip -published-on YYYY-MM-DD -holdout august.zip -output risk.json")
	}
	publishedOn, err := time.Parse(time.DateOnly, *published)
	if err != nil {
		return errors.New("-published-on must be the BTS training archive publication date (YYYY-MM-DD)")
	}
	var model *historicalrisk.Model
	if err := withBTSCSV(*training, func(reader io.Reader) error {
		var err error
		model, err = historicalrisk.Fit(reader, publishedOn)
		return err
	}); err != nil {
		return fmt.Errorf("fit historical departure risk: %w", err)
	}
	var report historicalrisk.Evaluation
	if err := withBTSCSV(*holdout, func(reader io.Reader) error {
		var err error
		report, err = historicalrisk.Evaluate(model, reader)
		return err
	}); err != nil {
		return fmt.Errorf("evaluate historical departure risk: %w", err)
	}
	if err := writeMetrics(stderr, report); err != nil {
		return err
	}
	if report.LearnedCoverage < 0.8 || report.LearnedOrFallbackBrier >= report.AirportOnlyBrier || report.LearnedOrFallbackBrier >= report.GlobalBrier {
		return errors.New("BTS holdout failed the deployment gate: need at least 80% coverage and a lower Brier score than both baselines")
	}
	return writeArtifact(*output, stdout, model.Save)
}

func withBTSCSV(path string, read func(io.Reader) error) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxBTSCSVBytes {
			return errors.New("BTS CSV must be a regular file of at most 512 MiB")
		}
		return read(file)
	case ".zip":
		archive, err := zip.OpenReader(path)
		if err != nil {
			return err
		}
		defer archive.Close()
		var csv *zip.File
		for _, member := range archive.File {
			if !strings.EqualFold(filepath.Ext(member.Name), ".csv") {
				continue
			}
			if csv != nil || !member.Mode().IsRegular() || member.UncompressedSize64 > maxBTSCSVBytes {
				return errors.New("BTS archive must contain a single regular CSV member of at most 512 MiB")
			}
			csv = member
		}
		if csv == nil {
			return errors.New("BTS archive has no CSV member")
		}
		member, err := csv.Open()
		if err != nil {
			return err
		}
		defer member.Close()
		return read(member)
	default:
		return errors.New("BTS input must be a .csv or .zip file")
	}
}
