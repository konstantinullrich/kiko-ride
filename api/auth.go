package api

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiko-backend/multisig"
)

const (
	// HeaderDriverSignature carries the hex Ed25519 signature of
	// DriverMessage by the driver's account key.
	HeaderDriverSignature = "X-Driver-Signature"
	// HeaderDriverTimestamp carries the Unix time in seconds the request was
	// signed at.
	HeaderDriverTimestamp = "X-Driver-Timestamp"
)

// MaxSignatureAge is how far a driver's X-Driver-Timestamp may be from the
// server's clock, in either direction.
const MaxSignatureAge = 30 * time.Second

// maxBodyBytes caps request bodies.
const maxBodyBytes = 1 << 20

// DriverMessage is what a driver signs for a request: a fixed prefix, the
// timestamp, the method, the request target (path and query, exactly as
// sent) and the raw body, separated by newlines. Signing the method and
// target keeps a signature from being replayed against another ride.
func DriverMessage(timestamp int64, method, target string, body []byte) []byte {
	head := "kiko-driver-request\n" + strconv.FormatInt(timestamp, 10) + "\n" + method + "\n" + target + "\n"
	return append([]byte(head), body...)
}

// readBody reads the request body and puts it back for the handler.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body: "+err.Error())
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// checkSignature verifies the request's X-Driver-Signature by key over
// DriverMessage, writing 401 if it is missing, stale or wrong.
func (s *Server) checkSignature(w http.ResponseWriter, r *http.Request, key string, body []byte) bool {
	fail := func(reason string) bool {
		s.log.Warn("driver signature refused", "driver", key, "method", r.Method, "path", r.URL.Path, "reason", reason)
		writeError(w, http.StatusUnauthorized, reason)
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get(HeaderDriverTimestamp)), 10, 64)
	if err != nil {
		return fail(HeaderDriverTimestamp + " header must be the Unix time in seconds")
	}
	if skew := time.Since(time.Unix(ts, 0)); skew > MaxSignatureAge || skew < -MaxSignatureAge {
		return fail(HeaderDriverTimestamp + " must be within " + MaxSignatureAge.String() + " of the server's time")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(r.Header.Get(HeaderDriverSignature)))
	if err != nil || len(sig) == 0 {
		return fail(HeaderDriverSignature + " header must be a hex signature")
	}
	target := r.RequestURI
	if target == "" {
		target = r.URL.RequestURI()
	}
	if err := multisig.VerifyMessage(key, DriverMessage(ts, r.Method, target, body), sig); err != nil {
		return fail(HeaderDriverSignature + " does not verify against the driver's key")
	}
	return true
}
