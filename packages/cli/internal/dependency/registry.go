package dependency

import "sort"

// Patterns describe syntax and dependency policy independently of command names.
// Every TeX spelling is registered directly; dispatch never rewrites a command.
type argumentKind uint8

const (
	bracedArgument argumentKind = iota
	inputArgument
	controlArgument
)

type syntaxKind uint8

const (
	argumentSyntax syntaxKind = iota
	plotSyntax
	environmentSyntax
	scopeOpenSyntax
	scopeCloseSyntax
	conditionalSyntax
)

type missingPolicy uint8

type conditionalAction uint8

const (
	conditionalUnknown conditionalAction = iota
	conditionalTrue
	conditionalFalse
	conditionalAlternative
	conditionalEnd
)

const (
	requiredFile missingPolicy = iota
	systemFile                 // A bare name may be provided by the TeX distribution.
	optionalFile
)

type referenceRule struct {
	extensions   []string
	recursive    bool
	missing      missingPolicy
	graphics     bool
	suffix       string
	rootRelative bool
}

type commandPattern struct {
	names           []string
	syntax          syntaxKind
	arguments       []argumentKind
	trailingOptions bool
	reference       int
	splitComma      bool
	rule            referenceRule
	handle          func(*discoverer, string, invocation)
	inheritOptions  bool
	relativeImport  bool
	loadKind        string
	globalOptions   bool
	changesState    bool
	condition       conditionalAction
	opensScope      bool
}

var commandRegistry map[string]commandPattern
var packageRegistry map[string]func(*discoverer, string, invocation, []option)
var assetRegistry map[string]func(*discoverer, string)

