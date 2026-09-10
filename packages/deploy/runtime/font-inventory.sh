#!/bin/sh
# Capture actual Fontconfig faces, plus hashes of every discoverable font file.
set -eu
export LC_ALL=C
out=${1:-.}
mkdir -p "$out"
{
  printf 'file\tindex\tfamily\tstyle\tpostscript_name\tfont_version\n'
  fc-list --format '%{file}\t%{index}\t%{family}\t%{style}\t%{postscriptname}\t%{fontversion}\n' | sort -u
} > "$out/fonts.tsv"
fc-list --format '%{file}\n' | sort -u > "$out/font-files.txt"
while IFS= read -r font; do
  sha256sum "$font"
done < "$out/font-files.txt" > "$out/fonts.sha256"
rm "$out/font-files.txt"
