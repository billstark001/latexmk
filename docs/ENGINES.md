# Compiler engines

`engine` in configuration, targets and protocol requests is a string, not a
closed enum. Current built-ins are `xelatex`, `lualatex` and `pdflatex`; the CLI
still defaults to `xelatex`. Names select behavior registered in trusted Go
application code. They do not become executable names or arbitrary command
arguments. An unregistered name produces an explicit error without a default
engine fallback.

The registry lives in `packages/shared/engine`, so CLI dependency discovery,
server request/configuration validation, compilation arguments and toolchain
probes use the same definitions. The server enables a subset through
`LATEXMK_ENGINES`, advertised in `capabilities.engines`. A registered but disabled
engine cannot compile on that server.

## Driver interface and registration

`engine.Driver` supplies three engine-specific behaviors:

- `LatexmkArgs() []string`: trusted engine options appended after `-norc`;
- `GraphicsExtensions() []string`: ordered suffixes for extensionless graphics;
- `VersionProbe() engine.Command`: executable and arguments for version detection.

Common validated interaction, recorder, SyncTeX, shell-escape, job-name and
other build settings remain in the server's compiler adapter. LuaLaTeX's
built-in driver retains `--safer --nosocket`. Registry entries cannot replace
existing names. Implementations must be immutable, safe for concurrent use and
return caller-owned slices. Register extensions during startup, before requests
are served. `NewRegistry` creates an independent registry; `engine.Default` is
the shared application registry. `Names()` returns a sorted copy.

For example, an application can add a trusted pdfTeX variant without adding a
name-specific branch to the CLI or server:

```go
package customengine

import "github.com/billstark001/latexmk/packages/shared/engine"

type researchPDF struct{ base engine.Driver }

func (researchPDF) LatexmkArgs() []string {
    return []string{"-pdf", "-pdflatex=research-pdftex %O %S"}
}

func (d researchPDF) GraphicsExtensions() []string {
    return d.base.GraphicsExtensions()
}

func (researchPDF) VersionProbe() engine.Command {
    return engine.Command{Name: "research-pdftex", Args: []string{"--version"}}
}

func init() {
    base, err := engine.Default.Lookup("pdflatex")
    if err != nil {
        panic(err)
    }
    if err := engine.Default.Register("lab/research-pdf", researchPDF{base: base}); err != nil {
        panic(err)
    }
}
```

Link that registration package into the CLI, controller and compile-worker
entry points that need it, and install the trusted executable in the runtime.
Enable `LATEXMK_ENGINES=lab/research-pdf` on the server and select
`--engine lab/research-pdf` in the CLI. Opaque names may include punctuation;
registry lookup is exact and case-sensitive. Use matching driver definitions
and toolchains in the controller/worker images. This extension mechanism does
not add JSON driver definitions, dynamic plugin loading or a profile DSL.

`auto` selection needs the driver's graphics behavior locally. Reviewed
`manifest`/`all` selection can target an engine registered only on the server,
since those modes do not perform static discovery. The server always requires
its registered and enabled implementation. Toolchain metadata is keyed by
engine name, even when its probe uses an executable with a different name.

Existing TeX-style engine flags and `xelatex`/`lualatex`/`pdflatex` executable
names remain public CLI behavior. Explicit engine flags take precedence over a
target's engine for compilation, file previews and packing. Compilation invoked
through an engine executable alias also takes precedence over target defaults.

## Validation scope

The built-ins preserve their original latexmk invocation and default graphics
rules. Real pdfLaTeX and XeLaTeX tests cover nested entries, conflicting image
extensions, BibTeX/index builds, portable cache reuse, SyncTeX, custom job names,
error propagation and verified source packages. See
[local integration checks](DEVELOPMENT.md#local-integration-checks).
This does not change the packages installed in the slim image.
