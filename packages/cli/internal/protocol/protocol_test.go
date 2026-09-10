package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAuxiliaryPolicyRoundTrip(t *testing.T) {
	for _, mode := range []string{"", "none", "retain", "reuse"} {
		data, err := json.Marshal(CompileRequest{Entry: "main.tex", Auxiliary: AuxiliaryOptions{Server: mode}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"auxiliary"`) != (mode != "") {
			t.Fatalf("unexpected wire format %s", data)
		}
		var roundtrip CompileRequest
		if err := json.Unmarshal(data, &roundtrip); err != nil {
			t.Fatal(err)
		}
		if mode != "" && roundtrip.Auxiliary.Server != mode {
			t.Fatal("lost cache mode")
		}
	}
}
