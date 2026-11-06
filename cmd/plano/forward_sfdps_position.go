package main

import (
	"errors"
	"io"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func sfdpsPositionRelayInput(input io.Reader, bindingPath string, now time.Time) (io.Reader, int, error) {
	invalid := errors.New("FAA SFDPS binding must be a readable version 1 JSON file of at most 4 KiB with displayAllowed enabled")
	var binding feeds.SFDPSPositionBinding
	if err := readRelayBinding(bindingPath, &binding, invalid); err != nil || binding.Version != 1 || !binding.DisplayAllowed {
		return nil, relayConfigError, invalid
	}
	report, err := feeds.DecodeSFDPSPositions(input, binding, now)
	switch {
	case errors.Is(err, feeds.ErrNotFound):
		return nil, 0, nil
	case errors.Is(err, feeds.ErrUnavailable):
		return nil, relayTempFailure, errors.New("could not read FAA SFDPS XML from standard input")
	case err != nil:
		return nil, relayDataError, errors.New("standard input did not yield a valid, bound FAA SFDPS position report")
	}
	return encodeRadarPositionRelay(report)
}
