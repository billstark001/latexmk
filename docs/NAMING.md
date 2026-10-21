# Context-aware output names

The CLI can render explicit naming templates for PDF exports and source ZIPs. Existing `targets.*.pdf`, `pack --output`, `outDir`, and `--jobname` remain literal values. Export naming does not change the TeX job name, auxiliary cache key, remote protocol, or realtime bundle identity.

```sh
latexmk compile main.tex \
  --pdf-template 'output/pdf/{entry.stem}-{date}-{git.commit:8}.pdf'

latexmk pack main.tex \
  --output-template 'release/{entry.stem}-{datetime:compact}-{git.commit:12}.zip'

latexmk name render --target paper \
  --template '{target}-{datetime:go:20060102}-{git.commit:8}.pdf' --json
```

## Syntax and values

The grammar is `{variable[:format]}`. The first colon separates the variable from its complete format; additional colons belong to the format. `{{` and `}}` produce literal braces. Expanded values are inserted once and are never interpreted as more template syntax. Variable names are case-sensitive. Nesting and expressions are not supported.

| Variable | Default value | Supported format |
| --- | --- | --- |
| `entry.stem` | Entry basename without its extension | None |
| `project` | Resolved project-root basename | None |
| `target` | Selected target name | None; unavailable for a direct entry without a target |
| `engine` | Final engine key, including target/CLI overrides | None |
| `git.commit` | Complete HEAD commit hash | Positive decimal prefix length, e.g. `8` or `12` |
| `date` | `2006-01-02` | Time presets or `go:LAYOUT` |
| `time` | `15-04-05` | Time presets or `go:LAYOUT` |
| `datetime` | `20060102T150405` | Time presets or `go:LAYOUT` |

Hash prefixes have exactly the requested length, at most the complete hash length. They are display labels and do not promise uniqueness. Git is queried from the resolved project root using Git itself, including worktrees and detached HEADs. A missing Git executable, non-repository root, or repository without a commit is an error only when `git.commit` is referenced. HEAD identifies a commit; uncommitted edits are not represented by that hash.

All three time variables share one captured instant and time zone. An explicit format overrides the variable's default:

| Preset               | Go layout                             |
| -------------------- | ------------------------------------- |
| `date`               | `2006-01-02`                          |
| `time`               | `15-04-05`                            |
| `compact`            | `20060102T150405`                     |
| `iso8601`, `rfc3339` | `2006-01-02T15:04:05Z07:00`           |
| `rfc3339nano`        | `2006-01-02T15:04:05.999999999Z07:00` |

