# Runtime fonts

Both runtime profiles install the baseline below. These are available font
families, not a global override of a document's font choices. Select them with
`fontspec` in XeLaTeX or LuaLaTeX; use `unicode-math` for OpenType mathematics.
Traditional pdfLaTeX font packages use TeX's separate font lookup.

| Use                        | Baseline families                                                                                            | Notes                                                                                                                                             |
| -------------------------- | ------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Code / monospace           | DejaVu Sans Mono, Liberation Mono, Inconsolata, TeX Gyre Cursor                                              | Inconsolata's Debian package supplies regular only; the other listed families have bold, italic/oblique and bold italic/oblique.                  |
| Serif body                 | TeX Gyre Termes, Liberation Serif, Caladea                                                                   | Caladea is a Cambria body alternative, not Cambria Math.                                                                                          |
| Sans serif                 | TeX Gyre Heros, Liberation Sans, Carlito                                                                     | Carlito is a Calibri alternative.                                                                                                                 |
| Chinese                    | Noto Serif CJK SC, Noto Sans CJK SC                                                                          | TC, HK, JP and KR variants also come from `fonts-noto-cjk`; use the appropriate regional family. These are not SimSun, SimHei or Microsoft YaHei. |
| OpenType math              | TeX Gyre Termes Math, TeX Gyre Pagella Math, TeX Gyre Bonum Math, TeX Gyre Schola Math, TeX Gyre DejaVu Math | Choose explicitly with `\setmathfont`; text fonts alone do not provide a math font.                                                               |
| Legacy compatibility names | Times New Roman, Arial, Courier New                                                                          | Renamed TeX Gyre Termes, Heros and Cursor copies respectively, **not Microsoft originals**. All four text styles are generated.                   |

Slim (`xelatex-cjk-slim`) supports XeLaTeX through the service. Full
(`texlive-full`) supports XeLaTeX, LuaLaTeX and pdfLaTeX and contains the complete
pinned TeX distribution, including additional fonts. Incidental upstream fonts
can differ between profiles; the per-image inventory is authoritative. Full does
not add licensed Microsoft or Apple originals. This change requires rebuilding
the runtime and adopting its new digest in the application deployment.

## Verifiable inventory

Every runtime build creates these files in `/usr/local/lib/latexmk/`:

- `fonts.tsv`: one row per Fontconfig face, with absolute file path, collection
  index, family, style, PostScript name and font version.
- `fonts.sha256`: SHA-256 hashes of all font files discoverable by Fontconfig.
- `debian-packages.txt`, `texlive.tlpdb`, `texlive-repository.txt`: package versions
  and the TeX repository used for that build.

Extract and verify the inventory from the exact runtime you deploy:

```sh
image=latexmk-runtime:slim-local # Or registry/name@sha256:YOUR_DIGEST
mkdir -p font-inventory
docker run --rm --entrypoint cat "$image" /usr/local/lib/latexmk/fonts.tsv > font-inventory/fonts.tsv
docker run --rm --entrypoint cat "$image" /usr/local/lib/latexmk/fonts.sha256 > font-inventory/fonts.sha256
docker run --rm --entrypoint sh "$image" -c 'sha256sum -c /usr/local/lib/latexmk/fonts.sha256'
docker run --rm --entrypoint fc-list "$image" ':family=DejaVu Sans Mono' family style file
```

`runtime-image` publishes these inventory files as a workflow artifact per
profile, alongside the image reference. Fontconfig's inventory covers registered
system/OpenType/TrueType faces, not every TeX bitmap/Type 1 font or an exhaustive
Unicode glyph-coverage report. `fc-match` may silently return a fallback; inspect
its returned family and file rather than treating success as proof of presence.
For the compatibility families, `Latexmk-*.otf` identifies the generated copies.

To capture user-mounted fonts, rerun
`sh /usr/local/lib/latexmk/font-inventory.sh /tmp/font-inventory` inside that
container. The original build inventory intentionally describes the image only.

## Academic templates and substitutions

Prefer explicit portable names, for example:

```tex
\usepackage{fontspec}
\usepackage{unicode-math}
\setmainfont{TeX Gyre Termes}
\setsansfont{TeX Gyre Heros}
\setmonofont{DejaVu Sans Mono}
\setmathfont{TeX Gyre Termes Math}
```

In XeLaTeX Chinese documents, additionally use `xeCJK` and
`\setCJKmainfont{Noto Serif CJK SC}`. LuaLaTeX Chinese documents need a
LuaLaTeX-compatible CJK layout package (for example `luatexja-fontspec`), not
`xeCJK`.

