package main

import (
	"errors"
	"io"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func cat062PositionRelayInput(input io.Reader, bindingPath string, now time.Time) (io.Reader, int, error) {
	invalid := errors.New("EUROCONTROL CAT062 binding must be a readable version 1 JSON file of at most 4 KiB with edition 1.21, displayAllowed and observedAirborne")
	var binding feeds.CAT062PositionBinding
	if err := readRelayBinding(bindingPath, &binding, invalid); err != nil ||
		binding.Version != 1 || binding.Edition != "1.21" || !binding.DisplayAllowed || !binding.ObservedAirborne {
		return nil, relayConfigError, invalid
	}
	report, err := feeds.DecodeCAT062Positions(input, binding, now)
	switch {
	case errors.Is(err, feeds.ErrNotFound):
		return nil, 0, nil
	case errors.Is(err, feeds.ErrUnavailable):
		return nil, relayTempFailure, errors.New("could not read EUROCONTROL CAT062 blocks from standard input")
	case err != nil:
		return nil, relayDataError, errors.New("standard input did not yield a valid, bound EUROCONTROL CAT062 position report")
	}
	return encodeRadarPositionRelay(report)
}
