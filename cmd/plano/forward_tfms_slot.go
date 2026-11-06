package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/crizzy9/plano/internal/feeds"
)

func tfmsATFMSlotRelayInput(input io.Reader, bindingPath string, now time.Time) (io.Reader, int, error) {
	binding, err := readTFMSATFMBinding(bindingPath)
	if err != nil {
		return nil, relayConfigError, err
	}
	slot, err := feeds.DecodeTFMSATFMSlot(input, binding, now)
	switch {
	case errors.Is(err, feeds.ErrNotFound):
		// A restricted record has no normalized envelope to send.
		return nil, 0, nil
	case errors.Is(err, feeds.ErrUnavailable):
		return nil, relayTempFailure, errors.New("could not read FAA FlightControl XML from standard input")
	case err != nil:
		return nil, relayDataError, errors.New("standard input did not yield a valid, bound FAA FlightControl slot message")
	}
	body, err := json.Marshal(slot)
	if err != nil {
		return nil, relayDataError, errors.New("could not encode the FAA slot")
	}
	return bytes.NewReader(body), 0, nil
}

func readTFMSATFMBinding(path string) (feeds.TFMSATFMSlotBinding, error) {
	const maxBindingBytes = 4 << 10
	invalid := errors.New("FAA FlightControl binding must be a readable JSON file of at most 4 KiB")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBindingBytes {
		return feeds.TFMSATFMSlotBinding{}, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return feeds.TFMSATFMSlotBinding{}, invalid
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxBindingBytes+1))
	if err != nil || len(body) > maxBindingBytes || !utf8.Valid(body) {
		return feeds.TFMSATFMSlotBinding{}, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var binding feeds.TFMSATFMSlotBinding
	if decoder.Decode(&binding) != nil || decoder.Decode(new(any)) != io.EOF || binding.Version != 1 {
		return feeds.TFMSATFMSlotBinding{}, invalid
	}
	return binding, nil
}
