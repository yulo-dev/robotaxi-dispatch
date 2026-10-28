package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	headerReplayed       = "Idempotent-Replayed"
	maxBodyBytes         = 64 << 10
	maxKeyLen            = 200
)

type ctxKey int

const idemKey ctxKey = iota

type idempotency struct {
	Key  string
	Hash string
	Body []byte
}

// RequestHash returns a hash of the canonical JSON form of body: whitespace
// and object key order do not change the hash.
func RequestHash(body []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if dec.More() {
		return "", errors.New("trailing data after JSON value")
	}
	canon, err := json.Marshal(v) // map keys are sorted
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// requireIdempotency rejects requests without an Idempotency-Key header and
// stores the key, body, and canonical body hash in the request context.
func requireIdempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(headerIdempotencyKey)
		if key == "" || len(key) > maxKeyLen {
			writeError(w, http.StatusBadRequest, "idempotency_key_required",
				"Idempotency-Key header is required (1 to 200 characters)")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil || len(body) > maxBodyBytes {
			writeError(w, http.StatusBadRequest, "invalid_body", "request body unreadable or too large")
			return
		}
		hash, err := RequestHash(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		ctx := context.WithValue(r.Context(), idemKey, idempotency{Key: key, Hash: hash, Body: body})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func idempotencyFrom(ctx context.Context) (idempotency, bool) {
	v, ok := ctx.Value(idemKey).(idempotency)
	return v, ok
}
