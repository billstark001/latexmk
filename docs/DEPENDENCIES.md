# Client dependency selection

The default client upload mode is `auto`. It builds the normal policy-filtered
manifest first, then selects literal dependencies starting from the entry TeX
file. The scanner never reads a path that Git-ignore, the denylist, symlink
checks, or the project-root boundary removed from that manifest.

## Supported references

The scanner recognizes literal arguments for these registered patterns:

| Area                  | Commands and behavior                                                                                                                                                                                                                                   |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| TeX inputs            | `input`, `include`, `subfile`, `loadglsentries`; recursively scan local inputs. `input chapter.tex` also accepts a literal unbraced filename ending at whitespace.                                                                                      |
| Class/package loaders | `documentclass`, `LoadClass`, `LoadClassWithOptions`, `usepackage`, `RequirePackage`, `RequirePackageWithOptions`; follow local `.cls` and `.sty` files.                                                                                                |
| Forwarded options     | `PassOptionsToPackage`, `PassOptionsToClass`; retain options until loading. `WithOptions` loaders inherit the enclosing module's literal options.                                                                                                       |
| Bibliography          | `bibliography`, `addbibresource`, `bibliographystyle`; biblatex `style`, `bibstyle`, `citestyle`, `datamodel`; `RequireBibliographyStyle`, `RequireCitationStyle`, `DeclareLanguageMapping`, `InheritBibliographyExtras`, `InheritBibliographyStrings`. |
| Conditional inputs    | `InputIfFileExists`; select an existing allowed input and its dependencies, without an error for an absent optional input.                                                                                                                              |
| Imported projects     | `import`, `subimport`, `inputfrom`, `subinputfrom`, `includefrom`, `subincludefrom`; nested inputs use the active import context.                                                                                                                       |
| Graphics              | `includegraphics`, `graphicspath`, `DeclareGraphicsExtensions`, `includepdf`, `includesvg`; declared extension order is searched before directory order.                                                                                                |
| Fonts                 | `setmainfont`, `setsansfont`, `setmonofont`, `newfontfamily`, `newfontface`, `fontspec`, `defaultfontfeatures`; explicit faces, `Path`, `Extension`, `*` substitution, local `.fontspec` settings.                                                      |
| Data/code             | `lstinputlisting`, `verbatiminput`, `VerbatimInput`, `inputminted`, `DTLloaddb`, `pgfplotstableread`, and `table`/`file` forms of `addplot`/`addplot3`.                                                                                                 |
| SVG assets            | Local XML `href`/`xlink:href` on `image`, `use`, `feImage`; existing exports and PDF+LaTeX wrappers. `svgsetup` and `svgpath` configure SVG selection.                                                                                                  |

Options may span lines and contain braced values, nested commas, protected
brackets, escaped delimiters, comments, CRLF and trailing commas. Comments and
common verbatim environments are excluded. Selected files have reasons in
`latexmk files` output.