Custom formats require `go:`, for example `{datetime:go:2006-01-02_15-04-05}`. They use [Go's reference-time layouts](https://pkg.go.dev/time#Time.Format). Go preserves unrecognized layout text literally, so `go:YYYY` prints `YYYY`. Unknown preset names are errors. `iso8601` is defined here as the RFC3339 profile, rather than an unspecified choice among ISO 8601 representations.

Use `compact` or hyphens for filename timestamps. RFC3339 includes colons and therefore fails output-name validation. `name render` still previews the unchanged RFC3339 text because it does not write a path.

Unknown/unavailable variables, invalid formats, unbalanced braces and empty explicit format specifiers are errors. Template sources and rendered results are each limited to 64 KiB. Rendering performs no HTML escaping or shell evaluation.

## Output filename policy

Formatting itself preserves the formatted text. PDF/ZIP output adapters then use shared `safefs` validation before authentication, upload, verification builds, or output-directory creation. They reject invalid names with an error; they never replace characters, truncate a name, or silently choose another path. Dry-run applies the same syntax policy as publication. Plain `name render` is a text preview and intentionally does not apply filename restrictions.

Output paths use a portable policy informed by [Windows filename conventions](https://learn.microsoft.com/en-us/windows/win32/fileio/naming-a-file), consistently on all supported hosts:

- Components reject `< > : " \\ | ? *` and Unicode control characters, including NUL, newline and DEL. Names must be valid UTF-8.
- Components cannot end with a dot or space, or use a reserved device basename such as `CON`, `NUL`, `COM1` or `LPT1`, including when followed by an extension. The superscript variants `COM¹`/`LPT¹`, `²` and `³` are reserved too.
- A component is limited to 255 UTF-8 bytes; the complete output path is limited to 4096 bytes. Unicode letters and internal spaces are preserved.
- Separators belong in literal template text. Each expanded value is a filename fragment and rejects both `/` and `\\`. For date directories, write `release/{date:go:2006}/{date:go:01}/{entry.stem}.zip`, rather than putting separators inside a time layout.

PDF paths must additionally remain inside the project root; existing symlink/non-regular destinations are rejected during preflight and checked again during publication. Pack retains its existing ability to select an external destination explicitly. These output rules are separate from `safefs.Clean`'s protocol-path confinement and do not impose portable filename restrictions on unrelated source inputs.

## Configuration and precedence

```json
{
  "outDir": "output/build",
  "naming": {
    "scope": "invocation",
    "timeZone": "Asia/Tokyo"
  },
  "pdfRetention": { "keepLast": 10 },
  "targets": {
    "paper": {
      "entry": "main.tex",
      "pdfTemplate": "output/pdf/{target}-{datetime:compact}-{git.commit:8}.pdf",
      "pdfRetention": { "keepLast": 3 }
    }
  }
}
```

`pdfTemplate` can also be declared at the top level as the default for targets and direct-entry compilation. A target's `pdf` or `pdfTemplate` overrides that default. Declaring both fields on the same target is invalid. `--pdf-template` overrides the configured PDF destination and can also contain an ordinary literal path. PDF exports must remain inside the resolved project root, have a `.pdf` extension, and satisfy the existing symlink and checksum checks. They are written only after successful compilation. Detach cannot use template exports or bounded PDF retention because it does not download results.

`naming.scope` defaults to `invocation`; `naming.timeZone` defaults to `local`. Time zones accept `local`, `UTC`, and IANA names supported by the runtime. User/project JSON merges naming fields individually. `LATEXMK_NAME_SCOPE` and `LATEXMK_NAME_TIMEZONE` override JSON through the normal dotenv/process-environment precedence. `--name-scope invocation|build` and `--name-timezone ZONE` take final precedence.

`pack --output-template` and literal `--output` are mutually exclusive. Both are relative to the working directory, and the rendered result must end in `.zip`. Pack does not use target PDF exports or PDF retention. One pack operation uses one captured naming context, including verification builds. Explicit PDF export flags are rejected by pack.

## Continuous compilation

| Scope | Watch/realtime behavior |
| --- | --- |
| `invocation` (default) | Freeze the naming time and lazily captured Git values for the CLI invocation. Rebuilds and automatic session reconnects reuse the name. Target iteration shares the invocation time; Git values are cached per project root. |
| `build` | Capture a new naming context for each build/realtime revision. A retry of a submitted revision retains its original name. Export uses the completed revision's captured name even if newer input is pending. |

Invocation-scoped Git values describe the HEAD first observed for that invocation. Choose `build` when the filename should reflect later commits. Naming time is captured before building, not when the result finishes downloading. Settings reloads retain invocation state; changing the template or time zone can still change its rendered destination.

Realtime no-op reconciliations do not capture another build context. A pending revision owns a temporary output-name reservation until it is published, rejected, superseded or the session closes. Successful publication reloads managed history, so pruned paths do not accumulate in memory. Selection also reloads completed export history from other CLI invocations.

```sh
latexmk --realtime --target paper --name-scope build --pdf-keep-last 5
```

Refreshing context creates an additional export only when the rendered path changes. A date-only template usually creates one path per day; a commit-only template creates one per commit; fine-grained timestamps can create many. Equal paths are atomically replaced under the existing export rules. Timestamp precision and hash prefixes do not guarantee a unique filename.

## Retention and generated files

Retention is optional and defaults to keeping all distinct paths. Set `pdfRetention.keepLast` to a positive integer globally or on a target, or use `--pdf-keep-last N`. A target retention object overrides the global setting. `--pdf-keep-last all`, an empty target retention object, or `keepLast: null` disables bounded retention. Zero and negative counts are invalid.

The count covers distinct successful export paths, ordered by their most recent successful publication, within the same project, entry, target and engine. Case/Unicode spellings that resolve to the same physical file are counted once. It persists across CLI invocations. It does not count overwritten versions of one path, normal downloaded artifacts, or realtime's internal generations. Realtime continues retaining its current and previous internal bundles separately.

Template PDF exports, bounded literal PDF exports, and template ZIP exports inside the project record paths, sizes and checksums in `.latexmk-cache/exports/`. This creates export history without creating a project ID. Those paths are excluded from later file selection, including `files`, pack, watch and realtime, so exports do not become new source inputs. Existing literal exports without retention retain their previous behavior. Removing the history forgets ownership and exclusions for old exports; use a dedicated ignored export directory if you want exclusions independent of that state.

PDF and template-ZIP publication stage the history manifest before replacing output, then record the successful publication before pruning. An unwritable history prevents output replacement. Cleanup also verifies the newest PDF before removing any older export; a changed latest output leaves previous exports intact. The output and manifest use separate atomic renames, so a process crash or a failure of the final manifest rename can still require recovery; publication does not claim a filesystem transaction across both files.

Cleanup only considers recorded paths. Edited files, symlinks and directories are preserved and leave managed history. Current/retained files and files referenced by another export group are protected by physical identity, including path aliases and hard links. Cleanup never searches by filename glob. It delegates conditional removal to `safefs.RemoveVerified`, which moves a checked candidate to a private sibling and re-verifies that exact file before unlinking. An editor's replacement is restored without overwriting a newer path. If restoration cannot proceed, cleanup reports the private recovery path and preserves the replacement there. Private staging/recovery names are excluded from source selection.

Export publication/history updates use a process lock; a concurrent publisher receives an error and can retry. Each group's history is bounded to 16 MiB. Export history is separate from auxiliary cache cleanup.

## Preview and automation

`name render --template TEMPLATE [entry|--target NAME]` resolves normal configuration, target, entry, engine and naming options. It requires an existing entry but performs no authentication, upload, compilation, output write, or state creation. It renders arbitrary text and does not promise that the result is a valid output path; use PDF/pack dry-run for destination validation.

`--json` returns command `name.render` in the versioned agent envelope, with `data.template` and `data.resolved`. Argument/template failures use `invalid_arguments`. Existing compile/files JSON shapes remain unchanged. Pack dry-run reports its resolved absolute ZIP destination without publishing an archive or history.
