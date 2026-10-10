package engine

// Default contains the built-in drivers and application-registered extensions.
var Default = builtinRegistry()

type latexmkDriver struct {
	args       []string
	extensions []string
	probe      Command
}

func (d latexmkDriver) LatexmkArgs() []string        { return append([]string(nil), d.args...) }
func (d latexmkDriver) GraphicsExtensions() []string { return append([]string(nil), d.extensions...) }
func (d latexmkDriver) VersionProbe() Command {
	return Command{Name: d.probe.Name, Args: append([]string(nil), d.probe.Args...)}
}

func builtinRegistry() *Registry {
	registry := NewRegistry()
	// Ordered graphics-def defaults. Preserve suffix case for Linux filesystems.
	pdfExtensions := []string{
		".pdf",
		".png",
		".jpg",
		".mps",
		".jpeg",
		".jbig2",
		".jb2",
		".PDF",
		".PNG",
		".JPG",
		".JPEG",
		".JBIG2",
		".JB2",
	}
	for name, driver := range map[string]latexmkDriver{
		"pdflatex": {
			args:       []string{"-pdf"},
			extensions: pdfExtensions,
			probe:      Command{Name: "pdflatex", Args: []string{"--version"}},
		},
		"lualatex": {
			args:       []string{"-lualatex", "-pdflualatex=lualatex --safer --nosocket %O %S"},
			extensions: pdfExtensions,
			probe:      Command{Name: "lualatex", Args: []string{"--version"}},
		},
		"xelatex": {
			args: []string{"-xelatex"},
			extensions: []string{
				".pdf", ".PDF", ".ai", ".AI", ".png", ".PNG", ".jpg", ".JPG", ".jpeg", ".JPEG",
				".jp2", ".JP2", ".jpf", ".JPF", ".bmp", ".BMP", ".ps", ".PS", ".eps", ".EPS", ".mps", ".MPS",
			},
			probe: Command{Name: "xelatex", Args: []string{"--version"}},
		},
	} {
		if err := registry.Register(name, driver); err != nil {
			panic(err)
		}
	}
	return registry
}
