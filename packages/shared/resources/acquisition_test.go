package resources

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

func TestCanonicalRetryAfterRejectsUnboundedOrNonHTTPValues(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"00030", "30"},
		{"0", "0"},
		{"Wed, 21 Oct 2015 07:28:00 GMT", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"-1", ""},
		{"1h", ""},
		{" 30 ", ""},
		{"30\r\nX-Secret: value", ""},
		{strings.Repeat("9", 129), ""},
		{"9223372036854775808", ""},
	} {
		if got := CanonicalRetryAfter(test.input); got != test.want {
			t.Fatalf("Retry-After %q canonicalized as %q, want %q", test.input, got, test.want)
		}
	}
}

func TestFetchRejectsOverflowingContentLimitBeforeOpeningDestination(t *testing.T) {
	_, pkg := archiveFixture()
	_, err := FetchArchive(context.Background(), ArchiveFetchOptions{
		Repository: "https://snapshot.example", Package: pkg, MaxBytes: math.MaxInt64,
	}, func(func(io.Writer) error) error {
		t.Fatal("invalid limit opened a destination")
		return nil
	})
	var failure *AcquisitionError
	if !errors.As(err, &failure) || failure.Code != "resource_limit_exceeded" || failure.FallbackAllowed {
		t.Fatalf("overflowing limit not rejected: %#v %v", failure, err)
	}
}
