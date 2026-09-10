#!/usr/bin/env python3
"""Create TeX Gyre compatibility copies under the GUST Font License."""

from pathlib import Path
import sys

from fontTools.ttLib import TTFont


FONTS = {
    "texgyretermes-regular.otf": ("Times New Roman", "Regular"),
    "texgyretermes-bold.otf": ("Times New Roman", "Bold"),
    "texgyretermes-italic.otf": ("Times New Roman", "Italic"),
    "texgyretermes-bolditalic.otf": ("Times New Roman", "Bold Italic"),
    "texgyreheros-regular.otf": ("Arial", "Regular"),
    "texgyreheros-bold.otf": ("Arial", "Bold"),
    "texgyreheros-italic.otf": ("Arial", "Italic"),
    "texgyreheros-bolditalic.otf": ("Arial", "Bold Italic"),
    "texgyrecursor-regular.otf": ("Courier New", "Regular"),
    "texgyrecursor-bold.otf": ("Courier New", "Bold"),
    "texgyrecursor-italic.otf": ("Courier New", "Italic"),
    "texgyrecursor-bolditalic.otf": ("Courier New", "Bold Italic"),
}


def set_name(font: TTFont, name_id: int, value: str) -> None:
    names = font["name"]
    for record in names.names:
        if record.nameID != name_id:
            continue
        record.string = value.encode(record.getEncoding(), errors="replace")
    names.setName(value, name_id, 3, 1, 0x409)
    names.setName(value, name_id, 1, 0, 0)


def main(directory: Path) -> None:
    for source_name, (family, style) in FONTS.items():
        source = directory / source_name
        if not source.is_file():
            raise FileNotFoundError(source)
        font = TTFont(source)
        postscript = f"Latexmk-{family.replace(' ', '')}-{style.replace(' ', '')}"
        set_name(font, 1, family)
        set_name(font, 2, style)
        set_name(font, 3, f"Latexmk {family} {style}")
        set_name(font, 4, f"{family} {style}".rstrip())
        set_name(font, 6, postscript)
        set_name(font, 16, family)
        set_name(font, 17, style)
        font.save(directory / f"{postscript}.otf")


if __name__ == "__main__":
    main(Path(sys.argv[1]))