Biblatex appends `.dbx` to an explicit `datamodel` name. The last value wins;
an empty value permits implicit models again. Without an explicit model, select
an existing `style.dbx`, or separate `citestyle.dbx` and `bibstyle.dbx` when no
`style` was given. `.bbx` and `.cbx` options are applied in order; local styles,
models and language mappings are traversed. See the
[biblatex loading implementation](https://github.com/plk/biblatex/blob/dev/tex/latex/biblatex/biblatex.sty).

Bare class, package and bibliography-style names absent from the allowed
manifest are distribution dependencies, including `.bbx`, `.cbx` and `.lbx`.
Absent implicit models are optional. Explicit models, local font filenames and
ordinary required inputs produce diagnostics when unavailable. An ignored bare
local style cannot be distinguished from a system style without reading excluded
files; the compiler remains responsible for that distinction.

`import` directories are project-root-relative; `subimport` extends the active
import directory. Nested inputs search import directories before the project
root. Paths are normalized in that context before enforcing the root boundary.
Ordinary `input` does not become relative to its containing file. Import search
paths are restored on return. Graphics paths, extension lists, font defaults and
SVG settings obey braced groups, `begingroup`/`endgroup` and environments.

Font options work before or after the name, and around the control-sequence
argument of `newfontfamily`. Explicit upright, bold, italic, bold-italic,
slanted, swash and small-caps faces support nested face features. Named system
fonts are not missing project files. System font availability and arbitrary
OpenType feature code are not inferred. See the
[fontspec documentation](https://github.com/latex3/fontspec/blob/main/fontspec-doc-fontsel.tex).

Inline plot tables and whole table macros are data, not filenames. Their literal
`pgfplotstableread` loaders remain discoverable independently. Computed filenames
still require explicit selection.

SVG resource URIs resolve against the SVG's directory; fragments, embedded data
and remote URLs never trigger downloads. Local SVG references have cycle and
depth limits. XML base overrides require explicit selection; CSS and script
loading are outside this subset. `inkscapepath`
supports literal directories and `basedir`, `basesubdir`, `svgdir`, `svgsubdir`;
`inkscapename`, `inkscapeformat` and `inkscapelatex` select export names. Existing
exports are uploaded; absent exports may be generated, unless `inkscape=false`
makes them required inputs. Discovery never enables shell escape or performs
conversion. Checked-in `.pdf_tex` wrappers are scanned in their rendering
context. See the [SVG implementation](https://github.com/mrpiggi/svg/blob/master/source/svg.dtx).

## Fail-closed cases

The client stops before contacting the server when a non-optional recognized
reference:

- contains a macro or another non-literal path;
- is missing from the local project;
- was removed by Git-ignore or the denylist;
- escapes the project root;
- uses a supported command form the parser cannot understand.

Missing, ignored, and denied references intentionally use the same
`unavailable` diagnostic. Dependency selection does not inspect the contents of
files removed by policy.

Inspect the result and diagnostics with:

```sh
latexmk files main.tex
latexmk files --json main.tex
```

After reviewing the full policy-allowed manifest, compatibility mode is:

```sh
latexmk --upload-mode all main.tex
```

`all` does not override `.env`, key, project-config, Git-ignore, symlink, or
other upload-policy exclusions. Use `--no-gitignore` separately only when an
ignored file is an intentional compile input.

## Explicit manifest

Paths and glob patterns can supplement `auto` discovery without uploading every
allowed project file:

```sh
latexmk files --manifest .latexmk-files main.tex
latexmk --include-file generated/table.tex main.tex
```

The manifest format is UTF-8 text with one project-relative path or glob per line.
Blank lines and lines beginning with `#` are ignored. All user-authored manifest
sources, including JSON `includeFiles` and CLI `--include-file`, support the same
glob syntax: `*`, `?`, character classes, recursive `**`, and brace alternatives.
`**` matches zero or more directory levels; `dir/` includes files recursively.
Patterns use `/` separators; backslash escapes a literal glob character.
Quote CLI patterns to prevent the shell expanding them. Results are sorted and
deduplicated. Patterns are expanded only over policy-allowed candidates, not by
walking arbitrary paths. Invalid patterns, absolute paths and `..` components
are rejected. Exact missing paths are always errors; `unmatchedGlob` (or
`--unmatched-glob`) controls unmatched globs with `error`, `warn`, or `ignore`.
The manifest path
must itself stay inside the project root and cannot contain symlink components.
`.latexmk-manifest`, `.latexmk-files`, and the explicitly configured manifest
are denied from upload because they are client policy,
not a TeX input.

Equivalent project configuration is:

```json
{
  "manifestFile": ".latexmk-files",
  "includeFiles": ["generated/table.tex"]
}
```

In `auto`, explicit files are merged with static and recorder dependencies.
Their presence does not prove that a computed reference is covered. For dynamic
inputs, use `manifest` mode with a reviewed list. An explicit file that is missing, Git-ignored,
denied, outside the root, or otherwise absent from the policy-filtered manifest
causes selection to fail.

For a strict user-maintained allowlist:

```sh
latexmk files --upload-mode manifest --manifest .latexmk-files main.tex
latexmk --upload-mode manifest --manifest .latexmk-files main.tex
```

`manifest` uploads only the entry and expanded explicit files. It does not run the
static scanner or read `.fls` history. `resolved: true` in this mode means the
declared list is valid, not that it is a complete TeX dependency closure. A
missing declaration therefore becomes a normal remote compile failure rather
than an automatic wider upload.

If no inline includes or manifest filename are supplied in `manifest` mode,
the CLI tries `.latexmk-manifest`, then `.latexmk-files`. Generated network
manifests always contain exact expanded paths, hashes and sizes; the server
does not expand user glob expressions.

## Ignore sources and previews

Git repositories use Git itself for tracked and nonignored untracked selection.
Tracked files remain candidates even when they match `.gitignore`. Directories
without `.git` still honor root and nested `.gitignore` files. Explicit custom
ignore files use Git-style ordered rules, `!` negation, anchored paths, directory
rules and `**`. An excluded parent directory must be re-included before any of
its children can be selected. Enabled Git filtering and custom exclusions are
independent; a custom negation cannot restore a Git-excluded file or a credential.

The optional `ignoreFiles` list chooses custom sources; `[]` disables them.
The implicit default is an existing `.latexmkignore`, not a required new file.
Custom ignore-file patterns are relative to the project root. See
[configuration](CONFIGURATION.md) for combining ignore and manifest policies.

```sh
latexmk files --explain figures/plot.pdf main.tex
latexmk files --explain .env.latexmk --json main.tex
latexmk files --include-file 'figures/**/*.pdf' main.tex
```

`--explain` reports the candidate upload policy without reading excluded file
contents. The normal preview reports actual selected files and their reasons.
Preview, compilation and watch share selection code. Only selected files are
hashed and charged against the upload byte limit; candidate enumeration keeps
its separate file-count bound.

Watch refreshes the dependency set while waiting, detecting new glob matches,
deleted files, renamed files, and changed policy files. Project/user JSON,
dotenv and CLI settings remain fixed for a watch session; restart to reload them.

## Limits

This is a static scanner, not TeX. It does not expand macros, execute loops or
Lua, evaluate arbitrary conditionals, or interpret custom wrappers. It can
include extra references in unused macro definitions and unknown branches.
Unknown formatting commands are silent; recognized dynamic references and Lua
commands produce diagnostics. `resolved: true` means recognized references
resolved; it is not proof of a complete dependency set.

Literal `iftrue`/`iffalse` branches are evaluated, including nesting. Other
registered primitive conditionals are traversed on both sides. Conditional
changes to module options, search settings or generated contents remain
unresolved and require explicit manifest selection. `InputIfFileExists` uses
the allowed manifest to choose its branch; its true hook runs before the input.
An absent or excluded optional file takes the false branch. Distribution-only
existence and computed filenames cannot be determined.

`filecontents`/`filecontents*` bodies are inert until their generated file is
referenced. Generated files are not uploaded; referenced generated TeX is scanned
for its real inputs. Existing allowed files win unless `overwrite` is given.
Excluded files are never read, even if they share a generated name.

Catcode changes, arbitrary global assignments, dynamic graphics rules, complex
font features and unsupported environments require `includeFiles` with
`uploadMode: "manifest"`. Text parsing is bounded at 8 MiB per file; traversal
has a 20,000-visit budget and a 256-level recursion limit. Exceeding a limit
produces a diagnostic.

## Recorder cache

The server runs `latexmk` with `-recorder`. It parses `.fls` `INPUT` records
after compilation and returns only normalized paths for regular files inside
the disposable project workspace. TeX Live system paths, absolute container
paths, paths outside the workspace, and symlink escapes are not returned.

After a successful compile, the client stores these relative paths in:

```text
.latexmk-cache/dependencies.json
```

The cache is keyed by entry file and engine, written atomically with mode 0600
where supported, and excluded from uploads by default. A cached path is selected
only if it is also present in the current Git-ignore/denylist-filtered manifest.
Changing policy therefore cannot make an old cache restore a denied file.

History is additive and can be stale after edits. It never marks a dynamic
reference as covered merely because a non-entry file was accepted; the same
applies to unrelated explicit files. Missing literals, malformed commands,
dynamic references and out-of-root paths remain unresolved in `auto`. Use a
reviewed manifest for computed inputs, or review all candidates before `all`:

```sh
latexmk files --upload-mode all main.tex
latexmk --upload-mode all main.tex
# A successful compile adds recorder paths, but does not prove dynamic coverage.
latexmk files main.tex
```

Recorder history does not cover every subprocess input. In the Railway fixture,
Biber read `refs.bib`, but that file and the local OpenType fonts were absent
from `.fls` input history. Static bibliography and font selection remain
necessary.

The client never silently falls back to `all`. A corrupt cache blocks `auto`
with an explicit error; reviewed `manifest` and `all` modes remain available
and do not read the cache.

## Bounded missing-file retries

When the server advertises `capabilities.needsFiles`, `auto` mode asks it to
extract conservative missing-file diagnostics from failed TeX output and log
files. A result can contain project-relative `needsFiles` such as
`sections/new.tex`. Absolute paths and traversal are discarded by the server.

The server response is only a request. The client rebuilds its normal
policy-filtered candidate manifest and accepts an exact path only if it still
passes Git-ignore, denylist, project-root, regular-file, size, and symlink
checks. An extensionless request may select one unique common TeX or graphics
extension. Zero or multiple matches are refused. A refused request is shown as
a warning and the original TeX failure remains the result.

Each accepted retry creates a new upload plan, immutable snapshot, and compile
job. A running or finished job is never mutated. The client stops after 3 retry
rounds, 64 newly added files, or 64 MiB of newly added content. It also stops if
the server asks for a file already present. There is no hidden `all` fallback.

This mechanism is intentionally enabled only in `auto`. `manifest` is a strict
user allowlist and never accepts server additions; `all` already includes every
policy-allowed file. Capability negotiation keeps new request and result fields
away from strict older clients.

Missing-file parsing is diagnostic-based, not a general TeX file-discovery
protocol. It helps when recorder history is stale or an unsupported dynamic
reference reaches the remote compiler. It cannot bootstrap a first compile
when local static selection has already failed before network access. Use an
explicit manifest or reviewed `all` mode for that first compile.

## Dependency watcher

`latexmk watch main.tex` performs one compile immediately, then polls only the
selected dependency set. The default interval and debounce are both 500 ms and
can be changed with `--watch-interval` and `--watch-debounce`. Polling is used so
the same implementation works for native paths and Docker bind mounts.

The watch set contains:

- files selected by static discovery, recorder history, and explicit inputs;
- the configured explicit manifest, which is watched but never uploaded;
- `.gitignore` files on relevant paths and the repository-local
  `.git/info/exclude`, plus the effective global `core.excludesFile`; these are
  watched as policy inputs and never uploaded.

Unrelated project files and directories are not polled. Creating a random new
file therefore does not trigger a compile or expand the upload set. When a
selected TeX file changes, the client runs complete dependency selection again,
so a new literal dependency can join only after passing the normal policy. A
successful remote compile can also refresh recorder dependencies, and bounded
`needsFiles` retries remain available in `auto` mode.

Each event is a normal compile submission with a new immutable snapshot. Rapid
edits are coalesced. If an input changes while compilation is in progress, the
watcher schedules another compile instead of assuming the finished result is
current. TeX, selection, and network failures are reported but the process
continues so a later edit can recover it.

Project/user configuration and environment variables are resolved once at
startup. Restart the watcher after changing them. Git's effective global
excludes file is watched, including its default path before the file exists.
