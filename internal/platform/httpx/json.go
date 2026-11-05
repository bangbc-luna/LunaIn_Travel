package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "err", err)
	}
}

// DecodeJSON decodes exactly one JSON value from the body into dst. The body is
// limited to 1 MiB and unknown fields are rejected. Any failure is ErrInvalidRequest.
func DecodeJSON(r *http.Request, dst any) error {
	_, err := decode(r, dst, false)
	return err
}

// DecodeOptionalJSON is DecodeJSON for an optional body: an empty (or
// whitespace-only) body, chunked or not, leaves dst untouched and reports
// false.
func DecodeOptionalJSON(r *http.Request, dst any) (bool, error) {
	return decode(r, dst, true)
}

func decode(r *http.Request, dst any, optional bool) (bool, error) {
	if r.Body == nil {
		if optional {
			return false, nil
		}
		return false, ErrInvalidRequest
	}
	// A nil ResponseWriter is allowed by MaxBytesReader.
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, ErrInvalidRequest
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return false, ErrInvalidRequest
	}
	return true, nil
}
