#!/bin/sh
set -eu
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
cd "$work"
latexmk -v
biber --version
for engine in $(printf '%s' "$LATEXMK_ENGINES" | tr ',' ' '); do
  "$engine" --version
  printf '\\documentclass{article}\n\\begin{document}Runtime smoke test\\end{document}\n' > engine.tex
  "$engine" -interaction=nonstopmode -halt-on-error engine.tex > engine.log.out
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