For Consolas, choose Inconsolata or DejaVu Sans Mono explicitly. For Menlo,
DejaVu Sans Mono is a practical alternative. Neither is an exact reproduction
or a guarantee of identical metrics. No Consolas/Menlo aliases are installed.
Changing fonts can alter code-column width, line and page breaks, glyph shape,
x-height, kerning and math spacing; compare the final PDF with the template's
reference. The same caveat applies to compatibility names, Carlito and Caladea.
Publisher requirements for original Times New Roman or Cambria Math are not
satisfied merely by a similarly named or styled substitute.

Full also registers legacy Type 1 fonts: Caladea's Type 1 bold face can shadow
its TrueType face during name-based lookup in XeLaTeX. For reliable four-style
Caladea selection, use filenames (as in the smoke fixture):

```tex
\setmainfont{Caladea-Regular.ttf}[
  Path=/usr/share/fonts/truetype/crosextra/,
  BoldFont=Caladea-Bold.ttf,
  ItalicFont=Caladea-Italic.ttf,
  BoldItalicFont=Caladea-BoldItalic.ttf
]
```

## Using original Consolas, Menlo or institution fonts

The project does not distribute these originals. Supply files only under a
license permitting use on your Linux compilation server and, if applicable,
image distribution. A local Windows/macOS installation or PDF embedding
permission alone is not a server redistribution grant. See
[Microsoft's font redistribution FAQ](https://learn.microsoft.com/en-us/typography/fonts/font-faq)
and the [Apple software license applicable to your purchase](https://www.apple.com/legal/sla/).
If your license does not permit this deployment, select an alternative above.

For a private derived **runtime** image, place authorized font files in
`licensed-fonts/` beside this Dockerfile:

```dockerfile
FROM latexmk-runtime:slim-local
USER root
COPY licensed-fonts/ /usr/local/share/fonts/custom/
RUN chmod -R a+rX /usr/local/share/fonts/custom \
    && fc-cache -f \
    && sh /usr/local/lib/latexmk/font-inventory.sh /usr/local/lib/latexmk
USER 10001:10001
```

Build with `docker build -t latexmk-runtime:private-fonts .`, then pass
`--runtime-image latexmk-runtime:private-fonts` to the application bundler.
Keep the resulting image private unless your license permits redistribution.
LuaLaTeX refreshes its font database when resolving newly installed names; a
writable HOME/cache or `/tmp` is needed, as in the generated service setup.

Alternatively add a read-only bind mount to the generated Compose service:

```yaml
volumes:
  - ./licensed-fonts:/usr/local/share/fonts/custom:ro
```

Ensure directories are searchable and files readable by UID 10001. Restart the
container after adding fonts; do not mount over the whole fonts directory.
Validate the actual selected family with `fc-list` and a compilation, since a
read-only filesystem cannot save system Fontconfig caches.

For project-local fonts, upload the font files with your project (include them
in the upload manifest or use `--include-file 'fonts/**'`; confirm the selection
with `--dry-run`) and select exact filenames to avoid name collisions:

```tex
\setmonofont{consola.ttf}[
  Path=fonts/,
  BoldFont=consolab.ttf,
  ItalicFont=consolai.ttf,
  BoldItalicFont=consolaz.ttf
]
```

Use filenames matching your licensed files. For Menlo supplied as a `.ttc`, use
`FontIndex` to select the desired face; inspect collection indices with
`fc-scan --format '%{index}: %{family} %{style}\n' fonts/Menlo.ttc` instead of
assuming a fixed order. Explicit filenames also avoid the built-in compatibility
copies when installing genuine Times New Roman/Arial/Courier New.

## Loading tests and licenses

The build runs `runtime/font-smoke.tex` through XeLaTeX in both profiles and
LuaLaTeX in full, as UID 10001. It exercises serif, sans, monospace, four-style
text families, accented Latin, Chinese glyph loading and OpenType mathematics.
Missing glyphs, missing fonts and unavailable shapes fail the build. The existing
XeLaTeX Chinese-layout/Biber test remains separate. Rerun all smoke tests with:

```sh
docker run --rm --entrypoint sh "$image" /usr/local/lib/latexmk/smoke.sh
```

This checks font loading and representative glyphs, not every academic template
or visual equivalence with commercial fonts.

System font packages are installed from Debian with their copyright/license
files under `/usr/share/doc/<package>/copyright`; see the
[Debian font package catalog](https://packages.debian.org/stable/fonts/).
TeX Gyre and the renamed compatibility copies use the
[GUST Font License](https://ctan.org/texarchive/fonts/tex-gyre?lang=en), not OFL.
The conversion preserves existing font copyright and license metadata and
changes the family/PostScript names. TeX Live retains its package documentation.
