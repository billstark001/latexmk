package protocol

import "testing"

func TestBearerTokenTransportBoundaries(t *testing.T) {
	for _, token := range []string{"", "two tokens", " leading", "trailing ", "tab\tvalue", "line\nvalue", "nul\x00value", "del\x7fvalue", "unicode\u00a0space"} {
		if ValidBearerToken(token) {
			t.Errorf("accepted %q", token)
		}
	}
	for _, token := range []string{"opaque-token", "a+b/c=d!", "非ASCII凭据"} {
		if !ValidBearerToken(token) {
			t.Errorf("rejected %q", token)
		}
	}
}
