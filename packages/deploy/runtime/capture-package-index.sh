#!/bin/sh
set -eu

inventory=$1
repository=$2
year=$3
index="$inventory/texlive-package-index.tlpdb"

if [ -f "$index" ]; then
  (cd "$inventory" && sha256sum --check texlive-package-index.sha256)
  exit 0
fi

pending="$index.pending"
canonical="$index.canonical"
trap 'rm -f "$pending" "$canonical"' EXIT HUP INT TERM
tlmgr --repository "$repository" dump-tlpdb --remote > "$pending"
test -s "$pending"
awk -v expected="$repository" '
  $1 == "location-url" {
    actual=$2; sub(/\/+$/, "", actual); sub(/\/+$/, "", expected)
    if (NF != 2 || actual != expected) invalid=1
  }
  END { exit invalid }
' "$pending"
if ! grep -E '^location-url([[:space:]]|$)' "$pending" > /dev/null; then
  printf 'location-url\t%s\n' "$repository" > "$canonical"
  cat "$pending" >> "$canonical"
  mv "$canonical" "$pending"
fi
grep -F -x "depend release/$year" "$pending"
grep -E '^containerchecksum [a-f0-9]{128}$' "$pending" > /dev/null
checksum=$(sha256sum "$pending")
checksum=${checksum%% *}
mv "$pending" "$index"
printf '{"version":1,"path":"texlive-package-index.tlpdb","sha256":"%s","repository":"%s","texliveYear":%s}\n' \
  "$checksum" "$repository" "$year" > "$inventory/texlive-package-index.json"
printf '%s  texlive-package-index.tlpdb\n' "$checksum" > "$inventory/texlive-package-index.sha256"
chmod 0644 "$index" "$inventory/texlive-package-index.json" "$inventory/texlive-package-index.sha256"
trap - EXIT HUP INT TERM
