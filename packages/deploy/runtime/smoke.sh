#!/bin/sh
set -eu
PATH=/usr/local/bin:/usr/bin:/bin
export PATH
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
cd "$work"
latexmk -v
biber --version
tlmgr --version
kpsewhich --version
fc-scan --version
xz --version
cp /usr/local/share/fonts/latexmk/Latexmk-TimesNewRoman-Regular.otf resource-font
LC_ALL=C HOME="$work" fc-scan --format '%{family}\t%{style}\n' resource-font > resource-font.tsv
awk -F '\t' 'NF >= 2 && length($1) > 0 { found=1 } END { exit !found }' resource-font.tsv

# Exercise managed package preparation without downloading anything. TeX Live's
# own downloader may bundle an xz decoder even when the server's required system
# `xz` is missing, so verify the exact external decompression operation too.
mkdir -p resource-package/tlpkg/tlpobj resource-package/tex/latex/latexmk-resource-smoke
cat > resource-package/tlpkg/tlpobj/latexmk-resource-smoke.tlpobj <<'TLP'
name latexmk-resource-smoke
category Package
revision 1
relocated 1
shortdesc Offline managed resource smoke fixture
runfiles size=1
 RELOC/tex/latex/latexmk-resource-smoke/latexmk-resource-smoke.sty
TLP
cat > resource-package/tex/latex/latexmk-resource-smoke/latexmk-resource-smoke.sty <<'STYLE'
\ProvidesPackage{latexmk-resource-smoke}
\newcommand{\latexmkresourcesmoke}{Managed resource smoke test}
STYLE
tar -C resource-package -cf resource.tar tlpkg/tlpobj/latexmk-resource-smoke.tlpobj tex/latex/latexmk-resource-smoke/latexmk-resource-smoke.sty
xz --compress --stdout -- resource.tar > latexmk-resource-smoke.r1.tar.xz
xz --decompress --stdout -- latexmk-resource-smoke.r1.tar.xz > resource-restored.tar
resource_hash=$(sha256sum resource.tar)
resource_hash=${resource_hash%% *}
printf '%s  resource-restored.tar\n' "$resource_hash" | sha256sum --check
resource_tree="$work/resource-tree"
prepare_resource() {
  tlmgr --usermode --usertree "$resource_tree" init-usertree || return
  tlmgr --usermode --usertree "$resource_tree" option install_docfiles 0 || return
  tlmgr --usermode --usertree "$resource_tree" option install_srcfiles 0 || return
  tlmgr --usermode --usertree "$resource_tree" install --no-depends-at-all --file "$work/latexmk-resource-smoke.r1.tar.xz" || return
  resource_style=$(TEXMFHOME="$resource_tree" kpsewhich --must-exist latexmk-resource-smoke.sty) || return
  test "$resource_style" = "$resource_tree/tex/latex/latexmk-resource-smoke/latexmk-resource-smoke.sty"
}
prepare_resource > resource-prepare.log 2>&1 || { cat resource-prepare.log; exit 1; }
cat > resource.tex <<'TEX'
\documentclass{article}
\usepackage{latexmk-resource-smoke}
\begin{document}\latexmkresourcesmoke\end{document}
TEX
TEXMFHOME="$resource_tree" xelatex -interaction=nonstopmode -halt-on-error resource.tex > resource-compile.log 2>&1 || {
  cat resource-compile.log; exit 1;
}
test -s resource.pdf
for engine in $(printf '%s' "$LATEXMK_ENGINES" | tr ',' ' '); do
  "$engine" --version
  printf '\\documentclass{article}\n\\begin{document}Runtime smoke test\\end{document}\n' > engine.tex
  "$engine" -interaction=nonstopmode -halt-on-error engine.tex > engine.log.out
  case "$engine" in
    xelatex|lualatex)
      cp /usr/local/lib/latexmk/font-smoke.tex fonts.tex
      "$engine" -interaction=nonstopmode -halt-on-error fonts.tex > fonts.log.out 2>&1 || {
        cat fonts.log.out; exit 1;
      }
      test -s fonts.pdf
      if grep -E 'Missing character:|Some font shapes were not available|Font shape .* undefined' fonts.log; then
        cat fonts.log; exit 1
      fi
      ;;
  esac
done
cat > main.tex <<'TEX'
\documentclass{article}
\usepackage{fontspec}
\setmainfont{Times New Roman}
\usepackage{xeCJK}
\usepackage[backend=biber]{biblatex}
\addbibresource{refs.bib}
\begin{document}
中文测试 English \cite{sample}.
\printbibliography
\end{document}
TEX
printf '@book{sample,author={Example, Alice},title={Runtime test},year={2026}}\n' > refs.bib
latexmk -norc -xelatex -interaction=nonstopmode -halt-on-error main.tex > compile.log 2>&1 || {
  cat compile.log; exit 1;
}
test -s main.pdf
test -s main.bbl
