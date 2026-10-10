package process

import "testing"

func TestEmptyWritesDoNotMarkOutputTruncated(t *testing.T) {
	for _, limit := range []int64{0, 3} {
		buffer := newCappedBuffer(limit)
		if limit > 0 {
			if _, err := buffer.Write([]byte("abc")); err != nil {
				t.Fatal(err)
			}
		}
		if count, err := buffer.Write(nil); count != 0 || err != nil || buffer.Truncated() {
			t.Fatalf("empty write: count=%d error=%v truncated=%v", count, err, buffer.Truncated())
		}
		if count, err := buffer.Write([]byte("x")); count != 1 || err != nil || !buffer.Truncated() {
			t.Fatalf("discarded write: count=%d error=%v truncated=%v", count, err, buffer.Truncated())
		}
	}
}
