// Package jsonutil validates bounded JSON envelopes shared by the client and server.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// Decode reads exactly one non-null JSON value into destination. maxBytes bounds
// the entire input, including trailing whitespace; negative and MaxInt64 limits
// are invalid. Unknown object fields are accepted for additive protocol changes.
// Destination may have been partially populated when decoding fails.
func Decode(reader io.Reader, maxBytes int64, destination any) error {
	return decode(reader, maxBytes, destination, false)
}

// DecodeStrict has Decode's envelope limits and rejects unknown object fields.
func DecodeStrict(reader io.Reader, maxBytes int64, destination any) error {
	return decode(reader, maxBytes, destination, true)
}

func decode(reader io.Reader, maxBytes int64, destination any, strict bool) error {
	payload, err := safefs.ReadLimited(reader, maxBytes)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return errors.New("JSON value must not be null")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON body contains trailing data")
	}
	return nil
}
