package client

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

func TestLogBudgetIncludesUTF8Repair(t *testing.T) {
	for _, payload := range [][]byte{
		{0xff, 'a', 0xff, 'b'},
		[]byte("中文日志"),
	} {
		archive := buildResultArchive(t, []tarEntry{{name: "stdout.log", payload: payload}})
		for budget := int64(1); budget <= int64(len(payload)+3); budget++ {
			entries, returned, err := readBoundedLogs(bytes.NewReader(archive), "stdout", 10, budget, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if returned > budget || len(entries) != 1 || !utf8.ValidString(entries[0].Content) {
				t.Fatalf("budget=%d returned=%d entries=%+v", budget, returned, entries)
			}
		}
	}
}

func TestLogsRejectDuplicateSelectedMembers(t *testing.T) {
	archive := buildResultArchive(t, []tarEntry{
		{name: "stdout.log", payload: []byte("first")},
		{name: "stdout.log", payload: []byte("again")},
	})
	if _, _, err := readBoundedLogs(bytes.NewReader(archive), "stdout", 10, 5, 1, nil); err == nil {
		t.Fatal("duplicate logs can exceed the advertised shared byte budget")
	}
}
