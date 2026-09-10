#!/bin/sh
set -eu
# The date-pinned repository must match the base TeX Live release.
tlmgr --version | grep -F "version $TEXLIVE_YEAR"
packages=$(sed '/^#/d; /^[[:space:]]*$/d' /usr/local/lib/latexmk/packages.txt)
tlmgr option repository "$TEXLIVE_REPOSITORY"
if [ -n "$packages" ]; then
  # Install a collection and its dependencies from the same snapshot.
  # shellcheck disable=SC2086
  tlmgr install $packages
fi
texbin=$(find /usr/local/texlive -type d -path '*/bin/*-linux' -print -quit)
test -n "$texbin"
find "$texbin" -maxdepth 1 \( -type f -o -type l \) -perm -u+x -exec ln -sf {} /usr/local/bin/ \;
install --directory /usr/local/share/fonts/latexmk
find /usr/local/texlive -path '*/fonts/opentype/public/tex-gyre/*' -type f -name '*.otf' -exec ln -sf {} /usr/local/share/fonts/latexmk/ \;
find /usr/local/texlive -path '*/fonts/opentype/public/xits/*' -type f -name '*.otf' -exec ln -sf {} /usr/local/share/fonts/latexmk/ \;
printf '%s\n' "$TEXLIVE_REPOSITORY" > /usr/local/lib/latexmk/texlive-repository.txt
find /usr/local/texlive -path '*/tlpkg/texlive.tlpdb' -exec cp {} /usr/local/lib/latexmk/texlive.tlpdb \;
dpkg-query -W > /usr/local/lib/latexmk/debian-packages.txt
