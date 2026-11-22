package main

import (
	"bytes"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// Expected outcomes were captured from the fixed-argument entry point before
// migration. The comparison data does not keep a second argv parser alive.
func TestInternalCommandLibraryPreservesDefinedInvocationContract(t *testing.T) {
	for _, test := range []struct {
		args []string
		want internalInvocation
		err  error
	}{
		{nil, internalInvocation{}, nil},
		{[]string{"compile-worker", "1", "2"}, internalInvocation{command: "compile-worker", maxBytes: 1, maxFiles: 2}, nil},
		{[]string{"compile-worker", "08", "+2"}, internalInvocation{command: "compile-worker", maxBytes: 8, maxFiles: 2}, nil},
		{[]string{"resource-font-scan", "08"}, internalInvocation{command: "resource-font-scan", maxBytes: 8}, nil},
		{[]string{"resource-font-scan", "+8"}, internalInvocation{command: "resource-font-scan", maxBytes: 8}, nil},
		{[]string{"compile-worker", strconv.FormatInt(math.MaxInt64, 10), strconv.Itoa(math.MaxInt)}, internalInvocation{command: "compile-worker", maxBytes: math.MaxInt64, maxFiles: math.MaxInt}, nil},
		{[]string{"resource-font-scan", strconv.FormatInt(math.MaxInt64, 10)}, internalInvocation{command: "resource-font-scan", maxBytes: math.MaxInt64}, nil},
		{[]string{"compile-worker"}, internalInvocation{}, errServerInvocation},
		{[]string{"compile-worker", "1"}, internalInvocation{}, errServerInvocation},
		{[]string{"compile-worker", "1", "2", "3"}, internalInvocation{}, errServerInvocation},
		{[]string{"resource-font-scan"}, internalInvocation{}, errServerInvocation},
		{[]string{"resource-font-scan", "1", "2"}, internalInvocation{}, errServerInvocation},
		{[]string{"other"}, internalInvocation{}, errServerInvocation},
		{[]string{"--help"}, internalInvocation{}, errServerInvocation},
		{[]string{"--unknown"}, internalInvocation{}, errServerInvocation},
		{[]string{"compile-worker", "0", "1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "1", "0"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "-1", "1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "1", "-1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "0x10", "1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "1", "0x10"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "bad", "1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "1", "bad"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "9223372036854775808", "1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"compile-worker", "1", "9223372036854775808"}, internalInvocation{}, errWorkerLimits},
		{[]string{"resource-font-scan", "0"}, internalInvocation{}, errFontLimit},
		{[]string{"resource-font-scan", "-1"}, internalInvocation{}, errFontLimit},
		{[]string{"resource-font-scan", "bad"}, internalInvocation{}, errFontLimit},
		{[]string{"resource-font-scan", "0x10"}, internalInvocation{}, errFontLimit},
		{[]string{"resource-font-scan", "9223372036854775808"}, internalInvocation{}, errFontLimit},
		{[]string{"compile-worker", "--", "1"}, internalInvocation{}, errWorkerLimits},
		{[]string{"resource-font-scan", "--"}, internalInvocation{}, errFontLimit},
		{[]string{"compile-worker", "--limit", "1"}, internalInvocation{}, errWorkerLimits},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			got, err := parseInternalInvocation(test.args)
			if got != test.want || !errors.Is(err, test.err) {
				t.Fatalf("want=%+v/%v got=%+v/%v", test.want, test.err, got, err)
			}
		})
	}
}

type failingInput struct{}

func (failingInput) Read([]byte) (int, error) { return 0, errors.New("font input failed") }

func TestInternalCommandExitContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if handled, code := runInternal(nil, strings.NewReader(""), &stdout, &stderr); handled || code != 0 {
		t.Fatalf("normal server startup handled=%v code=%d", handled, code)
	}
	if handled, code := runInternal(
		[]string{"compile-worker", "bad", "1"},
		strings.NewReader(""),
		&stdout,
		&stderr,
	); !handled || code != 2 ||
		stderr.String() != "invalid worker limits\n" {
		t.Fatalf("usage exit handled=%v code=%d stderr=%q", handled, code, stderr.String())
	}
	stderr.Reset()
	if handled, code := runInternal(
		[]string{"resource-font-scan", "1"},
		failingInput{},
		&stdout,
		&stderr,
	); !handled || code != 1 ||
		!strings.Contains(stderr.String(), "font input failed") {
		t.Fatalf("runtime exit handled=%v code=%d stderr=%q", handled, code, stderr.String())
	}
}
