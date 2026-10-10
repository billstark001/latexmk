package dependency

import "fmt"

// Ordered defaults from graphics-def's pdftex.def, luatex.def and xetex.def.
// Keep case-sensitive suffixes: the remote service runs on a case-sensitive FS.
var engineGraphicExtensions = map[string][]string{
	"pdflatex": {
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
	},
	"lualatex": {
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
	},
	"xelatex": {
		".pdf",
		".PDF",
		".ai",
		".AI",
		".png",
		".PNG",
		".jpg",
		".JPG",
		".jpeg",
		".JPEG",
		".jp2",
		".JP2",
		".jpf",
		".JPF",
		".bmp",
		".BMP",
		".ps",
		".PS",
		".eps",
		".EPS",
		".mps",
		".MPS",
	},
}

func defaultGraphicsExtensions(engine string) ([]string, error) {
	extensions, ok := engineGraphicExtensions[engine]
	if !ok {
		return nil, fmt.Errorf("unsupported dependency discovery engine %q", engine)
	}
	return append([]string(nil), extensions...), nil
}
