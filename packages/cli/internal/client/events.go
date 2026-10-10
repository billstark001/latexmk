package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

const maxSessionEventBytes = 64 << 10

// eventLineSplitter accepts LF, CRLF and CR, including a CRLF split across
// reads. CR ends a line immediately; a following LF is skipped, so a quiet
// connection never delays dispatch waiting to decide which ending was used.
func eventLineSplitter() bufio.SplitFunc {
	skipLF := false
	return func(data []byte, atEOF bool) (int, []byte, error) {
		offset := 0
		if skipLF && len(data) > 0 {
			skipLF = false
			if data[0] == '\n' {
				data = data[1:]
				offset = 1
			}
		}
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			skipLF = data[i] == '\r'
			return offset + i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return offset + len(data), data, nil
		}
		return offset, nil, nil
	}
}

func readSessionEvents(reader io.Reader, after uint64, receive func(protocol.SessionEvent)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Split(eventLineSplitter())
	scanner.Buffer(make([]byte, 4096), maxSessionEventBytes)
	var data strings.Builder
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			var event protocol.SessionEvent
			if err := json.Unmarshal([]byte(strings.TrimSuffix(data.String(), "\n")), &event); err != nil {
				return fmt.Errorf("invalid session event: %w", err)
			}
			data.Reset()
			if event.Type == "resync" || event.Sequence > after {
				after = event.Sequence
				receive(event)
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if len(value)+1 > maxSessionEventBytes-data.Len() {
			return fmt.Errorf("session event exceeds %d bytes", maxSessionEventBytes)
		}
		data.WriteString(value)
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}
