package client

import (
	"bytes"
	"testing"
)

func TestSyncTeXWindowsDriveAndUNCPaths(t *testing.T) {
	for _, root := range []struct{ local, want string }{
		{`C:\Users\Researcher\paper`, "C:/Users/Researcher/paper"},
		{`\\server\share\paper`, "//server/share/paper"},
	} {
		var output bytes.Buffer
		input := []byte("Input:1:/work/project/chapters/body.tex\nInput:2:/texmf/article.cls\n")
		err := writeRebasedSyncTeX(&output, input, "/work/project", root.local, 4096)
		want := "Input:1:" + root.want + "/chapters/body.tex\nInput:2:/texmf/article.cls\n"
		if err != nil || output.String() != want {
			t.Fatalf("mapping %q = %q, %v; want %q", root.local, output.String(), err, want)
		}
	}
}
