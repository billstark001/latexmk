package httpapi

import (
	"strings"
	"testing"
)

func TestStrictJSONRejectsNullAndInvalidEnvelopes(t *testing.T) {
	for _, payload := range []string{"null", " null\n", "{} {}", "{} garbage", `{"unknown":true}`, "{}" + strings.Repeat(" ", 64)} {
		t.Run(payload[:min(len(payload), 20)], func(t *testing.T) {
			var request struct {
				Name string `json:"name"`
			}
			if err := decodeStrictJSON(strings.NewReader(payload), 64, &request); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}