func init() {
	packageRegistry = map[string]func(*discoverer, string, invocation, []option){
		"biblatex": biblatexOptions,
		"svg":      svgOptions,
	}
	assetRegistry = map[string]func(*discoverer, string){".svg": svgAssets}
	commandRegistry = buildRegistry([]commandPattern{
		{
			names:     []string{"input"},
			arguments: []argumentKind{inputArgument},
			rule:      referenceRule{extensions: []string{"", ".tex"}, recursive: true},
		},
		{
			names:     []string{"include", "subfile", "loadglsentries"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{extensions: []string{"", ".tex"}, recursive: true},
		},
		{
			names:     []string{"includegraphics"},
			arguments: []argumentKind{bracedArgument},
			rule: referenceRule{
				extensions: []string{".pdf", ".png", ".jpg", ".jpeg", ".eps", ".mps"},
				graphics:   true,
			},
		},
		{
			names:     []string{"includepdf"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{extensions: []string{".pdf"}, graphics: true},
		},
		{
			names:     []string{"includesvg"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{extensions: []string{".svg"}, graphics: true},
			handle:    svgInput,
		},
		{names: []string{"svgsetup"}, arguments: []argumentKind{bracedArgument}, changesState: true, handle: svgSetup},
		{names: []string{"svgpath"}, arguments: []argumentKind{bracedArgument}, changesState: true, handle: svgPath},
		{
			names:      []string{"bibliography"},
			arguments:  []argumentKind{bracedArgument},
			splitComma: true,
			rule:       referenceRule{extensions: []string{".bib"}},
		},
		{
			names:     []string{"addbibresource"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{extensions: []string{".bib"}},
		},
		{
			names:     []string{"bibliographystyle"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{extensions: []string{".bst"}, missing: systemFile},
		},
		{
			names:         []string{"documentclass"},
			globalOptions: true,
			arguments:     []argumentKind{bracedArgument},
			loadKind:      "class",
			handle:        loadModule,
		},
		{
			names: []string{
				"LoadClass",
			},
			arguments: []argumentKind{bracedArgument},
			loadKind:  "class",
			handle:    loadModule,
		},
		{
			names:          []string{"LoadClassWithOptions"},
			arguments:      []argumentKind{bracedArgument},
			loadKind:       "class",
			inheritOptions: true,
			handle:         loadModule,
		},
		{
			names:     []string{"usepackage", "RequirePackage"},
			arguments: []argumentKind{bracedArgument},
			loadKind:  "package",
			handle:    loadModule,
		},
		{
			names:          []string{"RequirePackageWithOptions"},
			arguments:      []argumentKind{bracedArgument},
			loadKind:       "package",
			inheritOptions: true,
			handle:         loadModule,
		},
		{
			names:     []string{"PassOptionsToPackage"},
			arguments: []argumentKind{bracedArgument, bracedArgument},
			loadKind:  "package",
			handle:    forwardOptions,
		},
		{
			names:     []string{"PassOptionsToClass"},
			arguments: []argumentKind{bracedArgument, bracedArgument},
			loadKind:  "class",
			handle:    forwardOptions,
		},
		{
			names:     []string{"RequireBibliographyStyle"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{suffix: ".bbx", recursive: true, missing: systemFile},
		},
		{
			names:     []string{"RequireCitationStyle"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{suffix: ".cbx", recursive: true, missing: systemFile},
		},
		{
			names:     []string{"DeclareLanguageMapping"},
			arguments: []argumentKind{bracedArgument, bracedArgument},
			reference: 1,
			rule:      referenceRule{suffix: ".lbx", recursive: true, missing: systemFile},
		},
		{
			names:     []string{"InheritBibliographyExtras", "InheritBibliographyStrings"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{suffix: ".lbx", recursive: true, missing: systemFile},
		},
		{
			names:     []string{"InputIfFileExists"},
			arguments: []argumentKind{bracedArgument, bracedArgument, bracedArgument},
			rule:      referenceRule{extensions: []string{"", ".tex"}, recursive: true, missing: optionalFile},
			handle:    conditionalInput,
		},
		{
			names:     []string{"import", "inputfrom", "includefrom"},
			arguments: []argumentKind{bracedArgument, bracedArgument},
			handle:    importFile,
		},
		{
			names:          []string{"subimport", "subinputfrom", "subincludefrom"},
			arguments:      []argumentKind{bracedArgument, bracedArgument},
			relativeImport: true,
			handle:         importFile,
		},
		{
			names:     []string{"lstinputlisting", "verbatiminput", "VerbatimInput"},
			arguments: []argumentKind{bracedArgument},
			rule:      referenceRule{extensions: []string{""}},
		},
		{
			names:     []string{"inputminted", "DTLloaddb"},
			arguments: []argumentKind{bracedArgument, bracedArgument},
			reference: 1,
			rule:      referenceRule{extensions: []string{""}},
		},
		{names: []string{"pgfplotstableread"}, arguments: []argumentKind{bracedArgument}, handle: tableInput},
		{names: []string{"addplot", "addplot3"}, syntax: plotSyntax, handle: tableInput},
		{
			names:        []string{"graphicspath"},
			arguments:    []argumentKind{bracedArgument},
			changesState: true,
			handle:       graphicsPath,
		},
		{
			names:        []string{"DeclareGraphicsExtensions"},
			arguments:    []argumentKind{bracedArgument},
			handle:       graphicsExtensions,
			changesState: true,
		},
		{
			names:           []string{"setmainfont", "setsansfont", "setmonofont", "fontspec"},
			arguments:       []argumentKind{bracedArgument},
			trailingOptions: true,
			handle:          fontInput,
		},
		{
			names:           []string{"newfontfamily", "newfontface"},
			arguments:       []argumentKind{controlArgument, bracedArgument},
			trailingOptions: true,
			reference:       1,
			handle:          fontInput,
		},
		{
			names:        []string{"defaultfontfeatures"},
			arguments:    []argumentKind{bracedArgument},
			changesState: true,
			handle:       fontDefaults,
		},
		{
			names:      []string{"begin"},
			syntax:     environmentSyntax,
			arguments:  []argumentKind{bracedArgument},
			opensScope: true,
		},
		{names: []string{"end"}, syntax: environmentSyntax, arguments: []argumentKind{bracedArgument}},
		{names: []string{"begingroup", "bgroup"}, syntax: scopeOpenSyntax},
		{names: []string{"endgroup", "egroup"}, syntax: scopeCloseSyntax},
		{names: []string{"iftrue"}, syntax: conditionalSyntax, condition: conditionalTrue},
		{names: []string{"iffalse"}, syntax: conditionalSyntax, condition: conditionalFalse},
		{names: []string{"else", "or"}, syntax: conditionalSyntax, condition: conditionalAlternative},
		{names: []string{"fi"}, syntax: conditionalSyntax, condition: conditionalEnd},
		{
			names: []string{
				"if",
				"ifcat",
				"ifnum",
				"ifdim",
				"ifodd",
				"ifvmode",
				"ifhmode",
				"ifmmode",
				"ifinner",
				"ifvoid",
				"ifhbox",
				"ifvbox",
				"ifx",
				"ifeof",
				"ifcase",
				"ifdefined",
				"ifcsname",
				"iffontchar",
			},
			syntax: conditionalSyntax,
		},
		{names: []string{"directlua", "luaexec"}, arguments: []argumentKind{bracedArgument}, handle: dynamicCode},
	})
}

func buildRegistry(patterns []commandPattern) map[string]commandPattern {
	registry := make(map[string]commandPattern)
	for _, pattern := range patterns {
		for _, name := range pattern.names {
			if _, exists := registry[name]; exists {
				panic("duplicate dependency command: " + name)
			}
			registry[name] = pattern
		}
	}
	return registry
}

type environmentPattern struct{ generated bool }

var environmentRegistry = map[string]environmentPattern{
	"verbatim": {}, "verbatim*": {}, "Verbatim": {}, "lstlisting": {}, "minted": {},
	"filecontents": {generated: true}, "filecontents*": {generated: true},
}

var biblatexStyleRegistry = map[string][]string{
	"style": {".bbx", ".cbx"}, "bibstyle": {".bbx"}, "citestyle": {".cbx"},
}

var moduleExtensions = map[string]string{"class": ".cls", "package": ".sty"}

var fontFaceRegistry = []struct{ font, features string }{
	{"UprightFont", "UprightFeatures"}, {"BoldFont", "BoldFeatures"},
	{"ItalicFont", "ItalicFeatures"}, {"BoldItalicFont", "BoldItalicFeatures"},
	{"SlantedFont", "SlantedFeatures"}, {"BoldSlantedFont", "BoldSlantedFeatures"},
	{"SwashFont", "SwashFeatures"}, {"BoldSwashFont", "BoldSwashFeatures"},
	{"SmallCapsFont", "SmallCapsFeatures"},
}

var fontExtensions = []string{".otf", ".ttf", ".ttc", ".pfb"}

// Option-driven inputs and companion formats supplement command suffixes for
// bounded server requests. The resolver uses this same registry inventory.
var optionInputExtensions = []string{".dbx", ".fontspec", ".dat", ".csv", ".pdf_tex"}

func registeredExtensions() []string {
	unique := make(map[string]bool)
	for _, pattern := range commandRegistry {
		for _, extension := range pattern.rule.extensions {
			unique[extension] = true
		}
		unique[pattern.rule.suffix] = true
	}
	for _, extension := range moduleExtensions {
		unique[extension] = true
	}
	for _, extension := range fontExtensions {
		unique[extension] = true
	}
	for _, extension := range optionInputExtensions {
		unique[extension] = true
	}
	delete(unique, "")
	result := make([]string, 0, len(unique))
	for extension := range unique {
		result = append(result, extension)
	}
	sort.Strings(result)
	return result
}

var svgDirectoryRegistry = map[string]struct {
	sourceRelative bool
	subdirectory   string
}{
	"svgdir": {sourceRelative: true}, "svgsubdir": {sourceRelative: true, subdirectory: "svg-inkscape"},
	"basedir": {}, "basesubdir": {subdirectory: "svg-inkscape"},
}
