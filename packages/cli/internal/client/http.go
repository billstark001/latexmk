package client

import (
	"io"

	"github.com/billstark001/latexmk/packages/shared/jsonutil"
)

// decodeAPIResponse bounds the entire body, including trailing whitespace.
// Metadata permits additive fields; other API responses enforce the schema.
func decodeAPIResponse(reader io.Reader, maxBytes int64, output any, strict bool) error {
	if strict {
		return jsonutil.DecodeStrict(reader, maxBytes, output)
	}
	return jsonutil.Decode(reader, maxBytes, output)
}
