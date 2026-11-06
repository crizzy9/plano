package historicalrisk

import (
	"bytes"
	_ "embed"
)

//go:embed bts-departure-risk-2026-06.json
var bundledModel []byte

// Bundled loads the historical model shipped with Plano.
func Bundled() (*Model, error) {
	return Load(bytes.NewReader(bundledModel))
}
