package dependency

import "testing"

func TestScanUnterminatedPlotControlSequence(t *testing.T) {
	// An unfinished edit in watch/realtime mode must not crash selection.
	for _, source := range []string{`\addplot coordinates \`, `\addplot \`, `\addplot+ [red] \`} {
		scanInvocations(source)
	}
}

func FuzzScanInvocations(f *testing.F) {
	for _, source := range []string{
		`\input{chapter}`, `\includegraphics[width={1,2}]{plot}`,
		`\begin{filecontents*}{generated.tex}\input{body}\end{filecontents*}`,
		`\addplot coordinates \`, `\input a% comment` + "\n.tex",
		`\verb|\input{ignored}|`, "\\", "}", "\x00\xff",
	} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		scanInvocations(source)
	})
}
