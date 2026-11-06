package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) advisoryCallback(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	body, ok := readSignedCallback(w, r, now, s.AdvisoryIngressKey, feeds.MaxAdvisoryIngressBytes, "advisory")
	if !ok {
		return
	}
	advisory, err := feeds.DecodeAdvisoryIngress(bytes.NewReader(body))
	if err != nil || !advisory.ObservationAllowed(now) {
		s.failure(w, errInvalid)
		return
	}
	if err := s.Store.PublishAdvisory(r.Context(), advisory, now); err != nil {
		s.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readSignedCallback(w http.ResponseWriter, r *http.Request, now time.Time, key []byte, maxBytes int64, subject string) ([]byte, bool) {
	timestamp := r.Header.Get("X-Plano-Timestamp")
	epoch, err := strconv.ParseInt(timestamp, 10, 64)
	signature, decodeErr := hex.DecodeString(r.Header.Get("X-Plano-Signature"))
	invalidSignature := "invalid " + subject + " signature"
	if err != nil || decodeErr != nil || len(signature) != sha256.Size || strconv.FormatInt(epoch, 10) != timestamp ||
		time.Unix(epoch, 0).Sub(now).Abs() > 5*time.Minute {
		respond(w, 401, map[string]string{"error": invalidSignature})
		return nil, false
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		respond(w, 415, map[string]string{"error": "expected application/json"})
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var oversized *http.MaxBytesError
		var timeout net.Error
		status, message := http.StatusServiceUnavailable, subject+" could not be read"
		switch {
		case errors.As(err, &oversized):
			status, message = http.StatusRequestEntityTooLarge, subject+" exceeds size limit"
		case errors.As(err, &timeout) && timeout.Timeout():
			status, message = http.StatusRequestTimeout, subject+" upload timed out"
		}
		respond(w, status, map[string]string{"error": message})
		return nil, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		respond(w, 401, map[string]string{"error": invalidSignature})
		return nil, false
	}
	return body, true
}
