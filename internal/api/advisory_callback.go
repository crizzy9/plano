package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/crizzy9/plano/internal/feeds"
)

func (s Server) advisoryCallback(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	timestamp := r.Header.Get("X-Plano-Timestamp")
	epoch, err := strconv.ParseInt(timestamp, 10, 64)
	signature, decodeErr := hex.DecodeString(r.Header.Get("X-Plano-Signature"))
	if err != nil || decodeErr != nil || len(signature) != sha256.Size || strconv.FormatInt(epoch, 10) != timestamp ||
		time.Unix(epoch, 0).Sub(now).Abs() > 5*time.Minute {
		respond(w, 401, map[string]string{"error": "invalid advisory signature"})
		return
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		respond(w, 415, map[string]string{"error": "expected application/json"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, feeds.MaxAdvisoryIngressBytes))
	if err != nil {
		respond(w, 413, map[string]string{"error": "could not read advisory within size limit"})
		return
	}
	mac := hmac.New(sha256.New, s.AdvisoryIngressKey)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		respond(w, 401, map[string]string{"error": "invalid advisory signature"})
		return
	}
	advisory, err := feeds.DecodeAdvisoryIngress(bytes.NewReader(body))
	if err != nil || advisory.ObservedAt.After(now.Add(30*time.Second)) || advisory.ObservedAt.Before(now.Add(-6*time.Hour)) {
		s.failure(w, errInvalid)
		return
	}
	if err := s.Store.PublishAdvisory(r.Context(), advisory, now); err != nil {
		s.failure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
